package extender

// Tests of the lag.dev measurement API (lag.dev/PLAN.md M1 to M8, T1).
//
// The fixture is one extender's measurement API with fakes on the lifecycle
// side of M10 -- a certificate for a synthetic measurement name, and a report
// sink -- behind a loopback listener whose accept path peeks the ClientHello
// and dispatches it as the extender's does (M1). The clients are what a browser
// is: a Go websocket client that answers pings itself, and plain https.

import (
	"bufio"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/connect/protocol"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
)

// The synthetic measurement name -- an extender id under a zone (N1) --
// network host and page origin of the tests.
const (
	testLagName        = "0192e4a1-7b3c-7d2e-9f10-5a6b7c8d9e0f.extender.example"
	testLagNetworkHost = "lag-test.example"
	testLagOrigin      = "https://lag.dev"
)

// The lifecycle's certificate, as a fake that tests swap (M10), with the
// change monitor the lifecycle keeps (C4, C9).
type testLagCertificates struct {
	stateLock   sync.Mutex
	names       []string
	certificate *tls.Certificate
	changes     *connect.MonitorValue[uint64]
}

func (self *testLagCertificates) Certificate() ([]string, *tls.Certificate) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return slices.Clone(self.names), self.certificate
}

// Swaps the certificate and says so on the change monitor, as an install, a
// withdrawal or a drop does.
func (self *testLagCertificates) set(names []string, certificate *tls.Certificate) {
	self.setQuietly(names, certificate)
	self.changes.Update(func(version uint64) uint64 {
		return version + 1
	})
}

// Swaps the certificate with no word on the monitor, as a change the API only
// sees by reading Certificate again.
func (self *testLagCertificates) setQuietly(names []string, certificate *tls.Certificate) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.names = names
	self.certificate = certificate
}

// The lifecycle's report queue, as a sink (M10).
type testLagReports struct {
	reports chan *protocol.LagReport
}

func (self *testLagReports) Submit(report *protocol.LagReport) {
	self.reports <- report
}

type lagApiFixture struct {
	t *testing.T

	identityPublicKey ed25519.PublicKey
	rootPrivateKey    ed25519.PrivateKey
	certificates      *testLagCertificates
	certificate       *tls.Certificate
	rootCas           *x509.CertPool
	reports           *testLagReports

	server   *ExtenderServer
	api      *extenderLagApi
	listener net.Listener
	address  string
	// starts the accept loop on the first dial, so whatever a test sets up
	// before it dials is ordered before every connection the loop serves
	startOnce sync.Once
	startLoop func()
	// the server names the accept path saw and whether the api took them
	dispatches chan testLagDispatch
}

type testLagDispatch struct {
	serverName string
	dispatched bool
}

func newLagApiFixture(t *testing.T, configure func(settings *ExtenderLagSettings)) *lagApiFixture {
	t.Helper()
	seed, err := connect.NewExtenderKeySeed()
	if err != nil {
		t.Fatal(err)
	}
	identityPublicKey, err := connect.ExtenderPublicKeyFromSeed(seed)
	if err != nil {
		t.Fatal(err)
	}
	rootPublicKey, rootPrivateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	certificate, err := selfSignedCertificate([]string{testLagName}, "Lag", time.Hour, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	rootCas := x509.NewCertPool()
	rootCas.AddCert(certificate.Leaf)

	fixture := &lagApiFixture{
		t:                 t,
		identityPublicKey: identityPublicKey,
		rootPrivateKey:    rootPrivateKey,
		certificates: &testLagCertificates{
			changes: connect.NewMonitorValue[uint64](0),
		},
		certificate: certificate,
		rootCas:     rootCas,
		reports:     &testLagReports{reports: make(chan *protocol.LagReport, 64)},
		dispatches:  make(chan testLagDispatch, 64),
	}
	fixture.certificates.set([]string{testLagName}, certificate)

	settings := DefaultExtenderSettings()
	settings.IdentityKeySeed = seed
	settings.HeaderTimeout = 5 * time.Second
	// every client of a test is one loopback subnet, which the per-subnet
	// action limit would count as one flooding client; the per-subnet tests
	// set their own (A12)
	settings.AdmissionActionsPerSubnetPerMinute = 0
	ctx, cancel := context.WithCancel(context.Background())
	fixture.server = NewExtenderServer(ctx, nil, nil, nil, &net.Dialer{}, settings)

	lagSettings := DefaultExtenderLagSettings()
	lagSettings.RootPublicKeys = func() []ed25519.PublicKey {
		return []ed25519.PublicKey{rootPublicKey}
	}
	lagSettings.NetworkHosts = []string{testLagNetworkHost}
	if configure != nil {
		configure(&lagSettings)
	}
	fixture.api = newExtenderLagApi(
		fixture.server,
		lagSettings,
		fixture.certificates,
		fixture.reports,
		nil,
		fixture.certificates.changes,
	)
	if fixture.api == nil {
		t.Fatal("the lag api was not built")
	}
	// the fixture's api replaces the one the server wired with no lifecycle,
	// before anything reads it, so the server's close covers it
	fixture.server.lag.close()
	fixture.server.lag = fixture.api

	fixture.listener, err = net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	fixture.address = fixture.listener.Addr().String()
	var acceptWorkers sync.WaitGroup
	acceptWorkers.Add(1)
	var acceptLoop func()
	fixture.startLoop = func() {
		go acceptLoop()
	}
	acceptLoop = func() {
		defer acceptWorkers.Done()
		for {
			conn, err := fixture.listener.Accept()
			if err != nil {
				return
			}
			acceptWorkers.Add(1)
			go func() {
				defer acceptWorkers.Done()
				defer conn.Close()
				// the accept path's peek (M1)
				conn.SetDeadline(time.Now().Add(5 * time.Second))
				readBytes, parsed, ok := peekClientHello(conn, 16*1024)
				if !ok {
					return
				}
				dispatched := fixture.api.dispatch(ctx, conn, readBytes, parsed.ServerName)
				select {
				case fixture.dispatches <- testLagDispatch{serverName: parsed.ServerName, dispatched: dispatched}:
				default:
				}
			}()
		}
	}
	t.Cleanup(func() {
		// a fixture never dialed still runs and ends its loop
		fixture.start()
		fixture.listener.Close()
		fixture.api.close()
		fixture.server.CloseAndWait()
		cancel()
		acceptWorkers.Wait()
	})
	return fixture
}

// A test that cannot wait out the real timings shortens them before any
// session opens.
func (self *lagApiFixture) setTimings(configure func(timings *lagApiTimings)) {
	configure(&self.api.timings)
}

// Starts the accept loop, once.
func (self *lagApiFixture) start() {
	self.startOnce.Do(self.startLoop)
}

// The address of the fixture's accept path, started.
func (self *lagApiFixture) dialAddress() string {
	self.start()
	return self.address
}

func (self *lagApiFixture) dialContext(ctx context.Context, network string, address string) (net.Conn, error) {
	return (&net.Dialer{}).DialContext(ctx, "tcp", self.dialAddress())
}

// An https client that resolves every name to the fixture and offers h2 too,
// as a browser does.
func (self *lagApiFixture) httpClient() *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			DialContext: self.dialContext,
			TLSClientConfig: &tls.Config{
				RootCAs: self.rootCas,
			},
			ForceAttemptHTTP2:  true,
			DisableCompression: true,
		},
		Timeout: 30 * time.Second,
	}
}

func (self *lagApiFixture) get(path string, origin string) *http.Response {
	self.t.Helper()
	request, err := http.NewRequest(http.MethodGet, "https://"+testLagName+path, nil)
	if err != nil {
		self.t.Fatal(err)
	}
	if origin != "" {
		request.Header.Set("Origin", origin)
	}
	response, err := self.httpClient().Do(request)
	if err != nil {
		self.t.Fatal(err)
	}
	return response
}

func (self *lagApiFixture) dialWebSocket(path string, origin string) (*websocket.Conn, *http.Response, error) {
	dialer := &websocket.Dialer{
		NetDialContext: self.dialContext,
		TLSClientConfig: &tls.Config{
			RootCAs:    self.rootCas,
			ServerName: testLagName,
		},
		HandshakeTimeout: 5 * time.Second,
	}
	header := http.Header{}
	if origin != "" {
		header.Set("Origin", origin)
	}
	return dialer.Dial("wss://"+testLagName+path, header)
}

// A signed ticket for this extender, changed by configure before signing.
func (self *lagApiFixture) ticket(configure func(body *protocol.LagTicketBody)) string {
	self.t.Helper()
	now := time.Now()
	body := &protocol.LagTicketBody{
		TicketId:          testLagRandomBytes(16),
		ClientId:          testLagRandomBytes(16),
		ExtenderPublicKey: self.identityPublicKey,
		NetworkHost:       testLagNetworkHost,
		IssueTimeMs:       uint64(now.UnixMilli()),
		ExpireTimeMs:      uint64(now.Add(2 * time.Minute).UnixMilli()),
		TrackId:           testLagRandomBytes(16),
		TrackPosition:     3,
		PingIntervalMs:    250,
		DownloadMs:        5000,
		UploadMs:          5000,
		MaxStreams:        4,
		MaxDownloadBytes:  2 * 1024 * 1024 * 1024,
		MaxUploadBytes:    2 * 1024 * 1024 * 1024,
	}
	if configure != nil {
		configure(body)
	}
	ticket, err := connect.SignLagTicket(body, self.rootPrivateKey)
	if err != nil {
		self.t.Fatal(err)
	}
	ticketText, err := connect.EncodeLagTicket(ticket)
	if err != nil {
		self.t.Fatal(err)
	}
	return ticketText
}

// The next report, verified under the extender's key.
func (self *lagApiFixture) nextReport(timeout time.Duration) *protocol.LagReportBody {
	self.t.Helper()
	select {
	case report := <-self.reports.reports:
		body, err := connect.VerifyLagReport(report, self.identityPublicKey)
		if err != nil {
			self.t.Fatalf("report does not verify: %v", err)
		}
		return body
	case <-time.After(timeout):
		self.t.Fatal("no report")
		return nil
	}
}

func (self *lagApiFixture) noReport(wait time.Duration) {
	self.t.Helper()
	select {
	case <-self.reports.reports:
		self.t.Fatal("unexpected report")
	case <-time.After(wait):
	}
}

func testLagRandomBytes(byteCount int) []byte {
	b := make([]byte, byteCount)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return b
}

// Every message of M4 in one shape.
type testLagMessage struct {
	Type           string   `json:"type"`
	Session        string   `json:"session"`
	UploadKey      string   `json:"uploadKey"`
	PingIntervalMs int64    `json:"pingIntervalMs"`
	IntervalMs     int64    `json:"intervalMs"`
	DownloadMs     int64    `json:"downloadMs"`
	UploadMs       int64    `json:"uploadMs"`
	MaxStreams     int      `json:"maxStreams"`
	T              int64    `json:"t"`
	Ms             *float64 `json:"ms"`
	Lost           bool     `json:"lost"`
	Bytes          uint64   `json:"bytes"`
	At             *float64 `json:"at"`
	UploadVerified bool     `json:"uploadVerified"`
	EndReason      string   `json:"endReason"`
	UploadBytes    []uint64 `json:"uploadBytes"`
	Code           string   `json:"code"`
	RetryAfterMs   *int64   `json:"retryAfterMs"`

	raw map[string]any
}

// One control session as a browser holds it: a reader that answers pings --
// at once, late, or never -- and sorts what arrives.
type lagTestSession struct {
	t       *testing.T
	fixture *lagApiFixture
	conn    *websocket.Conn

	writeLock sync.Mutex
	pongDelay atomic.Int64
	dropPongs atomic.Bool

	rtts     chan testLagMessage
	messages chan testLagMessage
	// the close the extender sent, once the session is over
	closeCode int
	closeText string
	done      chan struct{}

	hello testLagMessage
}

// Opens a control session; it is closed by the browser at cleanup if the test
// leaves it open.
func (self *lagApiFixture) openSession(ticketText string, origin string) *lagTestSession {
	self.t.Helper()
	conn, _, err := self.dialWebSocket("/lag/v1/session?ticket="+url.QueryEscape(ticketText), origin)
	if err != nil {
		self.t.Fatal(err)
	}
	session := &lagTestSession{
		t:        self.t,
		fixture:  self,
		conn:     conn,
		rtts:     make(chan testLagMessage, 1024),
		messages: make(chan testLagMessage, 1024),
		done:     make(chan struct{}),
	}
	conn.SetPingHandler(func(appData string) error {
		if session.dropPongs.Load() {
			return nil
		}
		if delay := time.Duration(session.pongDelay.Load()); 0 < delay {
			go func() {
				time.Sleep(delay)
				conn.WriteControl(websocket.PongMessage, []byte(appData), time.Now().Add(time.Second))
			}()
			return nil
		}
		return conn.WriteControl(websocket.PongMessage, []byte(appData), time.Now().Add(time.Second))
	})
	go func() {
		defer close(session.done)
		for {
			_, messageBytes, err := conn.ReadMessage()
			if err != nil {
				closeError := &websocket.CloseError{}
				if errors.As(err, &closeError) {
					session.closeCode = closeError.Code
					session.closeText = closeError.Text
				}
				return
			}
			message := testLagMessage{}
			if err := json.Unmarshal(messageBytes, &message); err != nil {
				continue
			}
			json.Unmarshal(messageBytes, &message.raw)
			if message.Type == "rtt" {
				session.rtts <- message
			} else {
				session.messages <- message
			}
		}
	}()
	self.t.Cleanup(func() {
		session.close()
		select {
		case <-session.done:
		case <-time.After(10 * time.Second):
			self.t.Error("the session did not end")
		}
	})
	return session
}

// Opens a session and reads its hello.
func (self *lagApiFixture) openHelloSession(ticketText string) *lagTestSession {
	self.t.Helper()
	session := self.openSession(ticketText, testLagOrigin)
	session.hello = session.next("hello", 5*time.Second)
	return session
}

// The next message of a type, failing on any other non-rtt message first.
func (self *lagTestSession) next(messageType string, timeout time.Duration) testLagMessage {
	self.t.Helper()
	select {
	case message := <-self.messages:
		if message.Type != messageType {
			self.t.Fatalf("expected %s, got %s %+v", messageType, message.Type, message.raw)
		}
		return message
	case <-time.After(timeout):
		self.t.Fatalf("no %s message", messageType)
		return testLagMessage{}
	}
}

// The next message that is not progress.
func (self *lagTestSession) nextResult(timeout time.Duration) testLagMessage {
	self.t.Helper()
	deadline := time.After(timeout)
	for {
		select {
		case message := <-self.messages:
			if message.Type == "uploaded" {
				continue
			}
			return message
		case <-deadline:
			self.t.Fatal("no result message")
			return testLagMessage{}
		}
	}
}

func (self *lagTestSession) send(message any) {
	self.t.Helper()
	self.writeLock.Lock()
	defer self.writeLock.Unlock()
	if err := self.conn.WriteJSON(message); err != nil {
		self.t.Fatal(err)
	}
}

func (self *lagTestSession) load(phase string) {
	self.t.Helper()
	self.send(map[string]any{"type": "load", "phase": phase})
}

// The browser's close (M4), then the extender's answer.
func (self *lagTestSession) close() {
	self.writeLock.Lock()
	self.conn.WriteControl(
		websocket.CloseMessage,
		websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""),
		time.Now().Add(time.Second),
	)
	self.writeLock.Unlock()
	select {
	case <-self.done:
	case <-time.After(5 * time.Second):
	}
	self.conn.Close()
}

// Waits for the extender to end the session and returns its close.
func (self *lagTestSession) waitClosed(timeout time.Duration) (int, string) {
	self.t.Helper()
	select {
	case <-self.done:
		return self.closeCode, self.closeText
	case <-time.After(timeout):
		self.t.Fatal("the session was not closed")
		return 0, ""
	}
}

// The rtt messages that arrive within wait.
func (self *lagTestSession) rttsFor(wait time.Duration) []testLagMessage {
	rtts := []testLagMessage{}
	deadline := time.After(wait)
	for {
		select {
		case rtt := <-self.rtts:
			rtts = append(rtts, rtt)
		case <-deadline:
			return rtts
		}
	}
}

func (self *lagTestSession) uploadKey() []byte {
	self.t.Helper()
	uploadKey, err := base64.RawURLEncoding.DecodeString(self.hello.UploadKey)
	if err != nil || len(uploadKey) != 16 {
		self.t.Fatalf("bad upload key %q", self.hello.UploadKey)
	}
	return uploadKey
}

// The upload keystream of stream k from offset 0 (M6), built from the counter
// block as M6 writes it, independently of the extender's own helper.
func testLagUploadStream(t *testing.T, uploadKey []byte, streamIndex uint64) cipher.Stream {
	t.Helper()
	block, err := aes.NewCipher(uploadKey)
	if err != nil {
		t.Fatal(err)
	}
	counterBlock := make([]byte, aes.BlockSize)
	binary.BigEndian.PutUint64(counterBlock[0:8], streamIndex)
	return cipher.NewCTR(block, counterBlock)
}

// What one simulated browser load saw.
type lagTestLoad struct {
	downloadedByteCount int64
	uploadedByteCount   int64
	// the streams that opened; one that connects after its window closed is
	// refused, as the browser's client expects (S5)
	downloadStreamCount int64
	uploadStreamCount   int64
	// the extender's upload progress, in order
	uploaded         []testLagMessage
	result           testLagMessage
	uploadCloseCodes []int
	// when the progress that closes the upload's window arrived, when done
	// was sent (zero when it was not), and when the result arrived
	windowClosedTime time.Time
	doneTime         time.Time
	resultTime       time.Time
}

// Checks the upload's progress and result against its report (M4, amendment
// 14): each progress carries at, when the latest counted read arrived, never
// past its ms and never going back; and the result carries the upload's
// series exactly as the report signs it, whose sum is the last progress's
// bytes.
func (self *lagTestLoad) checkUpload(t *testing.T, body *protocol.LagReportBody) {
	t.Helper()
	previousAt := 0.0
	for i, uploaded := range self.uploaded {
		if uploaded.At == nil || *uploaded.At < previousAt || *uploaded.Ms < *uploaded.At {
			t.Errorf("progress %d %+v, after at %f", i, uploaded.raw, previousAt)
			continue
		}
		previousAt = *uploaded.At
	}
	if _, ok := self.result.raw["uploadBytes"]; !ok || !slices.Equal(self.result.UploadBytes, body.UploadBytes) {
		t.Errorf("result's upload %v, reported %v", self.result.raw["uploadBytes"], body.UploadBytes)
	}
	if 0 < len(self.uploaded) {
		byteCount := uint64(0)
		for _, intervalByteCount := range body.UploadBytes {
			byteCount += intervalByteCount
		}
		if last := self.uploaded[len(self.uploaded)-1]; byteCount != last.Bytes {
			t.Errorf("reported %d, told the browser %d", byteCount, last.Bytes)
		}
	}
}

// Options of a simulated load: corrupt, when set, changes one upload message
// of one stream before it is sent.
type lagTestLoadOptions struct {
	corrupt func(streamIndex int, messageIndex int, message []byte)
	// the bytes each upload stream sends at most; 0 sends until closed
	uploadByteCount int64
	// the wait between one upload stream's messages; 0 sends them back to back
	uploadInterval time.Duration
	// never say done, as a browser that went away mid load
	noDone bool
	// say done at the first progress, with the upload's window still open
	earlyDone bool
	// called when the progress that closes the upload's window arrives
	windowClosed func()
}

// Runs the session's load as the browser's client does (S5): the download on
// every stream with received progress each interval, each stream closed once
// the download's own window, from its first byte, is over (the extender writes
// on until then, amendment 15); then the upload on every stream; then done,
// once the extender's progress says the upload's window closed; then the
// loadResult. An extender that ends the load first sends the result with no
// done.
func (self *lagTestSession) runLoad(options lagTestLoadOptions) *lagTestLoad {
	self.t.Helper()
	fixture := self.fixture
	result := &lagTestLoad{}
	streamCount := self.hello.MaxStreams

	self.load("download")
	client := fixture.httpClient()
	var downloadedByteCount atomic.Int64
	var downloadStreamCount atomic.Int64
	var uploadStreamCount atomic.Int64
	var firstByteTime atomic.Pointer[time.Time]
	progressDone := make(chan struct{})
	progressStopped := make(chan struct{})
	go func() {
		defer close(progressStopped)
		ticker := time.NewTicker(lagApiInterval)
		defer ticker.Stop()
		for {
			select {
			case <-progressDone:
				return
			case <-ticker.C:
				if startTime := firstByteTime.Load(); startTime != nil {
					self.send(map[string]any{
						"type":  "received",
						"bytes": downloadedByteCount.Load(),
						"ms":    time.Since(*startTime).Milliseconds(),
					})
				}
			}
		}
	}()
	var downloads sync.WaitGroup
	for k := 0; k < streamCount; k += 1 {
		downloads.Add(1)
		go func() {
			defer downloads.Done()
			request, _ := http.NewRequest(
				http.MethodGet,
				fmt.Sprintf("https://%s/lag/v1/down?session=%s&stream=%d", testLagName, self.hello.Session, k),
				nil,
			)
			request.Header.Set("Origin", testLagOrigin)
			response, err := client.Do(request)
			if err != nil {
				self.t.Errorf("download %d: %v", k, err)
				return
			}
			defer response.Body.Close()
			if response.StatusCode != http.StatusOK {
				if response.StatusCode != http.StatusConflict {
					self.t.Errorf("download %d: %d", k, response.StatusCode)
				}
				return
			}
			downloadStreamCount.Add(1)
			buffer := make([]byte, 32*1024)
			downloadDuration := time.Duration(self.hello.DownloadMs) * time.Millisecond
			for {
				n, err := response.Body.Read(buffer)
				if 0 < n {
					now := time.Now()
					firstByteTime.CompareAndSwap(nil, &now)
					downloadedByteCount.Add(int64(n))
				}
				if err != nil {
					return
				}
				if startTime := firstByteTime.Load(); startTime != nil && downloadDuration <= time.Since(*startTime) {
					return
				}
			}
		}()
	}
	downloads.Wait()
	close(progressDone)
	<-progressStopped
	result.downloadedByteCount = downloadedByteCount.Load()
	result.downloadStreamCount = downloadStreamCount.Load()

	self.load("upload")
	uploadKey := self.uploadKey()
	var uploadedByteCount atomic.Int64
	var uploads sync.WaitGroup
	closeCodes := make([]int, streamCount)
	for k := 0; k < streamCount; k += 1 {
		uploads.Add(1)
		go func() {
			defer uploads.Done()
			conn, response, err := fixture.dialWebSocket(
				fmt.Sprintf("/lag/v1/up?session=%s&stream=%d", self.hello.Session, k),
				testLagOrigin,
			)
			if err != nil {
				if response == nil || response.StatusCode != http.StatusConflict {
					self.t.Errorf("upload %d: %v", k, err)
				}
				return
			}
			defer conn.Close()
			uploadStreamCount.Add(1)
			readDone := make(chan struct{})
			go func() {
				defer close(readDone)
				for {
					if _, _, err := conn.NextReader(); err != nil {
						closeError := &websocket.CloseError{}
						if errors.As(err, &closeError) {
							closeCodes[k] = closeError.Code
						}
						return
					}
				}
			}()
			stream := testLagUploadStream(self.t, uploadKey, uint64(k))
			message := make([]byte, lagApiBufferByteCount)
			sentByteCount := int64(0)
			for messageIndex := 0; ; messageIndex += 1 {
				select {
				case <-readDone:
					return
				default:
				}
				byteCount := len(message)
				if 0 < options.uploadByteCount {
					byteCount = int(min(int64(byteCount), options.uploadByteCount-sentByteCount))
					if byteCount <= 0 {
						break
					}
				}
				clear(message[:byteCount])
				stream.XORKeyStream(message[:byteCount], message[:byteCount])
				if options.corrupt != nil {
					options.corrupt(k, messageIndex, message[:byteCount])
				}
				conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
				if err := conn.WriteMessage(websocket.BinaryMessage, message[:byteCount]); err != nil {
					break
				}
				sentByteCount += int64(byteCount)
				uploadedByteCount.Add(int64(byteCount))
				if 0 < options.uploadInterval {
					select {
					case <-readDone:
						return
					case <-time.After(options.uploadInterval):
					}
				}
			}
			<-readDone
		}()
	}
	windowClosedMs := float64(self.hello.UploadMs) - float64(self.hello.IntervalMs)/2
	deadline := time.After(30 * time.Second)
	for result.result.Type == "" {
		select {
		case message := <-self.messages:
			switch message.Type {
			case "uploaded":
				result.uploaded = append(result.uploaded, message)
				if options.earlyDone && result.doneTime.IsZero() {
					result.doneTime = time.Now()
					self.load("done")
				}
				if message.Ms != nil && windowClosedMs <= *message.Ms && result.windowClosedTime.IsZero() {
					result.windowClosedTime = time.Now()
					if options.windowClosed != nil {
						options.windowClosed()
					}
					if !options.noDone && result.doneTime.IsZero() {
						result.doneTime = time.Now()
						self.load("done")
					}
				}
			case "loadResult":
				result.result = message
				result.resultTime = time.Now()
			default:
				self.t.Fatalf("unexpected %s %+v", message.Type, message.raw)
			}
		case <-deadline:
			self.t.Fatal("no loadResult")
		}
	}
	uploads.Wait()
	result.uploadedByteCount = uploadedByteCount.Load()
	result.uploadStreamCount = uploadStreamCount.Load()
	result.uploadCloseCodes = closeCodes
	return result
}

// M10: the settings block's defaults, and what an extender does with them and
// nothing else: the API is built for an extender with an identity, recognizes
// no name, and so leaves the accept path as it was.
func TestLagApiDefaultSettings(t *testing.T) {
	settings := DefaultExtenderSettings()
	measurement := settings.Measurement
	// no daily budget by default (decision 18, Q6)
	if !measurement.Enabled || measurement.MaxSessions != 512 || measurement.ConcurrentLoads != 2 ||
		measurement.DailyBudgetBytes != 0 ||
		!slices.Equal(measurement.AllowedOrigins, []string{"https://lag.dev", "https://ur.io"}) {
		t.Errorf("measurement defaults %+v", measurement)
	}
	if measurement.RootPublicKeys != nil || 0 < len(measurement.NetworkHosts) {
		t.Error("admission trusts something by default")
	}
	if settings.LagLifecycle != nil {
		t.Error("a lifecycle by default")
	}

	seed, err := connect.NewExtenderKeySeed()
	if err != nil {
		t.Fatal(err)
	}
	newServer := func(configure func(settings *ExtenderSettings)) *ExtenderServer {
		settings := DefaultExtenderSettings()
		settings.IdentityKeySeed = seed
		if configure != nil {
			configure(settings)
		}
		server := NewExtenderServer(context.Background(), nil, nil, nil, &net.Dialer{}, settings)
		t.Cleanup(server.CloseAndWait)
		return server
	}

	server := newServer(nil)
	if server.lag == nil {
		t.Fatal("no api for an extender with an identity")
	}
	if server.lag.peeks() || server.lag.servesName(testLagName) {
		t.Error("the api recognizes a name with no certificate to keep")
	}
	if server.measurementZone != connect.ExtenderMeasurementZone {
		t.Errorf("measurement zone %q", server.measurementZone)
	}
	if server.lag.settings.MaxSessions != 512 || server.lag.settings.ConcurrentLoads != 2 ||
		server.lag.settings.DailyBudgetBytes != 0 || server.lag.budget.limit != 0 {
		t.Errorf("api settings %+v", server.lag.settings)
	}
	if len(server.lag.allowedOrigins) != 2 || !server.lag.allowedOrigins["https://lag.dev"] || !server.lag.allowedOrigins["https://ur.io"] {
		t.Errorf("allowed origins %v", server.lag.allowedOrigins)
	}
	if server.lag.ticketExpireSkew != 5*time.Minute {
		t.Errorf("ticket expiry skew %s", server.lag.ticketExpireSkew)
	}
	// zero limits take the defaults rather than refusing everything, and a
	// zero budget stays no budget rather than taking any default
	server = newServer(func(settings *ExtenderSettings) {
		settings.Measurement = ExtenderLagSettings{Enabled: true}
	})
	if server.lag.settings.MaxSessions != 512 || server.lag.settings.ConcurrentLoads != 2 ||
		server.lag.settings.DailyBudgetBytes != 0 || server.lag.budget.limit != 0 {
		t.Errorf("zero settings became %+v", server.lag.settings)
	}
	// a budget an operator sets is kept as set
	server = newServer(func(settings *ExtenderSettings) {
		settings.Measurement.DailyBudgetBytes = 1 << 30
	})
	if server.lag.budget.limit != 1<<30 {
		t.Errorf("an operator's budget became %d", server.lag.budget.limit)
	}

	// turned off, or with no identity to sign a report with: no api, and
	// everything the accept path asks of it answers no
	for name, configure := range map[string]func(settings *ExtenderSettings){
		"off": func(settings *ExtenderSettings) {
			settings.Measurement.Enabled = false
		},
		"no identity": func(settings *ExtenderSettings) {
			settings.IdentityKeySeed = nil
		},
	} {
		server := newServer(configure)
		if server.lag != nil {
			t.Errorf("%s: an api", name)
		}
		if server.lag.peeks() || server.lag.servesName(testLagName) || server.lag.dispatch(context.Background(), nil, nil, testLagName) {
			t.Errorf("%s: the missing api takes a connection", name)
		}
		server.lag.close()
	}

	// a dev origin is kept only in a test build (M2)
	server = newServer(func(settings *ExtenderSettings) {
		settings.Measurement.AllowedOrigins = []string{"https://lag.dev", "http://localhost:5173"}
	})
	if len(server.lag.allowedOrigins) != 1 || !server.lag.allowedOrigins["https://lag.dev"] {
		t.Errorf("allowed origins %v", server.lag.allowedOrigins)
	}
}

func TestLagApiCanonicalOrigin(t *testing.T) {
	for _, origin := range []struct {
		origin    string
		canonical string
		ok        bool
		devOk     bool
	}{
		{origin: "https://lag.dev", canonical: "https://lag.dev", ok: true, devOk: true},
		{origin: "https://LAG.dev/", canonical: "https://lag.dev", ok: true, devOk: true},
		{origin: "https://lag.dev:443", canonical: "https://lag.dev", ok: true, devOk: true},
		{origin: "https://ur.io:8443", canonical: "https://ur.io:8443", ok: true, devOk: true},
		{origin: "https://lag.dev/path", ok: false, devOk: false},
		{origin: "https://user@lag.dev", ok: false, devOk: false},
		{origin: "null", ok: false, devOk: false},
		{origin: "ftp://lag.dev", ok: false, devOk: false},
		{origin: "http://lag.dev", canonical: "http://lag.dev", ok: false, devOk: true},
		{origin: "http://localhost:5173", canonical: "http://localhost:5173", ok: false, devOk: true},
		{origin: "https://127.0.0.1:4321", canonical: "https://127.0.0.1:4321", ok: false, devOk: true},
		{origin: "https://[::1]:4321", canonical: "https://[::1]:4321", ok: false, devOk: true},
	} {
		for _, dev := range []bool{false, true} {
			func() {
				devOriginsAllowed := lagApiDevOriginsAllowed
				lagApiDevOriginsAllowed = dev
				defer func() {
					lagApiDevOriginsAllowed = devOriginsAllowed
				}()
				canonical, ok := lagApiCanonicalOrigin(origin.origin)
				expectOk := origin.ok
				if dev {
					expectOk = origin.devOk
				}
				if ok != expectOk || (ok && canonical != origin.canonical) {
					t.Errorf("%q dev %t: %q %t", origin.origin, dev, canonical, ok)
				}
			}()
		}
	}
}

// M2: cors for an allowed origin only, never with credentials; no-store and
// Vary on every response, refusals and 404 included; http/1.1 although the
// client offered h2 (M1).
func TestLagApiHttpHeaders(t *testing.T) {
	fixture := newLagApiFixture(t, nil)

	response := fixture.get("/lag/v1/down?session=AAAAAAAAAAAAAAAAAAAAAA&stream=0", testLagOrigin)
	io.Copy(io.Discard, response.Body)
	response.Body.Close()
	if response.ProtoMajor != 1 || response.TLS == nil || response.TLS.NegotiatedProtocol != "http/1.1" {
		t.Fatalf("protocol %s alpn %v", response.Proto, response.TLS)
	}
	if response.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown session: %d", response.StatusCode)
	}
	for header, value := range map[string]string{
		"Access-Control-Allow-Origin": testLagOrigin,
		"Timing-Allow-Origin":         testLagOrigin,
		"Vary":                        "Origin",
		"Cache-Control":               "no-store",
	} {
		if response.Header.Get(header) != value {
			t.Errorf("%s: %q", header, response.Header.Get(header))
		}
	}
	if _, ok := response.Header["Access-Control-Allow-Credentials"]; ok {
		t.Error("credentials allowed")
	}
	if _, ok := response.Header["Set-Cookie"]; ok {
		t.Error("cookie set")
	}

	for _, origin := range []string{"https://evil.example", ""} {
		response := fixture.get("/lag/v1/down?session=AAAAAAAAAAAAAAAAAAAAAA&stream=0", origin)
		response.Body.Close()
		if response.StatusCode != http.StatusForbidden {
			t.Errorf("origin %q: %d", origin, response.StatusCode)
		}
		if response.Header.Get("Access-Control-Allow-Origin") != "" || response.Header.Get("Timing-Allow-Origin") != "" {
			t.Errorf("origin %q echoed", origin)
		}
		if response.Header.Get("Cache-Control") != "no-store" || response.Header.Get("Vary") != "Origin" {
			t.Errorf("origin %q: headers %v", origin, response.Header)
		}
	}

	response = fixture.get("/elsewhere", testLagOrigin)
	response.Body.Close()
	if response.StatusCode != http.StatusNotFound || response.Header.Get("Cache-Control") != "no-store" {
		t.Errorf("elsewhere: %d %v", response.StatusCode, response.Header)
	}
	// the feed is not mounted in this fixture (M9)
	response = fixture.get("/ur/feed/v1", testLagOrigin)
	response.Body.Close()
	if response.StatusCode != http.StatusNotFound {
		t.Errorf("feed: %d", response.StatusCode)
	}

	request, _ := http.NewRequest(http.MethodOptions, "https://"+testLagName+"/lag/v1/down", nil)
	request.Header.Set("Origin", testLagOrigin)
	request.Header.Set("Access-Control-Request-Method", "GET")
	response, err := fixture.httpClient().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusNoContent ||
		response.Header.Get("Access-Control-Allow-Methods") != "GET" ||
		response.Header.Get("Access-Control-Allow-Origin") != testLagOrigin ||
		response.Header.Get("Cache-Control") != "no-store" {
		t.Errorf("preflight: %d %v", response.StatusCode, response.Header)
	}

	request, _ = http.NewRequest(http.MethodPost, "https://"+testLagName+"/lag/v1/down", nil)
	request.Header.Set("Origin", testLagOrigin)
	response, err = fixture.httpClient().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusMethodNotAllowed || response.Header.Get("Cache-Control") != "no-store" {
		t.Errorf("post: %d %v", response.StatusCode, response.Header)
	}

	// the websocket upgrade carries no-store too
	conn, upgradeResponse, err := fixture.dialWebSocket("/lag/v1/session?ticket="+url.QueryEscape(fixture.ticket(nil)), testLagOrigin)
	if err != nil {
		t.Fatal(err)
	}
	conn.Close()
	if upgradeResponse.Header.Get("Cache-Control") != "no-store" {
		t.Errorf("upgrade: %v", upgradeResponse.Header)
	}
}

// The feed is mounted from the lifecycle and checked for origin like every
// other upgrade (M2, M9).
func TestLagApiFeedMount(t *testing.T) {
	fixture := newLagApiFixture(t, nil)
	var servedCount atomic.Int64
	fixture.api.feedHandler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		servedCount.Add(1)
		w.WriteHeader(http.StatusTeapot)
	})
	response := fixture.get("/ur/feed/v1", testLagOrigin)
	response.Body.Close()
	if response.StatusCode != http.StatusTeapot || response.Header.Get("Cache-Control") != "no-store" {
		t.Errorf("feed: %d %v", response.StatusCode, response.Header)
	}
	if servedCount.Load() != 1 {
		t.Errorf("served %d", servedCount.Load())
	}
	// the lifecycle's handler checks no origin of its own: one that is not
	// allowed, or missing, never reaches it, upgrade or not
	for _, origin := range []string{"https://evil.example", "null", ""} {
		response = fixture.get("/ur/feed/v1", origin)
		response.Body.Close()
		if response.StatusCode != http.StatusForbidden {
			t.Errorf("feed from %q: %d", origin, response.StatusCode)
		}
		_, upgradeResponse, err := fixture.dialWebSocket("/ur/feed/v1", origin)
		if err == nil || upgradeResponse == nil || upgradeResponse.StatusCode != http.StatusForbidden {
			t.Errorf("feed upgrade from %q: %v", origin, err)
		}
	}
	if servedCount.Load() != 1 {
		t.Errorf("served %d after refused origins", servedCount.Load())
	}
}

// M1 at the api: a measurement name with a certificate installed is served
// with that certificate; another name is left to the existing path; a
// measurement name with no certificate is closed, never answered with another
// leaf.
func TestLagApiDispatchNames(t *testing.T) {
	fixture := newLagApiFixture(t, nil)

	conn, err := tls.Dial("tcp", fixture.dialAddress(), &tls.Config{
		RootCAs:    fixture.rootCas,
		ServerName: testLagName,
		NextProtos: []string{"h2", "http/1.1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	state := conn.ConnectionState()
	if state.NegotiatedProtocol != "http/1.1" {
		t.Errorf("alpn %q", state.NegotiatedProtocol)
	}
	if !state.PeerCertificates[0].Equal(fixture.certificate.Leaf) {
		t.Error("not the measurement certificate")
	}
	conn.Close()
	if dispatch := <-fixture.dispatches; !dispatch.dispatched || dispatch.serverName != testLagName {
		t.Errorf("dispatch %+v", dispatch)
	}

	// another name: not the api's
	_, err = tls.Dial("tcp", fixture.dialAddress(), &tls.Config{
		InsecureSkipVerify: true,
		ServerName:         "other.example",
	})
	if err == nil {
		t.Error("another name was served by the api")
	}
	if dispatch := <-fixture.dispatches; dispatch.dispatched {
		t.Errorf("dispatch %+v", dispatch)
	}

	// names compare without case, as dns names do
	conn, err = tls.Dial("tcp", fixture.dialAddress(), &tls.Config{
		RootCAs:    fixture.rootCas,
		ServerName: "0192E4A1-7B3C-7D2E-9F10-5A6B7C8D9E0F.EXTENDER.example",
	})
	if err != nil {
		t.Fatal(err)
	}
	conn.Close()
	<-fixture.dispatches

	// the name, with no certificate installed: not the api's, so the existing
	// path takes it like any unknown name (M1), which this fixture's accept
	// path, having none, closes
	fixture.certificates.set([]string{testLagName}, nil)
	_, err = tls.Dial("tcp", fixture.dialAddress(), &tls.Config{
		InsecureSkipVerify: true,
		ServerName:         testLagName,
	})
	if err == nil {
		t.Error("a measurement name was answered without its certificate")
	}
	if dispatch := <-fixture.dispatches; dispatch.dispatched {
		t.Errorf("dispatch %+v", dispatch)
	}
	if fixture.api.peeks() {
		t.Error("peeks with no certificate installed")
	}

	// no names: nothing to recognize
	fixture.certificates.set(nil, nil)
	if fixture.api.peeks() {
		t.Error("peeks with no names")
	}
}

// M3: a ticket that is not this extender's, expired or forged is refused with
// ticket and 4403; a used ticket is refused as reused; an origin that is not
// allowed is refused as origin before the ticket is read.
func TestLagApiAdmissionTicket(t *testing.T) {
	fixture := newLagApiFixture(t, nil)

	_, otherRootPrivateKey, _ := ed25519.GenerateKey(rand.Reader)
	otherSeed, _ := connect.NewExtenderKeySeed()
	otherPublicKey, _ := connect.ExtenderPublicKeyFromSeed(otherSeed)
	forged := func() string {
		ticket, err := connect.SignLagTicket(&protocol.LagTicketBody{
			TicketId:          testLagRandomBytes(16),
			ExtenderPublicKey: fixture.identityPublicKey,
			NetworkHost:       testLagNetworkHost,
			ExpireTimeMs:      uint64(time.Now().Add(time.Minute).UnixMilli()),
		}, otherRootPrivateKey)
		if err != nil {
			t.Fatal(err)
		}
		ticketText, _ := connect.EncodeLagTicket(ticket)
		return ticketText
	}()
	for name, ticketText := range map[string]string{
		"garbage": "not-a-ticket",
		"empty":   "",
		"forged":  forged,
		// past its expiry by more than the clock skew a record is allowed
		"expired": fixture.ticket(func(body *protocol.LagTicketBody) {
			body.ExpireTimeMs = uint64(time.Now().Add(-fixture.api.ticketExpireSkew - time.Second).UnixMilli())
		}),
		"other extender": fixture.ticket(func(body *protocol.LagTicketBody) {
			body.ExtenderPublicKey = otherPublicKey
		}),
		"other network": fixture.ticket(func(body *protocol.LagTicketBody) {
			body.NetworkHost = "other.example"
		}),
		"short id": fixture.ticket(func(body *protocol.LagTicketBody) {
			body.TicketId = testLagRandomBytes(8)
		}),
	} {
		session := fixture.openSession(ticketText, testLagOrigin)
		if message := session.next("error", 5*time.Second); message.Code != lagApiErrorTicket || message.RetryAfterMs != nil {
			t.Errorf("%s: %+v", name, message.raw)
		}
		// the browser's client waits 2 s for the close after the error (S5)
		if code, text := session.waitClosed(time.Second); code != lagApiCloseCodeRefused || text != lagApiErrorTicket {
			t.Errorf("%s: close %d %q", name, code, text)
		}
	}

	ticketText := fixture.ticket(nil)
	session := fixture.openSession(ticketText, "https://evil.example")
	if message := session.next("error", 5*time.Second); message.Code != lagApiErrorOrigin {
		t.Errorf("origin: %+v", message.raw)
	}
	if code, _ := session.waitClosed(5 * time.Second); code != lagApiCloseCodeRefused {
		t.Errorf("origin close %d", code)
	}
	session = fixture.openSession(ticketText, "")
	if message := session.next("error", 5*time.Second); message.Code != lagApiErrorOrigin {
		t.Errorf("no origin: %+v", message.raw)
	}

	// a ticket just past its expiry is within the skew: an extender whose
	// clock runs a little fast still admits it, once
	skewedTicketText := fixture.ticket(func(body *protocol.LagTicketBody) {
		body.ExpireTimeMs = uint64(time.Now().Add(-time.Minute).UnixMilli())
	})
	skewed := fixture.openHelloSession(skewedTicketText)
	skewed.close()
	fixture.nextReport(5 * time.Second)
	reusedSkewed := fixture.openSession(skewedTicketText, testLagOrigin)
	if message := reusedSkewed.next("error", 5*time.Second); message.Code != lagApiErrorReused {
		t.Errorf("reused within the skew: %+v", message.raw)
	}

	// the ticket was never admitted, so it still opens a session
	first := fixture.openHelloSession(ticketText)
	if first.hello.PingIntervalMs != 250 || first.hello.IntervalMs != 250 || first.hello.DownloadMs != 5000 ||
		first.hello.UploadMs != 5000 || first.hello.MaxStreams != 4 {
		t.Errorf("hello %+v", first.hello.raw)
	}
	if sessionId, ok := parseLagApiSessionId(first.hello.Session); !ok || fixture.api.session(sessionId) == nil {
		t.Errorf("hello session %q", first.hello.Session)
	}
	if len(first.uploadKey()) != 16 {
		t.Error("upload key")
	}
	reused := fixture.openSession(ticketText, testLagOrigin)
	if message := reused.next("error", 5*time.Second); message.Code != lagApiErrorReused {
		t.Errorf("reused: %+v", message.raw)
	}
	if code, _ := reused.waitClosed(5 * time.Second); code != lagApiCloseCodeRefused {
		t.Errorf("reused close %d", code)
	}
	// still reused once the first session ends: ids are kept until expiry
	first.close()
	fixture.nextReport(5 * time.Second)
	reused = fixture.openSession(ticketText, testLagOrigin)
	if message := reused.next("error", 5*time.Second); message.Code != lagApiErrorReused {
		t.Errorf("reused after close: %+v", message.raw)
	}
}

// M3: at most max_sessions open; past it, sessions with 4429, and room again
// once one closes.
func TestLagApiAdmissionSessions(t *testing.T) {
	fixture := newLagApiFixture(t, func(settings *ExtenderLagSettings) {
		settings.MaxSessions = 2
	})
	first := fixture.openHelloSession(fixture.ticket(nil))
	fixture.openHelloSession(fixture.ticket(nil))
	third := fixture.openSession(fixture.ticket(nil), testLagOrigin)
	if message := third.next("error", 5*time.Second); message.Code != lagApiErrorSessions {
		t.Errorf("sessions: %+v", message.raw)
	}
	if code, text := third.waitClosed(5 * time.Second); code != lagApiCloseCodeLimited || text != lagApiErrorSessions {
		t.Errorf("sessions close %d %q", code, text)
	}
	first.close()
	fixture.nextReport(5 * time.Second)
	fixture.openHelloSession(fixture.ticket(nil))
}

// M4: pings 4 a second while pongs come back at once, then back to back when
// the round trip is longer than the interval; one in flight at a time. Each
// goes out at the later of the interval after the previous send and the
// previous pong, which the times in the rtt messages show.
func TestLagApiPingCadence(t *testing.T) {
	fixture := newLagApiFixture(t, nil)
	session := fixture.openHelloSession(fixture.ticket(nil))

	// the gaps between sends, each checked against the round trip before it;
	// the bounds leave room for a loaded machine's scheduling
	gapsOf := func(rtts []testLagMessage) []int64 {
		gaps := []int64{}
		for i := 1; i < len(rtts); i += 1 {
			if rtts[i-1].Ms == nil || rtts[i-1].Lost {
				t.Errorf("rtt %+v", rtts[i-1].raw)
				continue
			}
			gap := rtts[i].T - rtts[i-1].T
			expected := max(int64(250), int64(*rtts[i-1].Ms))
			if gap < expected-5 || expected+250 < gap {
				t.Errorf("gap %d ms after a round trip of %f ms", gap, *rtts[i-1].Ms)
			}
			gaps = append(gaps, gap)
		}
		slices.Sort(gaps)
		return gaps
	}

	rtts := session.rttsFor(2 * time.Second)
	if len(rtts) < 4 {
		t.Fatalf("%d rtts", len(rtts))
	}
	// 4 a second
	if gaps := gapsOf(rtts); gaps[len(gaps)/2] < 245 || 320 < gaps[len(gaps)/2] {
		t.Errorf("gaps %v ms", gaps)
	}

	// a round trip of 400 ms: the next ping goes at each pong
	session.pongDelay.Store(int64(400 * time.Millisecond))
	session.rttsFor(600 * time.Millisecond)
	rtts = session.rttsFor(2500 * time.Millisecond)
	if len(rtts) < 3 {
		t.Fatalf("%d slow rtts", len(rtts))
	}
	for _, rtt := range rtts {
		if rtt.Ms == nil || *rtt.Ms < 395 {
			t.Errorf("slow rtt %+v", rtt.raw)
		}
	}
	if gaps := gapsOf(rtts); gaps[0] < 395 || gaps[len(gaps)/2] < 395 || 600 < gaps[len(gaps)/2] {
		t.Errorf("slow gaps %v ms", gaps)
	}
}

// M4: a ping with no pong within the loss timeout is lost and the next goes at
// once; with no pong at all for the pong timeout the session ends, and its
// report holds the lost pings (M8).
func TestLagApiPingLossAndTimeout(t *testing.T) {
	fixture := newLagApiFixture(t, nil)
	fixture.setTimings(func(timings *lagApiTimings) {
		timings.pingLost = 300 * time.Millisecond
		timings.pongTimeout = 1200 * time.Millisecond
	})
	session := fixture.openSession(fixture.ticket(nil), testLagOrigin)
	session.dropPongs.Store(true)
	session.hello = session.next("hello", 5*time.Second)

	code, text := session.waitClosed(5 * time.Second)
	if code != lagApiCloseCodeTimeout || text != "timeout" {
		t.Errorf("close %d %q", code, text)
	}
	rtts := session.rttsFor(100 * time.Millisecond)
	if len(rtts) < 3 {
		t.Fatalf("%d rtts", len(rtts))
	}
	for i, rtt := range rtts {
		if !rtt.Lost || rtt.Ms != nil {
			t.Errorf("rtt %+v", rtt.raw)
		}
		if 0 < i {
			// the next goes out when the last is given up, with room for a
			// loaded machine
			if gap := rtt.T - rtts[i-1].T; gap < 295 || 600 < gap {
				t.Errorf("lost gap %d ms", gap)
			}
		}
	}
	body := fixture.nextReport(5 * time.Second)
	if body.EndReason != protocol.LagEndReason_LAG_END_NO_LOAD {
		t.Errorf("end %v", body.EndReason)
	}
	if len(body.PingMicros) != len(rtts) {
		t.Errorf("%d pings for %d rtts", len(body.PingMicros), len(rtts))
	}
	for _, pingMicros := range body.PingMicros {
		if pingMicros != 0 {
			t.Errorf("a lost ping of %d us", pingMicros)
		}
	}
}

// M3, M4: a load past concurrent_loads is refused as busy with a retry, and the
// session goes on pinging; a session that never loads reports busy (M8).
func TestLagApiLoadBusy(t *testing.T) {
	fixture := newLagApiFixture(t, func(settings *ExtenderLagSettings) {
		settings.ConcurrentLoads = 1
	})
	first := fixture.openHelloSession(fixture.ticket(nil))
	second := fixture.openHelloSession(fixture.ticket(nil))

	first.load("download")
	waitFor(t, func() bool {
		return fixture.api.loadCountNow() == 1
	})
	second.load("download")
	message := second.next("error", 5*time.Second)
	if message.Code != lagApiErrorBusy || message.RetryAfterMs == nil || *message.RetryAfterMs != 5000 {
		t.Errorf("busy %+v", message.raw)
	}
	if rtts := second.rttsFor(600 * time.Millisecond); len(rtts) < 2 {
		t.Errorf("%d rtts after busy", len(rtts))
	}
	second.close()
	body := fixture.nextReport(5 * time.Second)
	if body.EndReason != protocol.LagEndReason_LAG_END_BUSY || body.DownloadMs != 0 || body.UploadMs != 0 {
		t.Errorf("busy report %v %d %d", body.EndReason, body.DownloadMs, body.UploadMs)
	}

	// the first session's load times out without streams, freeing its slot
	first.close()
	fixture.nextReport(5 * time.Second)
	if fixture.api.loadCountNow() != 0 {
		t.Errorf("%d loads", fixture.api.loadCountNow())
	}
}

// M3: past the daily budget, loads are refused as budget until the next UTC
// midnight.
func TestLagApiLoadBudget(t *testing.T) {
	// a budget an operator set (decision 18, Q6): a load under way that runs
	// it out is cut and ends as a cap
	const budgetByteCount = 4 * 1024 * 1024
	fixture := newLagApiFixture(t, func(settings *ExtenderLagSettings) {
		settings.DailyBudgetBytes = budgetByteCount
	})
	ticket := func() string {
		return fixture.ticket(func(body *protocol.LagTicketBody) {
			body.DownloadMs = 2000
			body.UploadMs = 2000
		})
	}
	session := fixture.openHelloSession(ticket())
	load := session.runLoad(lagTestLoadOptions{})
	if load.result.EndReason != "cap" {
		t.Errorf("result %+v", load.result.raw)
	}
	if load.downloadedByteCount <= 0 || budgetByteCount < load.downloadedByteCount {
		t.Errorf("downloaded %d under a budget of %d", load.downloadedByteCount, budgetByteCount)
	}
	if !fixture.api.budget.exhausted(time.Now()) {
		t.Error("the budget is not spent")
	}
	session.close()
	if body := fixture.nextReport(5 * time.Second); body.EndReason != protocol.LagEndReason_LAG_END_CAP {
		t.Errorf("report %v", body.EndReason)
	}

	// past it, the next load is refused as budget until the next UTC
	// midnight, and the session goes on pinging
	session = fixture.openHelloSession(ticket())
	session.load("download")
	message := session.next("error", 5*time.Second)
	if message.Code != lagApiErrorBudget || message.RetryAfterMs == nil ||
		*message.RetryAfterMs <= 0 || 24*60*60*1000 < *message.RetryAfterMs {
		t.Errorf("budget %+v", message.raw)
	}
	if rtts := session.rttsFor(600 * time.Millisecond); len(rtts) < 2 {
		t.Errorf("%d rtts after budget", len(rtts))
	}
}

// Decision 18, Q6, M3: with the default settings an extender has no daily
// budget, so whatever the bytes moved, a load is never refused or cut for
// them.
func TestLagApiNoBudgetByDefault(t *testing.T) {
	fixture := newLagApiDurationFixture(t, nil)
	if fixture.api.settings.DailyBudgetBytes != 0 || fixture.api.budget.limit != 0 {
		t.Fatalf("budget %d", fixture.api.budget.limit)
	}
	now := time.Now()
	// far more than any day's loads, already counted today
	const byteCount = int64(1) << 60
	if granted := fixture.api.budget.take(now, byteCount); granted != byteCount {
		t.Errorf("granted %d of %d", granted, byteCount)
	}
	if fixture.api.budget.exhausted(now) {
		t.Error("exhausted with no budget")
	}
	session := fixture.directSession(1)
	if code, _ := fixture.api.beginLoad(now, session); code != "" {
		t.Errorf("refused as %s", code)
	} else {
		fixture.api.endLoad(session)
	}

	// neither direction is cut for its bytes
	download := fixture.directPhase(session, lagApiDirectionDownload, time.Minute, 1<<62)
	if granted := download.reserve(now, lagApiBufferByteCount); granted != lagApiBufferByteCount {
		t.Errorf("reserved %d", granted)
	}
	upload := fixture.directPhase(session, lagApiDirectionUpload, time.Minute, 1<<62)
	upload.window(now)
	upload.receive(0, now, lagApiBufferByteCount)
	for _, phase := range []*lagApiPhase{download, upload} {
		if _, _, end := phase.state(); end != lagApiPhaseRunning {
			t.Errorf("a phase was cut: %d", end)
		}
	}

	// and a whole load runs to its end
	controlSession := fixture.openHelloSession(fixture.ticket(func(body *protocol.LagTicketBody) {
		body.DownloadMs = 300
		body.UploadMs = 300
	}))
	load := controlSession.runLoad(lagTestLoadOptions{uploadByteCount: testLagDurationByteCount})
	if load.result.EndReason != "complete" || !load.result.UploadVerified {
		t.Errorf("result %+v", load.result.raw)
	}
}

// M3, M4, M5, M6, M8: one load end to end -- loadResult complete and verified,
// one load per session, and the report of the minute around the load, sent
// when the minute closes with the session still open.
func TestLagApiLoadAndReport(t *testing.T) {
	fixture := newLagApiDurationFixture(t, nil)
	fixture.setTimings(func(timings *lagApiTimings) {
		timings.reportBefore = 500 * time.Millisecond
		timings.reportAfter = 500 * time.Millisecond
		timings.reportWindow = 5 * time.Second
	})
	ticketText := fixture.ticket(func(body *protocol.LagTicketBody) {
		body.DownloadMs = 500
		body.UploadMs = 500
	})
	session := fixture.openHelloSession(ticketText)
	if session.hello.DownloadMs != 500 || session.hello.UploadMs != 500 {
		t.Fatalf("hello %+v", session.hello.raw)
	}
	time.Sleep(700 * time.Millisecond)

	load := session.runLoad(lagTestLoadOptions{uploadByteCount: testLagDurationByteCount})
	if load.result.Type != "loadResult" || !load.result.UploadVerified || load.result.EndReason != "complete" {
		t.Fatalf("result %+v", load.result.raw)
	}
	if load.downloadedByteCount <= 0 || load.uploadedByteCount <= 0 {
		t.Fatalf("moved %d down %d up", load.downloadedByteCount, load.uploadedByteCount)
	}
	for k, closeCode := range load.uploadCloseCodes {
		// 0 is a stream that connected after the window closed
		if closeCode != websocket.CloseNormalClosure && closeCode != 0 {
			t.Errorf("upload %d closed %d", k, closeCode)
		}
	}
	// the upload's progress: on whole intervals from the first byte, growing,
	// the last at the window's length; then the browser's done; then the result
	// one at 250 ms -- left out when the loop gets to it too late to tell it
	// from the last -- and the last at the window's 500 ms
	if len(load.uploaded) < 1 || 2 < len(load.uploaded) {
		t.Fatalf("%d progress messages", len(load.uploaded))
	}
	lastUploaded := load.uploaded[len(load.uploaded)-1]
	if ms := *lastUploaded.Ms; ms != 500 || lastUploaded.Bytes == 0 {
		t.Errorf("last progress of %d bytes at %f ms", lastUploaded.Bytes, ms)
	}
	if len(load.uploaded) == 2 {
		if ms := *load.uploaded[0].Ms; ms < 250 || 375 <= ms || lastUploaded.Bytes < load.uploaded[0].Bytes {
			t.Errorf("progress of %d bytes at %f ms", load.uploaded[0].Bytes, ms)
		}
	}
	if load.doneTime.IsZero() || load.resultTime.Before(load.doneTime) || 500*time.Millisecond < load.resultTime.Sub(load.doneTime) {
		t.Errorf("result %s after done", load.resultTime.Sub(load.doneTime))
	}

	// one load per session
	session.load("download")
	if message := session.next("error", 5*time.Second); message.Code != lagApiErrorBusy ||
		message.RetryAfterMs == nil || *message.RetryAfterMs != 0 {
		t.Errorf("second load %+v", message.raw)
	}

	// the minute closes with the session open
	body := fixture.nextReport(5 * time.Second)
	if body.EndReason != protocol.LagEndReason_LAG_END_COMPLETE || !body.UploadVerified {
		t.Errorf("report %v %t", body.EndReason, body.UploadVerified)
	}
	if body.DownloadStartMs != 500 || body.DownloadMs != 500 || body.UploadMs != 500 {
		t.Errorf("load at %d for %d, upload for %d", body.DownloadStartMs, body.DownloadMs, body.UploadMs)
	}
	if body.UploadStartMs < body.DownloadStartMs+body.DownloadMs || 5000 <= body.UploadStartMs+body.UploadMs {
		t.Errorf("upload at %d", body.UploadStartMs)
	}
	if body.IntervalMs != 250 || int64(body.DownloadStreams) != load.downloadStreamCount ||
		int64(body.UploadStreams) != load.uploadStreamCount || load.downloadStreamCount < 1 || load.uploadStreamCount < 1 {
		t.Errorf(
			"interval %d streams %d %d, opened %d %d",
			body.IntervalMs, body.DownloadStreams, body.UploadStreams, load.downloadStreamCount, load.uploadStreamCount,
		)
	}
	sum := func(values []uint64) uint64 {
		total := uint64(0)
		for _, value := range values {
			total += value
		}
		return total
	}
	if len(body.DownloadSentBytes) < 1 || lagApiMaxIntervalCount < len(body.DownloadSentBytes) {
		t.Errorf("%d sent intervals", len(body.DownloadSentBytes))
	}
	if sent := sum(body.DownloadSentBytes); sent < uint64(load.downloadedByteCount)*9/10 || uint64(load.downloadedByteCount) < sent*9/10 {
		t.Errorf("sent %d, downloaded %d", sent, load.downloadedByteCount)
	}
	if len(body.DownloadReceivedBytes) == 0 || uint64(load.downloadedByteCount) < sum(body.DownloadReceivedBytes) {
		t.Errorf("received %v of %d", body.DownloadReceivedBytes, load.downloadedByteCount)
	}
	if len(body.UploadBytes) < 1 || 3 < len(body.UploadBytes) || sum(body.UploadBytes) == 0 ||
		uint64(load.uploadedByteCount) < sum(body.UploadBytes) {
		t.Errorf("upload %v of %d", body.UploadBytes, load.uploadedByteCount)
	}
	// what the browser was told is what the report holds
	if sum(body.UploadBytes) != lastUploaded.Bytes {
		t.Errorf("reported %d, told the browser %d", sum(body.UploadBytes), lastUploaded.Bytes)
	}
	load.checkUpload(t, body)
	if string(body.TicketId) == "" || body.TrackPosition != 3 || len(body.TrackId) != 16 {
		t.Errorf("ticket fields %x %d %x", body.TicketId, body.TrackPosition, body.TrackId)
	}
	// the browser's address appears nowhere in the report, only its family
	// (amendment 9)
	assertLagReportCarriesNoAddress(t, body, netip.MustParseAddr("127.0.0.1"))
	if body.IpFamily != 4 {
		t.Errorf("ip family %d", body.IpFamily)
	}
	if body.WindowStartTimeMs+500 < body.SessionStartTimeMs+700 {
		t.Errorf("window at %d, session at %d", body.WindowStartTimeMs, body.SessionStartTimeMs)
	}
	// about 2 s of minute at 4 pings a second
	if len(body.PingOffsetMs) < 5 || len(body.PingOffsetMs) != len(body.PingMicros) {
		t.Fatalf("%d pings, %d rtts", len(body.PingOffsetMs), len(body.PingMicros))
	}
	for i, offset := range body.PingOffsetMs {
		if 5000 <= offset || (0 < i && offset <= body.PingOffsetMs[i-1]) {
			t.Errorf("ping offset %d", offset)
		}
		if body.PingMicros[i] == 0 {
			t.Errorf("ping %d lost", i)
		}
	}
	// the session goes on pinging after its report, with no second report
	if rtts := session.rttsFor(600 * time.Millisecond); len(rtts) < 2 {
		t.Errorf("%d rtts after the report", len(rtts))
	}
	session.close()
	fixture.noReport(500 * time.Millisecond)
}

// M8: a session that closes during its load reports at once, with what it
// has, as the browser closing it; one that closes with no load reports its
// last minute as no load.
func TestLagApiReportOnClose(t *testing.T) {
	fixture := newLagApiFixture(t, nil)
	session := fixture.openHelloSession(fixture.ticket(nil))
	time.Sleep(600 * time.Millisecond)
	session.close()
	body := fixture.nextReport(5 * time.Second)
	if body.EndReason != protocol.LagEndReason_LAG_END_NO_LOAD || body.DownloadMs != 0 {
		t.Errorf("no load report %v", body.EndReason)
	}
	if body.WindowStartTimeMs != body.SessionStartTimeMs || len(body.PingMicros) < 2 {
		t.Errorf("window %d session %d pings %d", body.WindowStartTimeMs, body.SessionStartTimeMs, len(body.PingMicros))
	}

	session = fixture.openHelloSession(fixture.ticket(nil))
	session.load("download")
	waitFor(t, func() bool {
		return fixture.api.loadCountNow() == 1
	})
	response := fixture.get(fmt.Sprintf("/lag/v1/down?session=%s&stream=0", session.hello.Session), testLagOrigin)
	buffer := make([]byte, 64*1024)
	if _, err := io.ReadFull(response.Body, buffer); err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond)
	session.close()
	response.Body.Close()
	body = fixture.nextReport(5 * time.Second)
	if body.EndReason != protocol.LagEndReason_LAG_END_CLIENT_CLOSED || body.DownloadStartMs != 25000 || body.DownloadMs == 0 {
		t.Errorf("closed report %v at %d for %d", body.EndReason, body.DownloadStartMs, body.DownloadMs)
	}
	waitFor(t, func() bool {
		return fixture.api.loadCountNow() == 0
	})
}

// M6: a mismatch ends the load, not the session: loadResult upload_mismatch,
// unverified, the stream closed with 4400, and the session goes on pinging.
func TestLagApiUploadMismatchEndsLoad(t *testing.T) {
	fixture := newLagApiFixture(t, nil)
	session := fixture.openHelloSession(fixture.ticket(func(body *protocol.LagTicketBody) {
		body.DownloadMs = 300
		body.UploadMs = 2000
	}))
	load := session.runLoad(lagTestLoadOptions{
		corrupt: func(streamIndex int, messageIndex int, message []byte) {
			if streamIndex == 2 && messageIndex == 3 {
				message[100] ^= 1
			}
		},
	})
	if load.result.Type != "loadResult" || load.result.UploadVerified || load.result.EndReason != "upload_mismatch" {
		t.Fatalf("result %+v", load.result.raw)
	}
	if load.uploadCloseCodes[2] != lagApiCloseCodeMismatch {
		t.Errorf("upload close codes %v", load.uploadCloseCodes)
	}
	if rtts := session.rttsFor(600 * time.Millisecond); len(rtts) < 2 {
		t.Errorf("%d rtts after the mismatch", len(rtts))
	}
	session.close()
	body := fixture.nextReport(5 * time.Second)
	if body.EndReason != protocol.LagEndReason_LAG_END_UPLOAD_MISMATCH || body.UploadVerified {
		t.Errorf("report %v %t", body.EndReason, body.UploadVerified)
	}
	// what was counted before the mismatch stays in the series, and the
	// result carries it as the report does (amendment 14)
	load.checkUpload(t, body)
}

// M4, amendment 14: an upload whose messages arrive across its window. Each
// progress says, as at, when the latest counted read arrived: it moves on
// with them, never past the progress's ms and never back. The result carries
// the upload's series as the report signs it. It holds every message but
// most of each stream's first: at the rate of the stream's second, that took
// the time before the window opened (amendment 15).
func TestLagApiUploadedAt(t *testing.T) {
	fixture := newLagApiDurationFixture(t, nil)
	session := fixture.openHelloSession(fixture.ticket(func(body *protocol.LagTicketBody) {
		body.MaxStreams = 2
		body.DownloadMs = 250
		body.UploadMs = 1000
	}))
	// each stream sends a message every 80 ms for the window's first 560 ms
	load := session.runLoad(lagTestLoadOptions{
		uploadByteCount: 8 * lagApiBufferByteCount,
		uploadInterval:  80 * time.Millisecond,
	})
	if load.result.EndReason != "complete" || !load.result.UploadVerified || load.uploadStreamCount != 2 {
		t.Fatalf("result %+v from %d streams", load.result.raw, load.uploadStreamCount)
	}
	session.close()
	body := fixture.nextReport(5 * time.Second)
	load.checkUpload(t, body)
	// progress from 250 ms on, and the last at the window's 1000 ms, whose at
	// is the last message's arrival rather than its own time
	if len(load.uploaded) < 2 {
		t.Fatalf("%d progress messages", len(load.uploaded))
	}
	first := load.uploaded[0]
	last := load.uploaded[len(load.uploaded)-1]
	if *last.Ms != 1000 || last.Bytes < 2*6*lagApiBufferByteCount || 2*8*lagApiBufferByteCount < last.Bytes ||
		*last.At <= *first.At || 800 <= *last.At {
		t.Errorf("first progress %+v, last %+v", first.raw, last.raw)
	}
}

// M8, amendment 14: an upload that goes quiet before its window runs out
// keeps the quiet intervals as zeros, ceil(uploadMs / intervalMs) of them in
// all, in the report and the loadResult alike, as the browser's figures count
// them; trimmed, they would leave the signed p50 high.
func TestLagApiUploadKeepsQuietIntervals(t *testing.T) {
	fixture := newLagApiDurationFixture(t, nil)
	session := fixture.openHelloSession(fixture.ticket(func(body *protocol.LagTicketBody) {
		body.MaxStreams = 1
		body.DownloadMs = 250
		body.UploadMs = 1000
	}))
	// two messages at once, then nothing for the rest of the window
	load := session.runLoad(lagTestLoadOptions{uploadByteCount: 2 * lagApiBufferByteCount})
	if load.result.EndReason != "complete" || !load.result.UploadVerified {
		t.Fatalf("result %+v", load.result.raw)
	}
	session.close()
	body := fixture.nextReport(5 * time.Second)
	load.checkUpload(t, body)
	if body.UploadMs != 1000 || len(body.UploadBytes) != 4 || body.UploadBytes[3] != 0 {
		t.Errorf("upload for %d ms: %v", body.UploadMs, body.UploadBytes)
	}
}

// M4, M8, amendment 14: every interval the browser reports is in the signed
// download series, one that brought no new bytes as a zero, so a download
// whose last intervals were quiet keeps them, as the browser's figures count
// them; trimmed, they would leave the signed p50 high.
func TestLagApiReceivedKeepsQuietIntervals(t *testing.T) {
	fixture := newLagApiDurationFixture(t, nil)
	session := fixture.openHelloSession(fixture.ticket(func(body *protocol.LagTicketBody) {
		body.MaxStreams = 1
		body.DownloadMs = 1000
		body.UploadMs = 500
	}))
	session.load("download")
	waitFor(t, func() bool {
		return fixture.api.loadCountNow() == 1
	})
	response := fixture.get(fmt.Sprintf("/lag/v1/down?session=%s&stream=0", session.hello.Session), testLagOrigin)
	downloadedByteCount, _ := io.Copy(io.Discard, response.Body)
	response.Body.Close()
	if downloadedByteCount != testLagDurationByteCount {
		t.Fatalf("downloaded %d", downloadedByteCount)
	}
	step := uint64(downloadedByteCount / 4)
	for _, report := range []struct {
		ms    int64
		bytes uint64
	}{
		{ms: 250, bytes: step},
		{ms: 500, bytes: 2 * step},
		// the last two intervals brought nothing new
		{ms: 750, bytes: 2 * step},
		{ms: 1000, bytes: 2 * step},
	} {
		session.send(map[string]any{"type": "received", "bytes": report.bytes, "ms": report.ms})
	}
	// the messages are read in order, so the close comes after them
	time.Sleep(200 * time.Millisecond)
	session.close()
	body := fixture.nextReport(5 * time.Second)
	if !slices.Equal(body.DownloadReceivedBytes, []uint64{step, step, 0, 0}) {
		t.Errorf("received %v, steps of %d", body.DownloadReceivedBytes, step)
	}
}

// M4: a load whose download never starts times out and frees its slot; the
// browser's done ends a load whose upload never started.
func TestLagApiLoadTimeouts(t *testing.T) {
	fixture := newLagApiFixture(t, nil)
	fixture.setTimings(func(timings *lagApiTimings) {
		timings.phaseWait = 300 * time.Millisecond
	})
	session := fixture.openHelloSession(fixture.ticket(nil))
	session.load("download")
	// with no upload, the result carries no upload series (amendment 14)
	if message := session.nextResult(5 * time.Second); message.Type != "loadResult" ||
		message.EndReason != "timeout" || message.UploadVerified || message.raw["uploadBytes"] != nil {
		t.Errorf("timeout %+v", message.raw)
	}
	if fixture.api.loadCountNow() != 0 {
		t.Errorf("%d loads", fixture.api.loadCountNow())
	}

	session = fixture.openHelloSession(fixture.ticket(nil))
	session.load("download")
	session.load("done")
	if message := session.nextResult(5 * time.Second); message.EndReason != "client_closed" ||
		message.raw["uploadBytes"] != nil {
		t.Errorf("done %+v", message.raw)
	}
}

// M4, S5: after an upload that ran out its window the result follows the
// browser's done at once; a browser that never says done gets it after the
// done wait, and in either case the load's slot is free as soon as the window
// closes.
func TestLagApiLoadResultFollowsDone(t *testing.T) {
	fixture := newLagApiDurationFixture(t, func(settings *ExtenderLagSettings) {
		settings.ConcurrentLoads = 1
	})
	fixture.setTimings(func(timings *lagApiTimings) {
		timings.doneWait = 2 * time.Second
	})
	ticket := func() string {
		return fixture.ticket(func(body *protocol.LagTicketBody) {
			body.DownloadMs = 250
			body.UploadMs = 500
		})
	}
	loadCounts := make(chan int, 2)
	options := lagTestLoadOptions{
		uploadByteCount: testLagDurationByteCount,
		windowClosed: func() {
			loadCounts <- fixture.api.loadCountNow()
		},
	}

	session := fixture.openHelloSession(ticket())
	load := session.runLoad(options)
	if load.result.EndReason != "complete" || !load.result.UploadVerified {
		t.Errorf("result %+v", load.result.raw)
	}
	if wait := load.resultTime.Sub(load.doneTime); time.Second < wait {
		t.Errorf("result %s after done", wait)
	}
	session.close()
	fixture.nextReport(5 * time.Second)

	session = fixture.openHelloSession(ticket())
	options.noDone = true
	load = session.runLoad(options)
	if load.result.EndReason != "complete" || !load.result.UploadVerified {
		t.Errorf("result without done %+v", load.result.raw)
	}
	if wait := load.resultTime.Sub(load.windowClosedTime); wait < 1900*time.Millisecond || 5*time.Second < wait {
		t.Errorf("result %s after the window closed, with no done", wait)
	}
	for i := 0; i < 2; i += 1 {
		if loadCount := <-loadCounts; loadCount != 0 {
			t.Errorf("%d loads when the window closed", loadCount)
		}
	}
	session.close()
	fixture.nextReport(5 * time.Second)

	// a browser that says done with the upload's window still open -- its
	// client does after 2 s with no word that the window closed (S5) -- does
	// not cut the window: it runs out, and the result follows at once
	session = fixture.openHelloSession(fixture.ticket(func(body *protocol.LagTicketBody) {
		body.DownloadMs = 250
		body.UploadMs = 750
	}))
	load = session.runLoad(lagTestLoadOptions{earlyDone: true, uploadByteCount: testLagDurationByteCount})
	if load.result.EndReason != "complete" || !load.result.UploadVerified {
		t.Errorf("result after an early done %+v", load.result.raw)
	}
	if last := load.uploaded[len(load.uploaded)-1]; *last.Ms != 750 {
		t.Errorf("last progress at %f ms", *last.Ms)
	}
	// done went with the first progress; when that came well before the
	// window's end, the result waited for the end rather than following done
	if first := load.uploaded[0]; *first.Ms <= 500 {
		if wait := load.resultTime.Sub(load.doneTime); wait < 150*time.Millisecond {
			t.Errorf("result %s after a done at %f ms", wait, *first.Ms)
		}
	}
	session.close()
	if body := fixture.nextReport(5 * time.Second); body.UploadMs != 750 || body.EndReason != protocol.LagEndReason_LAG_END_COMPLETE {
		t.Errorf("upload ran %d ms, ended %v", body.UploadMs, body.EndReason)
	}
}

// Runs a session's download on one stream and opens its upload on one stream,
// which sends two messages and is then never read: the extender's close at
// the window's end goes unanswered, so the stream stays open, with no read
// past the end, and the window is final lagSpreadMax past its end (amendment
// 15). Returns when the stream began to send, no later than the window's first
// byte.
func (self *lagTestSession) openSilentUpload() time.Time {
	self.t.Helper()
	fixture := self.fixture
	self.load("download")
	waitFor(self.t, func() bool {
		return fixture.api.loadCountNow() == 1
	})
	response := fixture.get(fmt.Sprintf("/lag/v1/down?session=%s&stream=0", self.hello.Session), testLagOrigin)
	io.Copy(io.Discard, response.Body)
	response.Body.Close()
	self.load("upload")
	conn, _, err := fixture.dialWebSocket(fmt.Sprintf("/lag/v1/up?session=%s&stream=0", self.hello.Session), testLagOrigin)
	if err != nil {
		self.t.Fatal(err)
	}
	self.t.Cleanup(func() {
		conn.Close()
	})
	uploadKey := [16]byte{}
	copy(uploadKey[:], self.uploadKey())
	sendTime := time.Now()
	for _, message := range testLagUploadMessages(self.t, uploadKey, 0, lagApiBufferByteCount, lagApiBufferByteCount) {
		conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
		if err := conn.WriteMessage(websocket.BinaryMessage, message); err != nil {
			self.t.Fatal(err)
		}
	}
	return sendTime
}

// Waits for the session's upload to have ended by its time, at its window's
// end, and returns the phase.
func (self *lagTestSession) waitUploadEnded() *lagApiPhase {
	self.t.Helper()
	sessionId, ok := parseLagApiSessionId(self.hello.Session)
	if !ok {
		self.t.Fatalf("bad session %q", self.hello.Session)
	}
	var upload *lagApiPhase
	waitFor(self.t, func() bool {
		session := self.fixture.api.session(sessionId)
		if session == nil {
			return false
		}
		session.stateLock.Lock()
		if session.load != nil {
			upload = session.load.upload
		}
		session.stateLock.Unlock()
		if upload == nil {
			return false
		}
		_, _, end := upload.state()
		return end == lagApiPhaseEndTime
	})
	return upload
}

// M4, amendment 15: the progress that says the upload's window is over goes
// out once the window is final, with the final total, which the result's
// series and the report's sum to. Here a stream stays open with no read past
// the end, so that is lagSpreadMax past the end. A done between the end and
// then waits for it, rather than ending the load as given up, and the result
// follows the last progress at once, complete. The window's end is still the
// phase's end for everything else: the last progress's ms, the report's
// UploadMs and the series' intervals.
func TestLagApiUploadWindowOverWhenFinal(t *testing.T) {
	fixture := newLagApiDurationFixture(t, nil)
	session := fixture.openHelloSession(fixture.ticket(func(body *protocol.LagTicketBody) {
		body.MaxStreams = 1
		body.DownloadMs = 250
		body.UploadMs = 500
	}))
	sendTime := session.openSilentUpload()
	// done once the window has run out, before it is final
	if upload := session.waitUploadEnded(); upload.windowFinal() {
		t.Fatal("final at the window's end")
	}
	session.load("done")
	doneTime := time.Now()

	uploaded := []testLagMessage{}
	var windowOverTime time.Time
	var result testLagMessage
	var resultTime time.Time
	deadline := time.After(10 * time.Second)
	for result.Type == "" {
		select {
		case message := <-session.messages:
			switch message.Type {
			case "uploaded":
				uploaded = append(uploaded, message)
				if 500-125 <= *message.Ms && windowOverTime.IsZero() {
					windowOverTime = time.Now()
				}
			case "loadResult":
				result = message
				resultTime = time.Now()
			default:
				t.Fatalf("unexpected %s %+v", message.Type, message.raw)
			}
		case <-deadline:
			t.Fatal("no loadResult")
		}
	}
	if windowOverTime.IsZero() || *uploaded[len(uploaded)-1].Ms != 500 {
		t.Fatalf("progress %v", uploaded)
	}
	// only the last progress reads as over
	for i, message := range uploaded[:len(uploaded)-1] {
		if 500-125 <= *message.Ms {
			t.Errorf("progress %d %+v read as over before the window was final", i, message.raw)
		}
	}
	// the window opened no sooner than the stream began to send, and was
	// final lagSpreadMax past its end
	if wait := windowOverTime.Sub(sendTime); wait < 500*time.Millisecond+lagSpreadMax {
		t.Errorf("the window was over %s after the upload began to send", wait)
	}
	if !doneTime.Before(windowOverTime) {
		t.Errorf("done went %s after the window was over", doneTime.Sub(windowOverTime))
	}
	if result.EndReason != "complete" || !result.UploadVerified || resultTime.Sub(windowOverTime) > 500*time.Millisecond {
		t.Errorf("result %+v, %s after the window was over", result.raw, resultTime.Sub(windowOverTime))
	}
	session.close()
	body := fixture.nextReport(5 * time.Second)
	load := &lagTestLoad{uploaded: uploaded, result: result}
	load.checkUpload(t, body)
	if body.EndReason != protocol.LagEndReason_LAG_END_COMPLETE || body.UploadMs != 500 || len(body.UploadBytes) != 2 {
		t.Errorf("report %v, upload for %d ms: %v", body.EndReason, body.UploadMs, body.UploadBytes)
	}
}

// M8, amendment 15: a session that closes after its upload's window ran out,
// but before the window is final, ends a load that is complete, as it would
// have been at the end: the session's end makes the window final, with what
// it has, and the report is the window's.
func TestLagApiSessionEndsBeforeTheWindowIsFinal(t *testing.T) {
	fixture := newLagApiDurationFixture(t, nil)
	session := fixture.openHelloSession(fixture.ticket(func(body *protocol.LagTicketBody) {
		body.MaxStreams = 1
		body.DownloadMs = 250
		body.UploadMs = 500
	}))
	session.openSilentUpload()
	if upload := session.waitUploadEnded(); upload.windowFinal() {
		t.Fatal("final at the window's end")
	}
	session.close()
	body := fixture.nextReport(5 * time.Second)
	if body.EndReason != protocol.LagEndReason_LAG_END_COMPLETE || !body.UploadVerified || body.UploadMs != 500 ||
		len(body.UploadBytes) != 2 || body.UploadBytes[0] == 0 {
		t.Errorf("report %v verified %t, upload for %d ms: %v", body.EndReason, body.UploadVerified, body.UploadMs, body.UploadBytes)
	}
	waitFor(t, func() bool {
		return fixture.api.loadCountNow() == 0
	})
}

// M5, M6, S5: a stream can overtake the control message that opens its phase;
// it waits for the message rather than being refused.
func TestLagApiStreamBeforeLoadMessage(t *testing.T) {
	fixture := newLagApiFixture(t, nil)
	session := fixture.openHelloSession(fixture.ticket(func(body *protocol.LagTicketBody) {
		body.DownloadMs = 300
		body.UploadMs = 300
	}))

	downloaded := make(chan string, 1)
	go func() {
		request, _ := http.NewRequest(
			http.MethodGet,
			fmt.Sprintf("https://%s/lag/v1/down?session=%s&stream=0", testLagName, session.hello.Session),
			nil,
		)
		request.Header.Set("Origin", testLagOrigin)
		response, err := fixture.httpClient().Do(request)
		if err != nil {
			downloaded <- err.Error()
			return
		}
		n, _ := io.Copy(io.Discard, response.Body)
		response.Body.Close()
		downloaded <- fmt.Sprintf("%d %t", response.StatusCode, 0 < n)
	}()
	time.Sleep(300 * time.Millisecond)
	session.load("download")
	select {
	case outcome := <-downloaded:
		if outcome != "200 true" {
			t.Errorf("download ahead of its message: %s", outcome)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the download did not end")
	}

	upgraded := make(chan error, 1)
	go func() {
		conn, _, err := fixture.dialWebSocket(
			fmt.Sprintf("/lag/v1/up?session=%s&stream=0", session.hello.Session),
			testLagOrigin,
		)
		if err == nil {
			conn.Close()
		}
		upgraded <- err
	}()
	time.Sleep(300 * time.Millisecond)
	session.load("upload")
	select {
	case err := <-upgraded:
		if err != nil {
			t.Errorf("upload ahead of its message: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the upload did not open")
	}
}

// C4, C9: once the certificate stops naming a session's name, the session
// closes as its dot going away -- 1001, with no error of the session's own --
// and a load it is running ends with it, as an error. That is so whether the
// lifecycle withdrew the certificate and its change monitor said so, the
// owner opted out in a way only a fresh read of the certificate shows, or the
// certificate now names another name. The name is then not answered even on a
// connection opened before. A renewal of the same name closes nothing.
func TestLagApiUnservedNameClosesSessions(t *testing.T) {
	fixture := newLagApiFixture(t, nil)
	fixture.setTimings(func(timings *lagApiTimings) {
		timings.certificateCheck = 300 * time.Millisecond
	})
	// a preflight succeeds, so its connection is kept for the next request
	client := fixture.httpClient()
	get := func() int {
		request, _ := http.NewRequest(http.MethodOptions, "https://"+testLagName+"/lag/v1/down", nil)
		request.Header.Set("Origin", testLagOrigin)
		request.Header.Set("Access-Control-Request-Method", "GET")
		response, err := client.Do(request)
		if err != nil {
			return 0
		}
		io.Copy(io.Discard, response.Body)
		response.Body.Close()
		return response.StatusCode
	}

	// a renewal: the same name, a new certificate
	session := fixture.openHelloSession(fixture.ticket(nil))
	renewed, err := selfSignedCertificate([]string{testLagName}, "Lag", time.Hour, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	fixture.certificates.set([]string{testLagName}, renewed)
	if rtts := session.rttsFor(900 * time.Millisecond); len(rtts) < 2 {
		t.Errorf("%d rtts after a renewal", len(rtts))
	}
	// and back to the certificate the clients trust, the same name again
	fixture.certificates.set([]string{testLagName}, fixture.certificate)
	if rtts := session.rttsFor(600 * time.Millisecond); len(rtts) < 1 {
		t.Errorf("%d rtts after a second renewal", len(rtts))
	}
	select {
	case <-session.done:
		t.Fatal("a renewal closed the session")
	default:
	}

	// withdrawn, with a load running
	session.load("download")
	waitFor(t, func() bool {
		return fixture.api.loadCountNow() == 1
	})
	download := fixture.get(fmt.Sprintf("/lag/v1/down?session=%s&stream=0", session.hello.Session), testLagOrigin)
	if _, err := io.ReadFull(download.Body, make([]byte, 64*1024)); err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond)
	// a connection opened before, kept idle
	if status := get(); status != http.StatusNoContent {
		t.Fatalf("before: %d", status)
	}
	withdrawnTime := time.Now()
	fixture.certificates.set(nil, nil)
	code, text := session.waitClosed(5 * time.Second)
	if code != lagApiCloseCodeAway || text != "withdrawn" {
		t.Errorf("closed %d %q", code, text)
	}
	// the monitor said so: no wait for the check
	if wait := time.Since(withdrawnTime); 250*time.Millisecond < wait {
		t.Errorf("closed %s after the withdrawal", wait)
	}
	select {
	case message := <-session.messages:
		t.Errorf("a message with the close: %+v", message.raw)
	default:
	}
	body := fixture.nextReport(5 * time.Second)
	if body.EndReason != protocol.LagEndReason_LAG_END_ERROR || body.DownloadMs == 0 {
		t.Errorf("report %v, download for %d ms", body.EndReason, body.DownloadMs)
	}
	io.Copy(io.Discard, download.Body)
	download.Body.Close()
	waitFor(t, func() bool {
		return fixture.api.loadCountNow() == 0
	})
	// not answered on the connection opened before, nor on a new one
	if status := get(); status != http.StatusMisdirectedRequest {
		t.Errorf("after: %d", status)
	}
	if _, err := tls.Dial("tcp", fixture.dialAddress(), &tls.Config{RootCAs: fixture.rootCas, ServerName: testLagName}); err == nil {
		t.Error("a withdrawn name was answered")
	}

	// opted out in a way only a fresh read shows: the check closes it
	fixture.certificates.set([]string{testLagName}, fixture.certificate)
	session = fixture.openHelloSession(fixture.ticket(nil))
	fixture.certificates.setQuietly(nil, nil)
	if code, text := session.waitClosed(3 * time.Second); code != lagApiCloseCodeAway || text != "withdrawn" {
		t.Errorf("opted out: closed %d %q", code, text)
	}
	if body := fixture.nextReport(5 * time.Second); body.EndReason != protocol.LagEndReason_LAG_END_NO_LOAD {
		t.Errorf("opted out: report %v", body.EndReason)
	}

	// another name: the sessions of the old one close
	fixture.certificates.set([]string{testLagName}, fixture.certificate)
	session = fixture.openHelloSession(fixture.ticket(nil))
	otherName := "0192e4a1-1111-7000-8000-000000000001.extender.example"
	other, err := selfSignedCertificate([]string{otherName}, "Lag", time.Hour, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	fixture.certificates.set([]string{otherName}, other)
	if code, _ := session.waitClosed(3 * time.Second); code != lagApiCloseCodeAway {
		t.Errorf("another name: closed %d", code)
	}
	fixture.nextReport(5 * time.Second)
}

// waitFor polls a condition for a few seconds.
func waitFor(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !condition() {
		if deadline.Before(time.Now()) {
			t.Fatal("condition not met")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// The loads running now, for tests.
func (self *extenderLagApi) loadCountNow() int {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.loadCount
}

// Serves the fixture's extender on a loopback tcp carrier, its accept path the
// extender's own (M1). Called before any connection, so changing the server's
// carrier and settings here races nothing.
func (self *lagApiFixture) serveExtender(configure func(settings *ExtenderSettings)) string {
	self.t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		self.t.Fatal(err)
	}
	self.t.Cleanup(func() { listener.Close() })
	port := listener.Addr().(*net.TCPAddr).Port
	self.server.ports = map[int][]connect.ExtenderConnectMode{
		port: {connect.ExtenderConnectModeTcpTls},
	}
	self.server.settings.Listen = func(network string, address string) (net.Listener, error) {
		return listener, nil
	}
	if configure != nil {
		configure(self.server.settings)
	}
	serveDone := make(chan error, 1)
	go func() {
		serveDone <- self.server.ListenAndServe()
	}()
	self.t.Cleanup(func() {
		self.server.CloseAndWait()
		select {
		case <-serveDone:
		case <-time.After(5 * time.Second):
			self.t.Error("the extender did not stop")
		}
	})
	select {
	case <-self.server.Listening():
	case <-time.After(5 * time.Second):
		self.t.Fatal("the extender did not listen")
	}
	return listener.Addr().String()
}

// M1 through the extender's own accept path, with camouflage off and on: a
// hello for a measurement name reaches the API with the measurement
// certificate and http/1.1 alone; another name takes the existing path
// unchanged, an identity-ca leaf for the name with h2 still offered; a
// measurement name with no certificate is closed, and no carrier answers one
// with an identity leaf.
func TestLagApiDispatchThroughExtender(t *testing.T) {
	for _, camouflageEnabled := range []bool{false, true} {
		t.Run(fmt.Sprintf("camouflage %t", camouflageEnabled), func(t *testing.T) {
			fixture := newLagApiFixture(t, nil)
			fixture.server.measurementZone = "extender.example"
			// every name an identity leaf is selected for
			serverNames := make(chan string, 16)
			fixture.server.certificates.certificateHandler = func(serverName string) {
				serverNames <- serverName
			}
			address := fixture.serveExtender(func(settings *ExtenderSettings) {
				settings.CamouflageEnabled = camouflageEnabled
			})
			dial := func(serverName string, rootCas bool) (*tls.Conn, error) {
				config := &tls.Config{
					ServerName: serverName,
					NextProtos: []string{"h2", "http/1.1"},
				}
				if rootCas {
					config.RootCAs = fixture.rootCas
				} else {
					config.InsecureSkipVerify = true
				}
				return tls.DialWithDialer(&net.Dialer{Timeout: 5 * time.Second}, "tcp", address, config)
			}

			conn, err := dial(testLagName, true)
			if err != nil {
				t.Fatal(err)
			}
			state := conn.ConnectionState()
			if state.NegotiatedProtocol != "http/1.1" || !state.PeerCertificates[0].Equal(fixture.certificate.Leaf) {
				t.Errorf("measurement name: alpn %q", state.NegotiatedProtocol)
			}
			request, _ := http.NewRequest(http.MethodGet, "https://"+testLagName+"/lag/v1/down?session=AAAAAAAAAAAAAAAAAAAAAA&stream=0", nil)
			request.Header.Set("Origin", testLagOrigin)
			if err := request.Write(conn); err != nil {
				t.Fatal(err)
			}
			conn.SetReadDeadline(time.Now().Add(5 * time.Second))
			response, err := http.ReadResponse(bufio.NewReader(conn), request)
			if err != nil {
				t.Fatal(err)
			}
			response.Body.Close()
			conn.Close()
			if response.StatusCode != http.StatusNotFound || response.Header.Get("Cache-Control") != "no-store" ||
				response.Header.Get("Access-Control-Allow-Origin") != testLagOrigin {
				t.Errorf("measurement api: %d %v", response.StatusCode, response.Header)
			}

			camouflageStats := fixture.server.CamouflageStats()
			conn, err = dial("other.example", false)
			if err != nil {
				t.Fatal(err)
			}
			state = conn.ConnectionState()
			conn.Close()
			leaf := state.PeerCertificates[0]
			if state.NegotiatedProtocol != "h2" || len(leaf.DNSNames) != 1 || leaf.DNSNames[0] != "other.example" ||
				leaf.Issuer.CommonName != "Extender Root" {
				t.Errorf("other name: alpn %q leaf %v issuer %q", state.NegotiatedProtocol, leaf.DNSNames, leaf.Issuer.CommonName)
			}
			select {
			case serverName := <-serverNames:
				if serverName != "other.example" {
					t.Errorf("certificate for %q", serverName)
				}
			case <-time.After(5 * time.Second):
				t.Error("no identity leaf was selected")
			}
			terminatedCount := fixture.server.CamouflageStats().TerminatedCount - camouflageStats.TerminatedCount
			if (camouflageEnabled && terminatedCount != 1) || (!camouflageEnabled && terminatedCount != 0) {
				t.Errorf("terminated %d", terminatedCount)
			}

			// a name under the zone with no certificate for it
			if _, err := dial("0192e4a1-0000-7000-8000-000000000009.extender.example", false); err == nil {
				t.Error("a measurement name with no certificate was answered")
			}
			select {
			case serverName := <-serverNames:
				t.Errorf("an identity leaf for %q", serverName)
			default:
			}
			for _, serverName := range []string{testLagName, "0192e4a1-0000-7000-8000-000000000009.extender.example", "0192E4A1-0000-7000-8000-000000000009.EXTENDER.example"} {
				if _, err := fixture.server.carrierCertificate(&tls.ClientHelloInfo{ServerName: serverName}); err == nil {
					t.Errorf("an identity leaf for %q", serverName)
				}
			}
			if _, err := fixture.server.carrierCertificate(&tls.ClientHelloInfo{ServerName: "extender.example.net"}); err != nil {
				t.Errorf("another name: %v", err)
			}
		})
	}
}

// A report names no client address (lag.dev/PLAN.md M8, amendment 9): its
// encoding holds no field 5, where ClientIp was, and no unknown field, and
// neither the peer address's bytes nor its text appear anywhere in it.
func assertLagReportCarriesNoAddress(t *testing.T, body *protocol.LagReportBody, peer netip.Addr) {
	t.Helper()
	if unknown := body.ProtoReflect().GetUnknown(); len(unknown) != 0 {
		t.Errorf("the report holds %d bytes of unknown fields", len(unknown))
	}
	bodyBytes, err := proto.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	for b := bodyBytes; 0 < len(b); {
		number, _, n := protowire.ConsumeField(b)
		if n < 0 {
			t.Fatalf("the report does not parse: %v", protowire.ParseError(n))
		}
		if number == 5 {
			t.Errorf("the report holds field 5, where the client address was")
		}
		b = b[n:]
	}
	forms := [][]byte{
		peer.AsSlice(),
		[]byte(peer.String()),
	}
	if peer.Is4() {
		forms = append(forms, netip.AddrFrom16(peer.As16()).AsSlice())
	}
	for _, form := range forms {
		if bytesContain(bodyBytes, form) {
			t.Errorf("the report holds the peer address as %x", form)
		}
	}
}

func bytesContain(b []byte, sub []byte) bool {
	for i := 0; i+len(sub) <= len(b); i++ {
		if string(b[i:i+len(sub)]) == string(sub) {
			return true
		}
	}
	return false
}

// A report names the browser's address family and nothing more of its
// address (amendment 9): a v4-mapped address counts as 4.
func TestLagApiIpFamily(t *testing.T) {
	for remoteAddr, family := range map[string]uint32{
		"203.0.113.77:51000":          4,
		"[::ffff:203.0.113.77]:51000": 4,
		"[2001:db8::77]:51000":        6,
		"not an address":              0,
	} {
		if got := lagApiIpFamily(remoteAddr); got != family {
			t.Errorf("%q: family %d, want %d", remoteAddr, got, family)
		}
	}
}

// One 100-Mb/s, 300-microsecond-added-RTT, one-flow download smoke uses real
// TLS/WebSocket H1, an owned TCP origin and a userspace gVisor TUN. Transfer
// encryption is disabled. The synthetic relay has no server authentication;
// native TUN, provider admission policy and database server tiers are absent.
package connect

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"google.golang.org/protobuf/encoding/protowire"
)

// Wire counters belong to one relay direction and contain no message contents.
type windowPhysicalH1Counters struct {
	messages    atomic.Int64
	bytes       atomic.Int64
	ackMessages atomic.Int64
	ackBytes    atomic.Int64
}

// A common-interval Transfer envelope reading, excluding TLS/TCP headers.
type windowPhysicalH1Wire struct {
	Messages    int64
	Bytes       int64
	AckMessages int64
	AckBytes    int64
}

// Read independent monotonic counters without inspecting application contents.
func (self *windowPhysicalH1Counters) snapshot() windowPhysicalH1Wire {
	return windowPhysicalH1Wire{Messages: self.messages.Load(), Bytes: self.bytes.Load(), AckMessages: self.ackMessages.Load(), AckBytes: self.ackBytes.Load()}
}

// Classify just the protobuf envelope; decoding Packs would acquire pooled
// message ownership and perturb the carrier measurement unnecessarily.
func (self *windowPhysicalH1Counters) observe(message []byte) bool {
	ack := false
	for data := message; len(data) > 0; {
		number, kind, size := protowire.ConsumeTag(data)
		if size < 0 {
			return false
		}
		data = data[size:]
		size = protowire.ConsumeFieldValue(number, kind, data)
		if size < 0 {
			return false
		}
		if number == 5 && kind == protowire.BytesType {
			ack = true
		}
		data = data[size:]
	}
	self.messages.Add(1)
	self.bytes.Add(int64(len(message)))
	if ack {
		self.ackMessages.Add(1)
		self.ackBytes.Add(int64(len(message)))
	}
	return true
}

// Each queued frame retains its original serialization deadline. Delayed host
// wakeups cannot charge the same queued work a second serialization delay.
// The hard queue covers propagation too; reader and writer each own at most
// one additional 8-KiB message outside this queue.
func runWindowPhysicalH1Relay(ctx context.Context, from, to Route, rate ByteCount, delay time.Duration, highCount, highBytes *atomic.Int64) {
	const countLimit = 512
	const byteLimit = 2 * 1024 * 1024
	const messageLimit = 8 * 1024
	var queue []windowPathFrame
	var ownedBytes int64
	departure := time.Now()
	timer := time.NewTimer(0)
	defer timer.Stop()
	defer func() {
		for _, frame := range queue {
			MessagePoolReturn(frame.bytes)
		}
	}()
	for {
		var output Route
		var ready []byte
		var timeout <-chan time.Time
		if len(queue) > 0 {
			if remaining := time.Until(queue[0].arrive); remaining <= 0 {
				output, ready = to, queue[0].bytes
			} else {
				timer.Reset(remaining)
				timeout = timer.C
			}
		}
		input := from
		if len(queue) >= countLimit || ownedBytes > byteLimit-messageLimit {
			input = nil
		}
		select {
		case <-ctx.Done():
			return
		case <-timeout:
		case output <- ready:
			ownedBytes -= int64(len(ready))
			copy(queue, queue[1:])
			queue[len(queue)-1] = windowPathFrame{}
			queue = queue[:len(queue)-1]
		case message := <-input:
			now := time.Now()
			if departure.Before(now) {
				departure = now
			}
			departure = departure.Add(time.Duration(int64(len(message)) * int64(time.Second) / int64(rate)))
			queue = append(queue, windowPathFrame{bytes: message, depart: departure, arrive: departure.Add(delay)})
			ownedBytes += int64(len(message))
			highCount.Store(max(highCount.Load(), int64(len(queue))))
			highBytes.Store(max(highBytes.Load(), ownedBytes))
		}
	}
}

// Resolved provider-side constructor limits for the owned actual origin.
type windowPhysicalH1Nat struct {
	ProviderTarget      ByteCount
	ReturnBudget        ByteCount
	Mtu                 int
	ReadBufferBytes     int
	WriteBatchCount     int
	SequenceBufferCount int
	MaximumWindowBytes  uint32
}

// Retain the cell, both directions, calibration and teardown accounting.
type windowPhysicalH1Reading struct {
	Arm                      string
	ProfileSha256            string
	Profiles                 [2]windowPathEndpointProfile
	Rate                     ByteCount
	AddedRoundTrip           time.Duration
	WarmupSeconds            float64
	Seconds                  float64
	DownloadMbps             float64
	IntervalMbps             []float64
	Wire                     [2]windowPhysicalH1Wire
	CarrierQueueCount        [2]int
	CarrierAckReserve        [2]int
	CarrierMessageLimit      [2]int64
	CarrierBudget            [2]PlatformTransportBudgetStats
	CarrierAfterClose        [2]PlatformTransportBudgetStats
	CarrierReceive           [2]PlatformTransportReceiveStatsSnapshot
	Receiver                 [2]ClientReceiveStatsSnapshot
	Recovery                 [2]ClientSendRecoveryStatsSnapshot
	Window                   [2]SendWindowEstimate
	Policy                   [2]windowPhysicalH1PolicyReading
	MaxRelayQueued           [2]int64
	MaxRelayQueuedBytes      [2]int64
	Nat                      windowPhysicalH1Nat
	NatRefused               int64
	Connections              [2]int64
	TransferBudgetAfterClose [2][3]ByteCount
}

// Each arm owns its clients, carrier sockets, finite relay, origin and gVisor
// socket. All workers join before accounting is reconciled and the next arm.
func measureWindowPhysicalH1(t *testing.T, arm, digest string, profiles [2]windowPathEndpointProfile) (reading windowPhysicalH1Reading) {
	t.Helper()
	const rate = ByteCount(12500000)
	const roundTrip = 300 * time.Microsecond
	const warmup = 2500 * time.Millisecond
	const duration = 5 * time.Second
	ctx, cancel := context.WithCancel(context.Background())
	reading = windowPhysicalH1Reading{Arm: arm, ProfileSha256: digest, Profiles: profiles, Rate: rate, AddedRoundTrip: roundTrip, WarmupSeconds: warmup.Seconds()}
	var clients [2]*Client
	var settings [2]*ClientSettings
	var transports [2]*PlatformTransport
	var transportSettings [2]*PlatformTransportSettings
	var strategies [2]*ClientStrategy
	var sockets [2]*websocket.Conn
	var connections [2]atomic.Int64
	var counters [2]windowPhysicalH1Counters
	var highCount, highBytes [2]atomic.Int64
	var workers sync.WaitGroup
	input := [2]Route{make(Route), make(Route)}
	output := [2]Route{make(Route), make(Route)}
	accepted := [2]chan *websocket.Conn{make(chan *websocket.Conn, 1), make(chan *websocket.Conn, 1)}
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	server := newTestingLoopbackHttpServer(t, 4, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		index := 0
		if r.URL.Path == "/device" {
			index = 1
		} else if r.URL.Path != "/provider" {
			http.NotFound(w, r)
			return
		}
		ws, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer ws.Close()
		if connections[index].Add(1) != 1 {
			t.Errorf("%s H1 carrier reconnected", arm)
			return
		}
		ws.SetReadLimit(8 * 1024)
		select {
		case accepted[index] <- ws:
		case <-ctx.Done():
			return
		}
		<-ctx.Done()
	}), true)
	cleanupWorkload := func() {}
	defer func() {
		cancel()
		for _, ws := range sockets {
			if ws != nil {
				ws.Close()
			}
		}
		cleanupWorkload()
		closeCtx, closeCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer closeCancel()
		for _, transport := range transports {
			if transport != nil {
				if err := transport.CloseAndWait(closeCtx); err != nil {
					t.Errorf("H1 close: %v", err)
				}
			}
		}
		for _, client := range clients {
			if client != nil {
				if err := client.CloseAndWait(closeCtx); err != nil {
					t.Errorf("client close: %v", err)
				}
			}
		}
		workers.Wait()
		for _, strategy := range strategies {
			if strategy != nil {
				strategy.Close()
			}
		}
		server.Close()
		for index := range settings {
			if transportSettings[index] != nil {
				reading.CarrierAfterClose[index] = transportSettings[index].PlatformTransportBudget.Stats()
				stats := reading.CarrierAfterClose[index]
				if stats.UsedByteCount != 0 || stats.UsedTransportCount != 0 || stats.PendingH1Count != 0 || stats.ReservedByteCount != stats.ReleasedByteCount {
					t.Errorf("%s carrier budget did not balance: %+v", arm, stats)
				}
			}
			if settings[index] == nil {
				continue
			}
			for j, budget := range []*TransferMemoryBudget{settings[index].SendBufferSettings.ResendQueueBudget, settings[index].ReceiveBufferSettings.ReceiveQueueBudget, settings[index].ReceiveBufferSettings.PackQueueBudget} {
				reading.TransferBudgetAfterClose[index][j] = budget.UsedByteCount()
				reserved, released := budget.Counts()
				if budget.UsedByteCount() != 0 || reserved != released {
					t.Errorf("%s transfer budget retained bytes: used=%d reserved=%d released=%d", arm, budget.UsedByteCount(), reserved, released)
				}
			}
		}
	}()
	for index, profile := range profiles {
		s := DefaultClientSettings()
		s.Log = NewNoopLogger()
		s.EncryptionSettings.Mode = EncryptionModeOff
		s.beforeClientKeyPublishForTest = func() { <-ctx.Done() }
		s.SendBufferSettings.WindowSizing = WindowSizingConstant
		if arm == "delivery" {
			s.SendBufferSettings.WindowSizing = WindowSizingFromDelivery
		}
		s.SendBufferSettings.ApplyWindowSizing()
		profile.apply(s)
		if arm != "delivery" {
			s.SendBufferSettings.ResendQueueMaxByteCount = profile.windowLimit(&profiles[1-index])
		}
		settings[index] = s
		clients[index] = NewClient(ctx, NewId(), NewNoContractClientOob(), s)
	}
	clients[0].ContractManager().AddNoContractPeer(clients[1].ClientId())
	clients[1].ContractManager().AddNoContractPeer(clients[0].ClientId())
	for index, profile := range profiles {
		strategySettings := DefaultClientStrategySettings()
		strategySettings.Log = NewNoopLogger()
		strategySettings.EnableResilient = false
		strategySettings.TlsConfig = server.Client().Transport.(*http.Transport).TLSClientConfig.Clone()
		strategies[index] = NewClientStrategy(ctx, strategySettings)
		carrier := DefaultPlatformTransportSettingsWithMemoryTarget(profile.DeviceTarget)
		carrier.Log = NewNoopLogger()
		carrier.PingTimeout = 30 * time.Second
		// SDK provider construction leaves this at zero. Only outbound
		// mobile window construction installs its eight-slot ACK reserve.
		if index == 1 {
			carrier.H1AckPriorityBufferSize = 8
		}
		transportSettings[index] = carrier
		reading.CarrierQueueCount[index] = carrier.TransportBufferSize
		reading.CarrierAckReserve[index] = carrier.H1AckPriorityBufferSize
		reading.CarrierMessageLimit[index] = carrier.H1MaxMessageByteCount
		path := "/provider"
		if index == 1 {
			path = "/device"
		}
		transports[index] = NewPlatformTransportWithTargetMode(ctx, strategies[index], clients[index].RouteManager(), "wss"+strings.TrimPrefix(server.URL, "https")+path, &ClientAuth{ByJwt: "synthetic-h1-smoke", InstanceId: NewId(), AppVersion: "synthetic"}, TransportModeH1, carrier)
	}
	setupCtx, setupCancel := context.WithTimeout(ctx, 10*time.Second)
	defer setupCancel()
	for index := range sockets {
		select {
		case sockets[index] = <-accepted[index]:
		case <-setupCtx.Done():
			t.Fatalf("H1 socket setup: %v", setupCtx.Err())
		}
		for {
			notify := transports[index].ConnectedNotify()
			if transports[index].IsConnected() {
				break
			}
			select {
			case <-notify:
			case <-setupCtx.Done():
				t.Fatalf("H1 route setup: %v", setupCtx.Err())
			}
		}
		reading.CarrierBudget[index] = transportSettings[index].PlatformTransportBudget.Stats()
	}
	for direction := range 2 {
		workers.Go(func() {
			runWindowPhysicalH1Relay(ctx, input[direction], output[direction], rate, roundTrip/2, &highCount[direction], &highBytes[direction])
		})
		workers.Go(func() {
			for ctx.Err() == nil {
				kind, message, err := sockets[direction].ReadMessage()
				if err != nil {
					if ctx.Err() == nil {
						t.Errorf("%s relay read: %v", arm, err)
						cancel()
					}
					return
				}
				if len(message) == 0 {
					continue
				}
				if kind != websocket.BinaryMessage || !counters[direction].observe(message) {
					t.Errorf("invalid H1 message")
					cancel()
					return
				}
				owned := MessagePoolGet(len(message))
				copy(owned, message)
				select {
				case input[direction] <- owned:
				case <-ctx.Done():
					MessagePoolReturn(owned)
					return
				}
			}
		})
		workers.Go(func() {
			for {
				select {
				case <-ctx.Done():
					return
				case message := <-output[direction]:
					err := sockets[1-direction].WriteMessage(websocket.BinaryMessage, message)
					MessagePoolReturn(message)
					if err != nil {
						if ctx.Err() == nil {
							t.Errorf("%s relay write: %v", arm, err)
							cancel()
						}
						return
					}
				}
			}
		})
	}
	var refused atomic.Int64
	counts := make([]atomic.Int64, 1)
	// SDK deviceMemoryShares allocates 4/20 of the target to the provider.
	// Preserve its constructor-sized replay pool; generic FIFO controls keep 48 MiB.
	providerTarget := profiles[0].DeviceTarget / 5
	natSettings := DefaultProviderLocalUserNatSettingsWithMemoryTarget(providerTarget)
	natTcp := natSettings.TcpBufferSettings
	reading.Nat = windowPhysicalH1Nat{ProviderTarget: providerTarget, ReturnBudget: natTcp.ReturnQueueBudget.TotalByteCount(), Mtu: natTcp.Mtu, ReadBufferBytes: natTcp.ReadBufferByteCount, WriteBatchCount: natTcp.WriteBatchSize, SequenceBufferCount: natTcp.SequenceBufferSize, MaximumWindowBytes: natTcp.MaxWindowSize}
	cleanupWorkload = startWindowTcpWorkloadWithNatSettings(t, ctx, clients[0], clients[1], counts, false, &refused, 0, natSettings)
	time.Sleep(warmup)
	before := counts[0].Load()
	wireBefore := [2]windowPhysicalH1Wire{counters[0].snapshot(), counters[1].snapshot()}
	start, previous := time.Now(), time.Now()
	previousBytes := before
	for elapsed := time.Duration(0); elapsed < duration; elapsed = time.Since(start) {
		time.Sleep(min(time.Second, duration-elapsed))
		now, bytes := time.Now(), counts[0].Load()
		reading.IntervalMbps = append(reading.IntervalMbps, float64(bytes-previousBytes)*8/now.Sub(previous).Seconds()/1e6)
		previous, previousBytes = now, bytes
	}
	reading.Seconds = time.Since(start).Seconds()
	reading.DownloadMbps = float64(counts[0].Load()-before) * 8 / reading.Seconds / 1e6
	reading.NatRefused = refused.Load()
	for i := range 2 {
		wire := counters[i].snapshot()
		reading.Wire[i] = windowPhysicalH1Wire{Messages: wire.Messages - wireBefore[i].Messages, Bytes: wire.Bytes - wireBefore[i].Bytes, AckMessages: wire.AckMessages - wireBefore[i].AckMessages, AckBytes: wire.AckBytes - wireBefore[i].AckBytes}
		reading.MaxRelayQueued[i], reading.MaxRelayQueuedBytes[i] = highCount[i].Load(), highBytes[i].Load()
		reading.Connections[i] = connections[i].Load()
		reading.CarrierReceive[i] = transports[i].ReceiveStats()
		reading.Receiver[i], reading.Recovery[i] = clients[i].ReceiveStats(), clients[i].SendRecoveryStats()
		reading.Window[i] = clients[i].DestinationSendStats(clients[1-i].ClientId()).SendWindow
		reading.Policy[i] = windowPhysicalH1PolicySnapshot(clients[i], clients[1-i].ClientId())
	}
	return reading
}

// A/B/A separates host calibration drift from the candidate ratio. All rows
// remain in the log, including a failed or calibration-censored measurement.
func TestWindowPhysicalH1SdkSmoke(t *testing.T) {
	if os.Getenv("CONNECT_WINDOW_H1_MEASURE") == "" {
		t.Skip("set CONNECT_WINDOW_H1_MEASURE=1 for the owned physical H1 smoke")
	}
	assertMessagePoolOwnership(t)
	profiles, digest := windowSdkProfiles(t)
	var selected [2]windowPathEndpointProfile
	for _, profile := range profiles {
		if profile.Name == "sdk-provider-h1" {
			selected[0] = profile
		}
		if profile.Name == "sdk-device-h1" && profile.Providing {
			selected[1] = profile
		}
	}
	if selected[0].Name == "" || selected[1].Name == "" {
		t.Fatal("required mobile SDK profiles missing")
	}
	previous := MemoryBudget()
	SetMemoryBudget(selected[0].ProcessBudget)
	defer SetMemoryBudget(previous)
	var readings []windowPhysicalH1Reading
	for _, arm := range []string{"ceiling-before", "delivery", "ceiling-after"} {
		reading := measureWindowPhysicalH1(t, arm, digest, selected)
		readings = append(readings, reading)
		encoded, err := json.Marshal(reading)
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("physical-h1-reading %s", encoded)
		if reading.DownloadMbps <= 0 || reading.DownloadMbps > 101 || reading.NatRefused != 0 {
			t.Errorf("%s stalled, exceeded its serializer, or refused NAT traffic", arm)
		}
		for i := range 2 {
			if reading.Connections[i] != 1 || reading.Wire[i].AckMessages == 0 || reading.Wire[i].AckBytes == 0 {
				t.Errorf("%s missing physical ACK or reconnected in direction %d", arm, i)
			}
			if reading.MaxRelayQueued[i] > 512 || reading.MaxRelayQueuedBytes[i] > 2*1024*1024 {
				t.Errorf("%s exceeded finite relay bound", arm)
			}
			carrier, receive := reading.CarrierReceive[i], reading.Receiver[i]
			if carrier.H1.QueueDropMessageCount != 0 || receive.ReceiveQueueDropCount != 0 || receive.ReceiveQueueEvictionCount != 0 || receive.PackHandoffDropCount != 0 || receive.AckHandoffDropCount != 0 {
				t.Errorf("%s dropped reliable H1 traffic in direction %d", arm, i)
			}
		}
	}
	if !windowPhysicalH1ArmPolicyMatches("delivery", readings[1], 0) || !windowPhysicalH1ArmPolicyMatches("ceiling-before", readings[0], 0) || !windowPhysicalH1ArmPolicyMatches("ceiling-after", readings[2], 0) {
		t.Error("physical H1 arms did not select the intended window policy")
	}
	reference := (readings[0].DownloadMbps + readings[2].DownloadMbps) / 2
	drift := float64(1)
	if high := max(readings[0].DownloadMbps, readings[2].DownloadMbps); high > 0 {
		drift = math.Abs(readings[0].DownloadMbps-readings[2].DownloadMbps) / high
	}
	calibrated := min(readings[0].DownloadMbps, readings[2].DownloadMbps) >= 90 && drift <= .1
	ratio := float64(0)
	if reference > 0 {
		ratio = readings[1].DownloadMbps / reference
	}
	failureReasons, censoredReasons := []string{}, []string{}
	if t.Failed() {
		failureReasons = append(failureReasons, "fixture, carrier or lifecycle gate failed")
	}
	if min(readings[0].DownloadMbps, readings[2].DownloadMbps) < 90 {
		censoredReasons = append(censoredReasons, "reference below 90 percent of the 100 Mb/s serializer")
	}
	if drift > .1 {
		censoredReasons = append(censoredReasons, "reference drift above 10 percent")
	}
	if ratio < .9 {
		failureReasons = append(failureReasons, "candidate below 90 percent of matched reference")
	}
	comparison := map[string]any{
		"Scope":         "one 100 Mb/s, 300 microsecond added RTT, one-flow download; real TLS/WebSocket H1 and owned TCP origin; userspace gVisor TUN; Transfer encryption disabled; synthetic relay without server authentication; no native TUN or database tier",
		"ReferenceMbps": reference, "CandidateMbps": readings[1].DownloadMbps,
		"Ratio": ratio, "ReferenceDrift": drift, "Calibrated": calibrated,
		"FailureReasons": failureReasons, "CensoredReasons": censoredReasons,
	}
	encoded, err := json.Marshal(comparison)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("physical-h1-comparison %s", encoded)
	if !calibrated {
		t.Error("physical H1 calibration below 90% link capacity or A/A drift above 10%")
	}
	if ratio < .9 {
		t.Error("physical H1 candidate below 90% matched reference")
	}
}

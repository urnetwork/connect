package connect

// Explicit upload-close gates force native and alt transport cleanup to
// finish after evaluation cleanup, so a too-early idle sweep cannot pass.

import (
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

// A finite upload whose transport-owned Close has an explicit completion gate.
type reviewRetirementUploadBody struct {
	io.ReadCloser
	entered chan struct{}
	release <-chan struct{}
	once    sync.Once
}

// Keeps the transport's stream ownership live after response materialization.
func (self *reviewRetirementUploadBody) Close() error {
	self.once.Do(func() {
		close(self.entered)
		<-self.release
		self.ReadCloser.Close()
	})
	return nil
}

// Native HTTP/2 forgets its stream only after the cloned upload Close returns.
func TestClientStrategyRetirementNativeH2DeferredUploadClose(t *testing.T) {
	for _, reset := range []bool{false, true} {
		strategy, dialer := newRetirementTestStrategy(t)
		releaseResponse, releaseUpload := make(chan struct{}), make(chan struct{})
		var responseOnce, uploadOnce sync.Once
		unblockResponse := func() { responseOnce.Do(func() { close(releaseResponse) }) }
		unblockUpload := func() { uploadOnce.Do(func() { close(releaseUpload) }) }
		defer unblockResponse()
		defer unblockUpload()
		headersSent, uploadEntered, closed := make(chan struct{}), make(chan struct{}), make(chan struct{})
		var closedOnce sync.Once
		server := newFamilyHttptestUnstartedServer(t, 4, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			writer.Header().Set("Content-Length", "2")
			writer.WriteHeader(http.StatusOK)
			writer.(http.Flusher).Flush()
			close(headersSent)
			select {
			case <-releaseResponse:
				io.WriteString(writer, "ok")
			case <-request.Context().Done():
			}
		}))
		server.EnableHTTP2 = true
		server.Config.ConnState = func(_ net.Conn, state http.ConnState) {
			if state == http.StateClosed {
				closedOnce.Do(func() { close(closed) })
			}
		}
		server.StartTLS()
		t.Cleanup(server.Close)
		strategy.settings.TlsConfig = server.Client().Transport.(*http.Transport).TLSClientConfig.Clone()
		dialer.httpDialTlsContext = newNormalDialTlsContext(strategy.settings, clientHttpNextProtos)
		native := dialer.HttpClient().Transport.(*http.Transport)
		request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, server.URL, strings.NewReader("input"))
		if err != nil {
			t.Fatal(err)
		}
		request.GetBody = func() (io.ReadCloser, error) {
			return &reviewRetirementUploadBody{ReadCloser: io.NopCloser(strings.NewReader("input")), entered: uploadEntered, release: releaseUpload}, nil
		}
		done := make(chan error, 1)
		go func() {
			result, err := strategy.HttpParallel(request)
			if err == nil && result.response.ProtoMajor != 2 {
				t.Error("fixture did not negotiate HTTP/2")
			}
			done <- err
		}()
		select {
		case <-headersSent:
		case <-time.After(5 * time.Second):
			t.Fatal("native response was not selected")
		}
		if reset {
			dialer.Close()
		} else {
			strategy.SetVlessConfigs(nil)
		}
		unblockResponse()
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("reset=%t: native result: %v", reset, err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("native evaluation did not return before upload cleanup")
		}
		select {
		case <-uploadEntered:
		case <-time.After(5 * time.Second):
			t.Fatal("native upload Close was not reached")
		}
		unblockUpload()
		select {
		case <-closed:
		case <-time.After(time.Second):
			t.Errorf("reset=%t: native owner leaked socket after delayed stream cleanup", reset)
		}
		native.CloseIdleConnections()
		server.Close()
	}
}

// Alt's active flag stays true until its asynchronous upload and send drain.
func TestClientStrategyRetirementAltDeferredUploadClose(t *testing.T) {
	for _, reset := range []bool{false, true} {
		strategy, dialer := newRetirementTestStrategy(t)
		releaseResponse, releaseUpload := make(chan struct{}), make(chan struct{})
		var responseOnce, uploadOnce sync.Once
		unblockResponse := func() { responseOnce.Do(func() { close(releaseResponse) }) }
		unblockUpload := func() { uploadOnce.Do(func() { close(releaseUpload) }) }
		defer unblockResponse()
		defer unblockUpload()
		headersSent, uploadEntered := make(chan struct{}), make(chan struct{})
		client, _ := newAltMemoryFixture(t, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			writer.Header().Set("Content-Length", "2")
			writer.WriteHeader(http.StatusOK)
			writer.(http.Flusher).Flush()
			close(headersSent)
			select {
			case <-releaseResponse:
				io.WriteString(writer, "ok")
			case <-request.Context().Done():
			}
		}))
		bounded := client.Transport.(*altQuicBoundedTransport)
		dialer.httpClientFactory = func() *http.Client { return client }
		request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "https://"+testAltApiHost+"/retirement", strings.NewReader("input"))
		if err != nil {
			t.Fatal(err)
		}
		request.GetBody = func() (io.ReadCloser, error) {
			return &reviewRetirementUploadBody{ReadCloser: io.NopCloser(strings.NewReader("input")), entered: uploadEntered, release: releaseUpload}, nil
		}
		done := make(chan error, 1)
		go func() { _, err := strategy.HttpParallel(request); done <- err }()
		select {
		case <-headersSent:
		case <-time.After(5 * time.Second):
			t.Fatal("alt response was not selected")
		}
		if reset {
			dialer.Close()
		} else {
			strategy.SetVlessConfigs(nil)
		}
		unblockResponse()
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("reset=%t: alt result: %v", reset, err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("alt evaluation did not return before upload cleanup")
		}
		select {
		case <-uploadEntered:
		case <-time.After(5 * time.Second):
			t.Fatal("alt upload Close was not reached")
		}
		unblockUpload()
		waitAltMemorySlot(t, client)
		if conn := bounded.connection(testAltApiHost + ":443"); conn != nil && conn.Context().Err() == nil {
			t.Errorf("reset=%t: alt owner leaked socket after delayed stream cleanup", reset)
		}
		bounded.Close()
	}
}

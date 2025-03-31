package extender

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"time"

	"testing"

	"github.com/go-playground/assert/v2"

	"github.com/urnetwork/connect"
)

func TestExtender(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping testing in short mode")
	}

	// actual content server, port 443 (127.0.0.1)
	// https, self signed
	// one route, /hello

	// extender server, port 442

	// client

	// test uses extender http client to GET /hello

	settings := DefaultExtenderSettings()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	certPemBytes, keyPemBytes, err := selfSign([]string{"localhost"}, "Connect Test", settings.ValidFrom, settings.ValidFor)
	assert.Equal(t, err, nil)

	tempDirPath, err := os.MkdirTemp("", "connect")
	assert.Equal(t, err, nil)

	certFile := filepath.Join(tempDirPath, "localhost.pem")
	keyFile := filepath.Join(tempDirPath, "localhost.key")
	os.WriteFile(certFile, certPemBytes, 0x777)
	os.WriteFile(keyFile, keyPemBytes, 0x777)

	server := &http.Server{
		Addr:    fmt.Sprintf(":%d", 443),
		Handler: &testExtenderServer{},
	}
	defer server.Close()
	go server.ListenAndServeTLS(certFile, keyFile)

	extenderServer := NewExtenderServer(
		ctx,
		[]string{"montrose"},
		[]string{"localhost"},
		map[int][]connect.ExtenderConnectMode{
			1442: []connect.ExtenderConnectMode{connect.ExtenderConnectModeTcpTls},
		},
		&net.Dialer{},
		settings,
	)
	defer extenderServer.Close()
	go extenderServer.ListenAndServe()

	select {
	case <-time.After(1 * time.Second):
	}

	localIp, err := netip.ParseAddr("127.0.0.1")
	assert.Equal(t, err, nil)

	connectSettings := connect.DefaultConnectSettings()
	connectSettings.TlsConfig = &tls.Config{
		InsecureSkipVerify: true,
	}

	client := connect.NewExtenderHttpClient(
		connectSettings,
		&connect.ExtenderConfig{
			Profile: connect.ExtenderProfile{
				ConnectMode: connect.ExtenderConnectModeTcpTls,
				ServerName:  "bringyour.com",
				Port:        1442,
			},
			Ip:     localIp,
			Secret: "montrose",
		},
	)

	r, err := client.Get("https://localhost/hello")

	assert.Equal(t, err, nil)
	assert.Equal(t, r.StatusCode, 200)

	body, err := io.ReadAll(r.Body)
	assert.Equal(t, err, nil)
	assert.Equal(t, string(body), "{}")

}

type testExtenderServer struct {
}

func (self *testExtenderServer) ServeHTTP(w http.ResponseWriter, req *http.Request) {

	w.Header().Add("Content-Type", "application/json")
	w.Write([]byte("{}"))
}

package extender

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"time"

	"testing"

	"github.com/urnetwork/connect"
)

func TestExtender(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping testing in short mode")
	}

	settings := DefaultExtenderSettings()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	certPemBytes, keyPemBytes, err := selfSign([]string{"127.0.0.1"}, "Connect Test", settings.ValidFrom, settings.ValidFor)
	if err != nil {
		t.Fatal(err)
	}

	tempDirPath := t.TempDir()

	certFile := filepath.Join(tempDirPath, "localhost.pem")
	keyFile := filepath.Join(tempDirPath, "localhost.key")
	if err := os.WriteFile(certFile, certPemBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyFile, keyPemBytes, 0o600); err != nil {
		t.Fatal(err)
	}

	contentListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	contentPort := contentListener.Addr().(*net.TCPAddr).Port
	server := &http.Server{
		Handler: &testExtenderServer{},
	}
	contentDone := make(chan error, 1)
	go func() {
		contentDone <- server.ServeTLS(contentListener, certFile, keyFile)
	}()
	t.Cleanup(func() {
		server.Close()
		if err := <-contentDone; err != nil && !errors.Is(err, http.ErrServerClosed) && !errors.Is(err, net.ErrClosed) {
			t.Errorf("content server: %v", err)
		}
	})

	extenderListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	extenderPort := extenderListener.Addr().(*net.TCPAddr).Port
	listenerClaimed := false
	settings.Listen = func(network string, address string) (net.Listener, error) {
		if network != "tcp" || address != fmt.Sprintf(":%d", extenderPort) {
			return nil, fmt.Errorf("unexpected extender listen %s %s", network, address)
		}
		if listenerClaimed {
			return nil, fmt.Errorf("extender listener requested more than once")
		}
		listenerClaimed = true
		return extenderListener, nil
	}
	handlerErrors := make(chan error, 1)
	settings.ErrorHandler = func(stage string, err error) {
		select {
		case handlerErrors <- fmt.Errorf("%s: %w", stage, err):
		default:
		}
	}

	extenderServer := NewExtenderServer(
		ctx,
		[]string{"montrose"},
		[]string{"127.0.0.1"},
		map[int][]connect.ExtenderConnectMode{
			extenderPort: {connect.ExtenderConnectModeTcpTls},
		},
		&net.Dialer{},
		settings,
	)
	extenderDone := make(chan error, 1)
	go func() {
		extenderDone <- extenderServer.ListenAndServe()
	}()
	t.Cleanup(func() {
		extenderServer.CloseAndWait()
		if err := <-extenderDone; err != nil {
			t.Errorf("extender server: %v", err)
		}
	})

	localIp, err := netip.ParseAddr("127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}

	rootCAs := x509.NewCertPool()
	if !rootCAs.AppendCertsFromPEM(certPemBytes) {
		t.Fatal("could not add content server certificate")
	}
	connectSettings := connect.DefaultConnectSettings()
	connectSettings.TlsConfig = &tls.Config{
		RootCAs: rootCAs,
	}

	client := connect.NewExtenderHttpClient(
		connectSettings,
		&connect.ExtenderConfig{
			Profile: connect.ExtenderProfile{
				ConnectMode: connect.ExtenderConnectModeTcpTls,
				ServerName:  "bringyour.com",
				Port:        extenderPort,
			},
			Ip:     localIp,
			Secret: "montrose",
		},
	)
	t.Cleanup(client.CloseIdleConnections)

	response, err := client.Get(fmt.Sprintf("https://127.0.0.1:%d/hello", contentPort))
	if err != nil {
		select {
		case handlerErr := <-handlerErrors:
			t.Fatalf("request: %v; extender: %v", err, handlerErr)
		default:
			t.Fatal(err)
		}
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, expected %d", response.StatusCode, http.StatusOK)
	}

	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "{}" {
		t.Fatalf("body = %q, expected %q", body, "{}")
	}

}

func TestSelfSignValiditySpansPresent(t *testing.T) {
	before := time.Now()
	certPemBytes, _, err := selfSign(
		[]string{"localhost"},
		"Connect Test",
		2*time.Hour,
		3*time.Hour,
	)
	if err != nil {
		t.Fatal(err)
	}
	block, _ := pem.Decode(certPemBytes)
	if block == nil {
		t.Fatal("selfSign returned no certificate PEM block")
	}
	certificate, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	after := time.Now()
	if delta := certificate.NotBefore.Sub(before.Add(-2 * time.Hour)); delta < -time.Second || time.Second < delta {
		t.Fatalf("NotBefore = %s, expected about two hours before creation", certificate.NotBefore)
	}
	if delta := certificate.NotAfter.Sub(after.Add(3 * time.Hour)); delta < -time.Second || time.Second < delta {
		t.Fatalf("NotAfter = %s, expected about three hours after creation", certificate.NotAfter)
	}
	if before.Before(certificate.NotBefore) || certificate.NotAfter.Before(after) {
		t.Fatalf("certificate validity %s..%s does not span creation", certificate.NotBefore, certificate.NotAfter)
	}
}

type testExtenderServer struct {
}

func (self *testExtenderServer) ServeHTTP(w http.ResponseWriter, req *http.Request) {

	w.Header().Add("Content-Type", "application/json")
	w.Write([]byte("{}"))
}

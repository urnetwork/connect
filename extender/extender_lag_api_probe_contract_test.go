package extender

import (
	"bufio"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"testing"
	"time"
)

// The contract the server's measurement api probe reads (server/lagprobe,
// lag.dev/PLAN.md A4). lag.dev leaves out an extender whose probes fail, so if
// this answer changes, healthy extenders read as failed and lag.dev offers none of
// them. The probe has no ticket and asks for no upgrade: it needs a tls handshake
// for the measurement name that agrees on http/1.1, and a GET of the session route
// from the lag.dev origin answered 400 with that origin allowed, for no admission.

// What the probe reads of one answer. The request is the probe's own, on a
// connection of its own, closed afterwards.
type testLagProbeAnswer struct {
	protocol string
	status   int
	origins  []string
}

func testLagProbe(fixture *lagApiFixture, address string, serverName string, origin string) (*testLagProbeAnswer, error) {
	conn, err := tls.DialWithDialer(&net.Dialer{Timeout: 5 * time.Second}, "tcp", address, &tls.Config{
		ServerName: serverName,
		RootCAs:    fixture.rootCas,
		NextProtos: []string{"http/1.1"},
		MinVersion: tls.VersionTLS12,
	})
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := fmt.Fprintf(
		conn,
		"GET /lag/v1/session HTTP/1.1\r\nHost: %s\r\nUser-Agent: urnetwork-measurement-probe/1\r\nOrigin: %s\r\nConnection: close\r\n\r\n",
		serverName,
		origin,
	); err != nil {
		return nil, err
	}
	response, err := http.ReadResponse(bufio.NewReader(io.LimitReader(conn, 16*1024)), nil)
	if err != nil {
		return nil, err
	}
	return &testLagProbeAnswer{
		protocol: conn.ConnectionState().NegotiatedProtocol,
		status:   response.StatusCode,
		origins:  response.Header.Values("Access-Control-Allow-Origin"),
	}, nil
}

// The real accept path and api, under the production limits on what a subnet may
// do: the probe passes, and however often it asks it takes no admission from its
// subnet, as one a session would, and leaves no connection or session behind.
func TestLagApiAnswersTheServersProbe(t *testing.T) {
	for _, camouflage := range []bool{false, true} {
		t.Run(fmt.Sprintf("camouflage %t", camouflage), func(t *testing.T) {
			fixture := newLagApiFixture(t, nil)
			fixture.server.measurementZone = "extender.example"
			address := fixture.serveExtender(func(settings *ExtenderSettings) {
				settings.CamouflageEnabled = camouflage
				settings.AdmissionActionsPerSubnetPerMinute = 8
				settings.AdmissionRefusalsPerSubnetPerMinute = 8
			})

			// far past 8 actions a minute
			for i := range 40 {
				answer, err := testLagProbe(fixture, address, testLagName, testLagOrigin)
				if err != nil {
					t.Fatalf("probe %d: %v", i, err)
				}
				if answer.protocol != "http/1.1" {
					t.Fatalf("probe %d agreed on %q", i, answer.protocol)
				}
				if answer.status != http.StatusBadRequest {
					t.Fatalf("probe %d answered %d", i, answer.status)
				}
				if len(answer.origins) != 1 || answer.origins[0] != testLagOrigin {
					t.Fatalf("probe %d allowed %q", i, answer.origins)
				}
			}

			fixture.server.admission.stateLock.Lock()
			entries := len(fixture.server.admission.subnetEntries)
			fixture.server.admission.stateLock.Unlock()
			if entries != 0 {
				t.Fatalf("the probes took admission from %d subnets", entries)
			}
			if stats := fixture.server.AdmissionStats(); stats != (ExtenderAdmissionStats{}) {
				t.Fatalf("the probes were counted by admission: %+v", stats)
			}
			fixture.api.stateLock.Lock()
			sessions := len(fixture.api.sessions)
			fixture.api.stateLock.Unlock()
			if sessions != 0 {
				t.Fatalf("the probes opened %d sessions", sessions)
			}
			waitFor(t, func() bool {
				return fixture.server.ConnectionCount() == 0
			})

			// another origin is answered without being allowed, which the probe reads
			// as a failure of cors
			answer, err := testLagProbe(fixture, address, testLagName, "https://example.com")
			if err != nil {
				t.Fatal(err)
			}
			if answer.status != http.StatusBadRequest || len(answer.origins) != 0 {
				t.Fatalf("another origin: %d, allowed %q", answer.status, answer.origins)
			}

			// a name it does not serve has no handshake the probe can verify
			if answer, err := testLagProbe(
				fixture,
				address,
				"0192e4a1-0000-7000-8000-000000000009.extender.example",
				testLagOrigin,
			); err == nil {
				t.Fatalf("another measurement name: %+v", answer)
			}

			// nor does its own, once it holds no certificate for it
			fixture.certificates.set(nil, nil)
			if answer, err := testLagProbe(fixture, address, testLagName, testLagOrigin); err == nil {
				t.Fatalf("no certificate: %+v", answer)
			}
		})
	}
}

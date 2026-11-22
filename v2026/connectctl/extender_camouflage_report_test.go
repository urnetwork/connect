package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/docopt/docopt-go"

	"github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/connect/v2026/extender"
)

// The camouflage report on an operator host (connect/EXTENDER-SPLICE-ROLLOUT.md,
// Reporting path): the flag is off by default and it is the cohort control, since a
// host has no network space. On, the extender posts to the operator what its server
// counted and its verdict, under its own credential. Off, it posts nothing.

func TestExtenderCamouflageReportFlagIsOffByDefault(t *testing.T) {
	opts, err := docopt.ParseArgs(connectCtlUsage(), []string{"extender", "--jwt=test-jwt"}, ConnectCtlVersion)
	if err != nil {
		t.Fatal(err)
	}
	options, err := extenderOptionsFromOpts(opts)
	if err != nil {
		t.Fatal(err)
	}
	if options.camouflageReport {
		t.Errorf("the report is on by default")
	}

	opts, err = docopt.ParseArgs(connectCtlUsage(), []string{"extender", "--jwt=test-jwt", "--camouflage_report"}, ConnectCtlVersion)
	if err != nil {
		t.Fatal(err)
	}
	options, err = extenderOptionsFromOpts(opts)
	if err != nil {
		t.Fatal(err)
	}
	if !options.camouflageReport {
		t.Errorf("the flag did not turn the report on")
	}
}

// Runs one extender against the fixture operator, and returns every report it posted
// by the time a look has had its chance.
func runTestExtenderWithCamouflageReport(t *testing.T, enabled bool) []*testCamouflageReportRequest {
	t.Helper()
	operator := newTestExtenderOperator(t)
	reports := make(chan *testCamouflageReportRequest, 8)
	operatorHandler := operator.handle
	operator.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != connect.ExtenderCamouflageReportPath {
			operatorHandler(w, r)
			return
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		reports <- &testCamouflageReportRequest{authorization: r.Header.Get("Authorization"), body: body}
		json.NewEncoder(w).Encode(&connect.ExtenderCamouflageReportResult{Outcome: connect.ExtenderCamouflageReportAccepted})
	})
	operatorAddress := operator.server.Listener.Addr().String()
	_, operatorPort, err := net.SplitHostPort(operatorAddress)
	if err != nil {
		t.Fatal(err)
	}
	tcpListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { tcpListener.Close() })

	// the first look at once, and one report for the whole test
	runs := make(chan *extenderRun, 1)
	options := &extenderOptions{
		jwt:      "test-jwt",
		apiUrl:   fmt.Sprintf("http://%s:%s", testExtenderApiHost, operatorPort),
		stateDir: t.TempDir(),
		tcpPort:  tcpListener.Addr().(*net.TCPAddr).Port,
		udpPort:  1,
		dnsPort:  2,
		listen: func(network string, address string) (net.Listener, error) {
			return tcpListener, nil
		},
		listenPacket: func(network string, address string) (net.PacketConn, error) {
			return nil, fmt.Errorf("this test serves tcp only")
		},
		dialContext: func(ctx context.Context, network string, address string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "tcp", operatorAddress)
		},
		packetConnFactory:      testLoopbackPacketConn,
		configureNetworkClient: testHermeticNetworkClient,
		onStart: func(run *extenderRun) {
			runs <- run
		},
		camouflageReport: enabled,
		configureCamouflageReport: func(settings *connect.ExtenderCamouflageReporterSettings) {
			settings.FirstInterval = time.Millisecond
			settings.FirstIntervalJitter = 0
			settings.Interval = time.Hour
			settings.ClientVersion = func() string { return "2026.10.30-1" }
		},
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runDone := make(chan error, 1)
	go func() {
		runDone <- runExtender(ctx, options)
	}()
	select {
	case <-runs:
	case err := <-runDone:
		t.Fatalf("the extender exited before it started: %v", err)
	case <-time.After(60 * time.Second):
		t.Fatal("the extender did not start")
	}

	received := []*testCamouflageReportRequest{}
	if enabled {
		select {
		case report := <-reports:
			received = append(received, report)
		case <-time.After(60 * time.Second):
			t.Fatal("the extender posted no report")
		}
	} else {
		select {
		case report := <-reports:
			t.Fatalf("the extender posted a report with the flag off: %s", report.body)
		case <-time.After(500 * time.Millisecond):
		}
	}
	cancel()
	select {
	case <-runDone:
	case <-time.After(60 * time.Second):
		t.Fatal("the extender did not stop")
	}
	return received
}

type testCamouflageReportRequest struct {
	authorization string
	body          []byte
}

func TestExtenderCommandPostsTheCamouflageReportOnlyWhenAsked(t *testing.T) {
	if reports := runTestExtenderWithCamouflageReport(t, false); len(reports) != 0 {
		t.Fatalf("reports = %d with the flag off", len(reports))
	}

	reports := runTestExtenderWithCamouflageReport(t, true)
	if len(reports) != 1 {
		t.Fatalf("reports = %d", len(reports))
	}
	report := reports[0]
	if report.authorization != "Bearer test-jwt" {
		t.Errorf("authorization = %q", report.authorization)
	}
	args := &connect.ExtenderCamouflageReportArgs{}
	if err := json.Unmarshal(report.body, args); err != nil {
		t.Fatal(err)
	}
	if len(args.ReportId) != 32 {
		t.Errorf("report id = %q", args.ReportId)
	}
	if args.ClientVersion != "2026.10.30-1" {
		t.Errorf("client version = %q", args.ClientVersion)
	}
	// camouflage is off on this host, and the honest verdict goes out all the same
	if args.Extender == nil || args.Extender.Covered || args.Extender.DegradedReason != extender.ExtenderCamouflageDegradedCamouflageOff {
		t.Errorf("extender part = %s", report.body)
	}
}

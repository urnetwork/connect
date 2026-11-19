// The rule of extender_error.go: no client's address reaches an ErrorHandler, at any
// stage, however the error holds it, except for the stages named below, whose whole
// error is the dial of something the operator named.

package extender

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"golang.org/x/net/dns/dnsmessage"
	"google.golang.org/protobuf/proto"

	"github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/connect/v2026/protocol"
)

// The only stages whose whole error an embedder hears. Written out here, not read from
// the code, so a name added there fails this until it is read and added here too.
var testExtenderWholeErrorStages = []string{
	"forward dial",
	"datagram forward dial",
	"nlayer hop dial",
	"nlayer probe dial",
}

// The client of these tests, in both families, and what no report may hold of it.
var (
	testErrorClient   = &net.TCPAddr{IP: net.ParseIP("203.0.113.7"), Port: 44444}
	testErrorClient6  = &net.TCPAddr{IP: net.ParseIP("2001:db8:1234::7"), Port: 44444}
	testErrorServer   = &net.TCPAddr{IP: net.ParseIP("192.0.2.1"), Port: 443}
	testErrorNotHeard = []string{"203.0.113.7", "2001:db8:1234::7", "44444", "192.0.2.1"}
)

// An error of a type nothing here has heard of, which holds the address in a field too.
type testErrorOfNoKnownType struct {
	addr *net.TCPAddr
}

func (self *testErrorOfNoKnownType) Error() string {
	return fmt.Sprintf("something happened with %s", self.addr)
}

// The same, reading as a timeout.
type testErrorTimeoutOfNoKnownType struct {
	testErrorOfNoKnownType
}

func (self *testErrorTimeoutOfNoKnownType) Timeout() bool {
	return true
}

// Every way an error can hold a client's address. Each case is checked to hold it, so
// none of them passes for having nothing to scrub.
func testErrorsNamingAClient() map[string]error {
	opErr := func(addr *net.TCPAddr, err error) error {
		return &net.OpError{Op: "read", Net: "tcp", Source: testErrorServer, Addr: addr, Err: err}
	}
	return map[string]error{
		// what the dns, probe and proxy limits built, none of which a typed scrub sees
		"plain text, ipv4":             errors.New("203.0.113.7 is over its query rate"),
		"plain text, ipv6":             errors.New("2001:db8:1234::7 already has a query in flight"),
		"plain text, with a port":      fmt.Errorf("from %s: refused", testErrorClient),
		"quoted in a plain text":       fmt.Errorf("host %q is not allowed", testErrorClient.IP),
		"op error, ipv4":               opErr(testErrorClient, net.ErrClosed),
		"op error, ipv6":               opErr(testErrorClient6, net.ErrClosed),
		"op error, an errno":           opErr(testErrorClient, os.NewSyscallError("read", syscall.ECONNRESET)),
		"op error, a timeout":          opErr(testErrorClient, os.ErrDeadlineExceeded),
		"wrapped op error":             fmt.Errorf("tls: %w", opErr(testErrorClient, net.ErrClosed)),
		"wrapped in text naming it":    fmt.Errorf("%s: %w", testErrorClient, syscall.ECONNRESET),
		"addr error":                   &net.AddrError{Err: "non-IPv4 address", Addr: testErrorClient.IP.String()},
		"addr error text naming it":    &net.AddrError{Err: "203.0.113.7 is not an address", Addr: "x"},
		"url error":                    &url.Error{Op: "Post", URL: "https://203.0.113.7:44444/", Err: errors.New("failed")},
		"dns error":                    &net.DNSError{Err: "no such host", Name: "203.0.113.7", Server: "192.0.2.1:53"},
		"parse error":                  &net.ParseError{Type: "IP address", Text: "203.0.113.7:44444"},
		"a type of no known kind":      &testErrorOfNoKnownType{addr: testErrorClient},
		"a timeout of no known kind":   &testErrorTimeoutOfNoKnownType{testErrorOfNoKnownType{addr: testErrorClient}},
		"joined to an end":             errors.Join(errors.New("203.0.113.7 failed"), net.ErrClosed),
		"joined, the end first":        errors.Join(net.ErrClosed, errors.New("203.0.113.7 failed")),
		"two wrapped, the second text": fmt.Errorf("%w: %w", context.Canceled, errors.New("203.0.113.7 failed")),
	}
}

// Everything a handler can read of an error: its text and its formatted values, at
// every level it can unwrap to.
func testErrorReadings(err error) []string {
	readings := []string{}
	var read func(err error)
	read = func(err error) {
		readings = append(readings, err.Error(), fmt.Sprintf("%+v", err), fmt.Sprintf("%#v", err))
		switch unwrapper := err.(type) {
		case interface{ Unwrap() error }:
			if causeErr := unwrapper.Unwrap(); causeErr != nil {
				read(causeErr)
			}
		case interface{ Unwrap() []error }:
			for _, causeErr := range unwrapper.Unwrap() {
				if causeErr != nil {
					read(causeErr)
				}
			}
		}
	}
	read(err)
	return readings
}

// Fails the test if a report holds any of the text, at any level, or still has a chain
// behind it, which is how a handler could get back to a typed error's address fields.
func testExpectErrorHoldsNone(t *testing.T, err error, notHeard ...string) {
	t.Helper()
	if err == nil {
		return
	}
	for _, reading := range testErrorReadings(err) {
		for _, text := range notHeard {
			if strings.Contains(reading, text) {
				t.Fatalf("the handler can read %q in %q", text, reading)
			}
		}
	}
	if errors.Unwrap(err) != nil {
		t.Fatalf("the handler can unwrap %#v", err)
	}
}

// A server with no carriers whose ErrorHandler records what it hears. It runs
// synchronously in reportError, and the dns workers call it from their own goroutines.
type testErrorRecorder struct {
	stateLock sync.Mutex
	reports   []testErrorReport
}

type testErrorReport struct {
	stage string
	err   error
}

func (self *testErrorRecorder) report(stage string, err error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.reports = append(self.reports, testErrorReport{stage: stage, err: err})
}

func (self *testErrorRecorder) stageReports(stage string) []testErrorReport {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	reports := []testErrorReport{}
	for _, report := range self.reports {
		if report.stage == stage {
			reports = append(reports, report)
		}
	}
	return reports
}

func (self *testErrorRecorder) allReports() []testErrorReport {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return slices.Clone(self.reports)
}

// One non-carrier server hearing with a new recorder, with its settings to adjust.
func newTestErrorServer(
	t *testing.T,
	allowedHosts []string,
	configure func(settings *ExtenderSettings),
) (*ExtenderServer, *testErrorRecorder) {
	t.Helper()
	recorder := &testErrorRecorder{}
	settings := DefaultExtenderSettings()
	settings.ErrorHandler = recorder.report
	if configure != nil {
		configure(settings)
	}
	ctx, cancel := context.WithCancel(context.Background())
	server := NewExtenderServer(ctx, nil, allowedHosts, nil, nil, settings)
	t.Cleanup(func() {
		server.CloseAndWait()
		cancel()
	})
	return server, recorder
}

// The non-test sources of this package, parsed.
func parseTestExtenderSources(t *testing.T) (*token.FileSet, []*ast.File) {
	t.Helper()
	paths, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fileSet := token.NewFileSet()
	files := []*ast.File{}
	for _, path := range paths {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fileSet, path, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, file)
	}
	if len(files) == 0 {
		t.Fatal("no source files")
	}
	return fileSet, files
}

// Calls fn with each function declared in the files, for what is inside it.
func forEachTestExtenderFunc(files []*ast.File, fn func(funcDecl *ast.FuncDecl)) {
	for _, file := range files {
		for _, decl := range file.Decls {
			if funcDecl, ok := decl.(*ast.FuncDecl); ok && funcDecl.Body != nil {
				fn(funcDecl)
			}
		}
	}
}

// The string constants the files declare, by name.
func testExtenderStringConstants(t *testing.T, files []*ast.File) map[string]string {
	t.Helper()
	constants := map[string]string{}
	for _, file := range files {
		for _, decl := range file.Decls {
			genDecl, ok := decl.(*ast.GenDecl)
			if !ok || genDecl.Tok != token.CONST {
				continue
			}
			for _, spec := range genDecl.Specs {
				valueSpec := spec.(*ast.ValueSpec)
				for i, value := range valueSpec.Values {
					if literal, ok := value.(*ast.BasicLit); ok && literal.Kind == token.STRING {
						text, err := strconv.Unquote(literal.Value)
						if err != nil {
							t.Fatal(err)
						}
						constants[valueSpec.Names[i].Name] = text
					}
				}
			}
		}
	}
	return constants
}

// The name of the function a call calls, whatever its receiver.
func testExtenderCallName(call *ast.CallExpr) string {
	switch fun := call.Fun.(type) {
	case *ast.Ident:
		return fun.Name
	case *ast.SelectorExpr:
		return fun.Sel.Name
	}
	return ""
}

// The parameters of a function, by name.
func testExtenderParamNames(funcDecl *ast.FuncDecl) []string {
	paramNames := []string{}
	for _, field := range funcDecl.Type.Params.List {
		for _, name := range field.Names {
			paramNames = append(paramNames, name.Name)
		}
	}
	return paramNames
}

// Which parameter of a function is the stage it reports, out of how many.
type testExtenderStageParam struct {
	index int
	count int
}

// Every function that takes a stage: reportError, and whatever passes a parameter of its
// own on to one that does, such as a refusal.
func testExtenderStageParams(files []*ast.File) map[string]testExtenderStageParam {
	stageParams := map[string]testExtenderStageParam{"reportError": {index: 0, count: 2}}
	for changed := true; changed; {
		changed = false
		forEachTestExtenderFunc(files, func(funcDecl *ast.FuncDecl) {
			if _, ok := stageParams[funcDecl.Name.Name]; ok {
				return
			}
			paramNames := testExtenderParamNames(funcDecl)
			ast.Inspect(funcDecl.Body, func(node ast.Node) bool {
				call, ok := node.(*ast.CallExpr)
				if !ok {
					return true
				}
				stageParam, ok := stageParams[testExtenderCallName(call)]
				if !ok || len(call.Args) != stageParam.count {
					return true
				}
				if arg, ok := call.Args[stageParam.index].(*ast.Ident); ok {
					if index := slices.Index(paramNames, arg.Name); 0 <= index {
						stageParams[funcDecl.Name.Name] = testExtenderStageParam{index: index, count: len(paramNames)}
						changed = true
					}
				}
				return true
			})
		})
	}
	return stageParams
}

// Every stage the sources can report. Fails if a stage is anything but a constant: a
// stage built at run time could hold what the error may not.
func testExtenderErrorStages(t *testing.T) []string {
	t.Helper()
	fileSet, files := parseTestExtenderSources(t)
	constants := testExtenderStringConstants(t, files)
	stageParams := testExtenderStageParams(files)
	stages := []string{
		extenderAdmissionStage(extenderLimitedBySubnets),
		extenderAdmissionStage(extenderLimitedBySource),
	}
	forEachTestExtenderFunc(files, func(funcDecl *ast.FuncDecl) {
		paramNames := testExtenderParamNames(funcDecl)
		ast.Inspect(funcDecl.Body, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok {
				return true
			}
			stageParam, ok := stageParams[testExtenderCallName(call)]
			if !ok || len(call.Args) != stageParam.count {
				return true
			}
			stageArg := call.Args[stageParam.index]
			switch arg := stageArg.(type) {
			case *ast.BasicLit:
				stage, err := strconv.Unquote(arg.Value)
				if err != nil {
					t.Fatal(err)
				}
				stages = append(stages, stage)
				return true
			case *ast.Ident:
				if stage, ok := constants[arg.Name]; ok {
					stages = append(stages, stage)
					return true
				}
				// a parameter passed on, each call of whose function is checked here
				if own, ok := stageParams[funcDecl.Name.Name]; ok && slices.Index(paramNames, arg.Name) == own.index {
					return true
				}
			case *ast.CallExpr:
				if fun, ok := arg.Fun.(*ast.Ident); ok && fun.Name == "extenderAdmissionStage" {
					return true
				}
			}
			t.Errorf("%s: a stage that is not a constant", fileSet.Position(stageArg.Pos()))
			return true
		})
	})
	slices.Sort(stages)
	return slices.Compact(stages)
}

// Across every stage the code has, and one it does not have yet: a stage named here
// hears the error it was given, and any other hears nothing of a client's address,
// however the error holds it. A plain text error is the case a scrub of *net.OpError
// does not reach.
func TestExtenderReportErrorKeepsAddressesFromEveryStage(t *testing.T) {
	stages := testExtenderErrorStages(t)
	for _, stage := range append(slices.Clone(testExtenderWholeErrorStages),
		"dns query", "dns answer", "header length", "h3 serve", "outer TLS handshake", "relay",
		"probe", "proxy limit", "proxy upstream", "datagram relay read client", "lag write",
	) {
		if !slices.Contains(stages, stage) {
			t.Fatalf("the sources report no stage %q, in %q", stage, stages)
		}
	}
	stages = append(stages, "a stage nobody has named yet")

	server, recorder := newTestErrorServer(t, nil, nil)
	for name, err := range testErrorsNamingAClient() {
		named := false
		for _, reading := range testErrorReadings(err) {
			named = named || strings.Contains(reading, "203.0.113.7") || strings.Contains(reading, "2001:db8:1234::7")
		}
		if !named {
			t.Fatalf("%s: the case names no address: %v", name, err)
		}
		for _, stage := range stages {
			t.Run(stage+"/"+name, func(t *testing.T) {
				server.reportError(stage, err)
				reports := recorder.allReports()
				report := reports[len(reports)-1]
				if report.stage != stage {
					t.Fatalf("the stage = %q, expected %q", report.stage, stage)
				}
				if slices.Contains(testExtenderWholeErrorStages, stage) {
					if report.err != err {
						t.Fatalf("the stage heard %v, expected its whole error %v", report.err, err)
					}
					return
				}
				testExpectErrorHoldsNone(t, report.err, testErrorNotHeard...)
			})
		}
	}
}

// reportError is the one way to the handler, a stage is a constant, a fixed error is
// converted from a literal and holds nothing, and no site builds a client's address into
// text at all, whether or not that text is on its way to the handler (the error of a
// refused quic Initial goes to quic-go, which drops it).
func TestExtenderErrorSources(t *testing.T) {
	fileSet, files := parseTestExtenderSources(t)
	// variables and calls that hold a client's address
	addressNames := []string{"source", "clientAddress", "remoteAddress", "remoteAddr", "RemoteAddr", "clientAddr"}
	addressFuncs := []string{"connectionSource", "connectionSourceAddress", "remoteAddressString"}
	// what builds or writes text
	textFuncs := []string{
		"Errorf", "Sprintf", "Sprint", "Sprintln", "Printf", "Print", "Println",
		"Infof", "Warningf", "Fatalf", "Join",
	}
	violations := []string{}
	violate := func(pos token.Pos, what string) {
		violations = append(violations, fmt.Sprintf("%s: %s", fileSet.Position(pos), what))
	}
	forEachTestExtenderFunc(files, func(funcDecl *ast.FuncDecl) {
		ast.Inspect(funcDecl.Body, func(node ast.Node) bool {
			switch n := node.(type) {
			case *ast.SelectorExpr:
				if n.Sel.Name == "ErrorHandler" && funcDecl.Name.Name != "reportError" {
					violate(n.Pos(), "reaches ErrorHandler outside reportError")
				}
			case *ast.CallExpr:
				name := testExtenderCallName(n)
				if name == "extenderFixedError" {
					if len(n.Args) != 1 {
						violate(n.Pos(), "a fixed error of no one literal")
					} else if literal, ok := n.Args[0].(*ast.BasicLit); !ok || literal.Kind != token.STRING {
						violate(n.Pos(), "a fixed error that is not converted from a string literal")
					}
				}
				if slices.Contains(textFuncs, name) {
					for _, arg := range n.Args {
						ast.Inspect(arg, func(argNode ast.Node) bool {
							switch a := argNode.(type) {
							case *ast.Ident:
								if slices.Contains(addressNames, a.Name) || slices.Contains(addressFuncs, a.Name) {
									violate(a.Pos(), fmt.Sprintf("%s builds %s into text", name, a.Name))
								}
							case *ast.SelectorExpr:
								if slices.Contains(addressNames, a.Sel.Name) {
									violate(a.Pos(), fmt.Sprintf("%s builds %s into text", name, a.Sel.Name))
								}
							}
							return true
						})
					}
				}
			}
			return true
		})
	})
	if 0 < len(violations) {
		t.Fatalf("%d violations:\n%s", len(violations), strings.Join(violations, "\n"))
	}
}

// The limits of the dns forwarder, which built the source's address into their text.
func TestExtenderDnsLimitsReportNoAddress(t *testing.T) {
	for _, client := range []*net.UDPAddr{
		{IP: testErrorClient.IP, Port: testErrorClient.Port},
		{IP: testErrorClient6.IP, Port: testErrorClient6.Port},
	} {
		t.Run(client.IP.String(), func(t *testing.T) {
			release := make(chan struct{})
			started := make(chan struct{}, 1)
			server, recorder := newTestErrorServer(t, nil, func(settings *ExtenderSettings) {
				settings.DnsForwardWorkerCount = 1
				// held for as long as the test needs it
				settings.DnsForwardTimeout = 0
				settings.DnsForward = func(ctx context.Context, _ dnsmessage.Type, _ string) ([]byte, bool) {
					started <- struct{}{}
					select {
					case <-release:
					case <-ctx.Done():
					}
					return nil, false
				}
			})
			defer close(release)
			forwarder := newExtenderDnsForwarder(server, newRecordingPacketConn())
			defer forwarder.close()

			queryBytes := newTestDnsQuery(t, 0x7171, testForwardName, dnsmessage.TypeA)
			forwarder.handleQuery(queryBytes, client)
			select {
			case <-started:
			case <-time.After(10 * time.Second):
				t.Fatal("the first query never reached the forward")
			}
			// the same source, another port: refused for its query in flight
			forwarder.handleQuery(queryBytes, &net.UDPAddr{IP: client.IP, Port: client.Port + 1})
			reports := recorder.stageReports("dns query")
			if len(reports) != 1 {
				t.Fatalf("heard %v, expected the query refused", reports)
			}
			testExpectErrorHoldsNone(t, reports[0].err, client.IP.String(), strconv.Itoa(client.Port))

			// its rate: a state with no tokens left, in a forwarder of its own
			rateServer, rateRecorder := newTestErrorServer(t, nil, func(settings *ExtenderSettings) {
				settings.DnsMaxQueryBurstPerSource = 1
				settings.DnsMaxQueryRatePerSource = 0.001
				settings.DnsMaxQueryRate = 0
				settings.DnsForward = func(context.Context, dnsmessage.Type, string) ([]byte, bool) {
					return nil, false
				}
			})
			rateForwarder := newExtenderDnsForwarder(rateServer, newRecordingPacketConn())
			defer rateForwarder.close()
			now := time.Now()
			source := connectionSourceAddress(client.String())
			if err := rateForwarder.admit(source, now); err != nil {
				t.Fatal(err)
			}
			rateForwarder.release(source)
			rateForwarder.handleQuery(queryBytes, client)
			rateReports := rateRecorder.stageReports("dns query")
			if len(rateReports) != 1 {
				t.Fatalf("heard %v, expected the query over its rate", rateReports)
			}
			testExpectErrorHoldsNone(t, rateReports[0].err, client.IP.String(), strconv.Itoa(client.Port))
		})
	}
}

// A query that did not resolve said which name, which is the client's own.
func TestExtenderDnsFailureReportsNoName(t *testing.T) {
	reported := make(chan error, 1)
	server, _ := newTestErrorServer(t, nil, func(settings *ExtenderSettings) {
		settings.ErrorHandler = func(stage string, err error) {
			if stage != "dns query" {
				t.Errorf("heard %q: %v", stage, err)
			}
			reported <- err
		}
		settings.DnsForward = func(context.Context, dnsmessage.Type, string) ([]byte, bool) {
			return nil, false
		}
	})
	forwarder := newExtenderDnsForwarder(server, newRecordingPacketConn())
	defer forwarder.close()

	forwarder.handleQuery(
		newTestDnsQuery(t, 0x7272, "client-names-this.example.", dnsmessage.TypeA),
		&net.UDPAddr{IP: testErrorClient.IP, Port: testErrorClient.Port},
	)
	select {
	case err := <-reported:
		testExpectErrorHoldsNone(t, err, "client-names-this", "203.0.113.7")
	case <-time.After(10 * time.Second):
		t.Fatal("the failed query was not reported")
	}
}

// One extender request of a client at testErrorClient, through the handler every
// carrier shares.
func testErrorExtenderRequest(t *testing.T, header *protocol.ExtenderHeader) *http.Request {
	t.Helper()
	headerBytes, err := proto.Marshal(header)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "https://"+testServerName+"/", bytes.NewReader(headerBytes))
	req.Header.Set("Content-Type", connect.ExtenderContentType)
	req.RemoteAddr = testErrorClient.String()
	return req
}

// The probe limit, which built the source's address into its text, and the destination
// a client asked for, which may be an address too.
func TestExtenderRefusalsOfARequestReportNoAddress(t *testing.T) {
	server, recorder := newTestErrorServer(t, []string{"dest.example"}, func(settings *ExtenderSettings) {
		settings.ProbeMaxRatePerSource = 0.001
		settings.ProbeMaxBurstPerSource = 1
	})
	handler := &extenderHandler{server: server}

	probeHeader := &protocol.ExtenderHeader{Service: connect.ExtenderServiceProbe}
	// the first is admitted, and the recorder is not a stream to take over
	handler.ServeHTTP(httptest.NewRecorder(), testErrorExtenderRequest(t, probeHeader))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, testErrorExtenderRequest(t, probeHeader))
	if response.Code != http.StatusForbidden {
		t.Fatalf("the second probe got status %d, expected a refusal", response.Code)
	}
	probeReports := recorder.stageReports("probe")
	if len(probeReports) != 1 {
		t.Fatalf("heard %v, expected the probe over its rate", recorder.allReports())
	}
	testExpectErrorHoldsNone(t, probeReports[0].err, testErrorNotHeard...)

	response = httptest.NewRecorder()
	handler.ServeHTTP(response, testErrorExtenderRequest(t, &protocol.ExtenderHeader{
		DestinationHost: testErrorClient.IP.String(),
		DestinationPort: 443,
	}))
	if response.Code != http.StatusForbidden {
		t.Fatalf("the destination got status %d, expected a refusal", response.Code)
	}
	destinationReports := recorder.stageReports("destination authorization")
	if len(destinationReports) != 1 {
		t.Fatalf("heard %v, expected the destination refused", recorder.allReports())
	}
	testExpectErrorHoldsNone(t, destinationReports[0].err, testErrorNotHeard...)
}

// The proxy's bound, which built the source's address into its text, and a server name
// off its whitelist, which is whatever the client sent.
func TestExtenderRefusalsOfAProxyRequestReportNoAddress(t *testing.T) {
	server, recorder := newTestErrorServer(t, []string{"dest.example"}, func(settings *ExtenderSettings) {
		settings.ProxyMaxConnectionCountPerSource = 1
	})
	handler := &extenderHandler{server: server}
	proxyRequest := func(serverName string) *http.Request {
		req := httptest.NewRequest(http.MethodGet, "https://"+serverName+"/", nil)
		req.RemoteAddr = testErrorClient.String()
		return req.WithContext(newExtenderRequestContext(req.Context(), serverName))
	}

	if !server.proxy.beginConnection(testErrorClient.IP.String()) {
		t.Fatal("the held slot was refused")
	}
	defer server.proxy.endConnection(testErrorClient.IP.String())
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, proxyRequest("dest.example"))
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("got status %d, expected the bound", response.Code)
	}
	limitReports := recorder.stageReports("proxy limit")
	if len(limitReports) != 1 {
		t.Fatalf("heard %v, expected the proxy limit", recorder.allReports())
	}
	testExpectErrorHoldsNone(t, limitReports[0].err, testErrorNotHeard...)

	response = httptest.NewRecorder()
	handler.ServeHTTP(response, proxyRequest(testErrorClient.IP.String()))
	if response.Code != http.StatusForbidden {
		t.Fatalf("got status %d, expected the whitelist", response.Code)
	}
	nameReports := recorder.stageReports("proxy name")
	if len(nameReports) != 1 {
		t.Fatalf("heard %v, expected the name refused", recorder.allReports())
	}
	testExpectErrorHoldsNone(t, nameReports[0].err, testErrorNotHeard...)
}

// What real sockets say: a client that connects and never finishes a handshake or sends
// a header ends with an error naming both ends of its socket. Nothing of the client
// reaches the handler, and what does reach it still says it was a timeout.
func TestExtenderRealSocketFailuresReportNoAddress(t *testing.T) {
	for _, c := range []struct {
		description string
		handshake   bool
		// the second of two only when the one budget of both (A9) ran out in the handshake
		stages []string
	}{
		{
			description: "a handshake that never comes",
			stages:      []string{"outer TLS handshake"},
		},
		{
			description: "a header that never comes",
			handshake:   true,
			stages:      []string{"header length", "outer TLS handshake"},
		},
	} {
		t.Run(c.description, func(t *testing.T) {
			fixture := newExtenderFixture(t, "127.0.0.1", func(settings *ExtenderSettings) {
				settings.HeaderTimeout = time.Second
			})
			conn, err := (&net.Dialer{}).Dial("tcp", fixture.authority(fixture.tcpPort))
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			_, portString, err := net.SplitHostPort(conn.LocalAddr().String())
			if err != nil {
				t.Fatal(err)
			}
			if c.handshake {
				// however it ends, it is the server's report that is read
				tls.Client(conn, &tls.Config{
					ServerName:         testServerName,
					InsecureSkipVerify: true,
					MinVersion:         tls.VersionTLS13,
				}).Handshake()
			}
			// the fixture's handler keeps the stage in front of what it heard
			reported, ok := fixture.nextError()
			if !ok {
				t.Fatal("the extender did not report the client it dropped")
			}
			if !slices.ContainsFunc(c.stages, func(stage string) bool {
				return strings.HasPrefix(reported.Error(), stage+":")
			}) {
				t.Fatalf("heard %v, expected one of the stages %q", reported, c.stages)
			}
			heard := errors.Unwrap(reported)
			testExpectErrorHoldsNone(t, heard, portString, "127.0.0.1")
			if !errors.Is(heard, os.ErrDeadlineExceeded) {
				t.Fatalf("heard %v, expected a timeout", heard)
			}
			for _, otherErr := range drainFixtureErrors(fixture) {
				testExpectErrorHoldsNone(t, errors.Unwrap(otherErr), portString, "127.0.0.1")
			}
		})
	}
}

// A dial whose error quotes what it was asked to dial, as a net.Dialer's does.
func testErrorQuotingDial(ctx context.Context, network string, address string) (net.Conn, error) {
	return nil, &net.OpError{Op: "dial", Net: network, Addr: testErrorAddrString(address), Err: errors.New("the egress is down")}
}

// An address that is the text it was dialed as.
type testErrorAddrString string

func (self testErrorAddrString) Network() string {
	return "tcp"
}

func (self testErrorAddrString) String() string {
	return string(self)
}

// The evidence for the stages that keep their whole error: the dial is told only the
// operator's destination or hop, so what it says holds nothing of the client, which here
// is a real socket with a port of its own. It stays the whole error, which an operator's
// log needs: the destination, the family and the cause.
func TestExtenderDialStagesHoldNoClientAddress(t *testing.T) {
	const destination = "dest.example:443"

	// settings that keep the port the client dials from, and a read of it
	localPort := func(t *testing.T, connectSettings *connect.ConnectSettings) (*connect.ConnectSettings, func() string) {
		var stateLock sync.Mutex
		ports := []string{}
		connectSettings.DialContextSettings = &connect.DialContextSettings{
			DialContext: func(ctx context.Context, network string, address string) (net.Conn, error) {
				conn, err := (&net.Dialer{}).DialContext(ctx, network, address)
				if err == nil {
					_, port, splitErr := net.SplitHostPort(conn.LocalAddr().String())
					if splitErr != nil {
						t.Error(splitErr)
					}
					stateLock.Lock()
					defer stateLock.Unlock()
					ports = append(ports, port)
				}
				return conn, err
			},
		}
		return connectSettings, func() string {
			stateLock.Lock()
			defer stateLock.Unlock()
			if len(ports) != 1 {
				t.Fatalf("the client dialed from ports %v, expected one", ports)
			}
			return ports[0]
		}
	}

	t.Run("forward dial", func(t *testing.T) {
		fixture := newExtenderFixture(t, "127.0.0.1", func(settings *ExtenderSettings) {
			settings.DialContext = testErrorQuotingDial
		})
		connectSettings, clientPort := localPort(t, fixture.connectSettings())
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		conn, _, err := connect.DialExtender(ctx, connectSettings, fixture.extenderConfig(connect.ExtenderCarrierTcp), &connect.ExtenderDial{
			DestinationHost: "dest.example",
			DestinationPort: 443,
		})
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		reported := waitForFixtureError(t, fixture, "forward dial")
		if !strings.Contains(reported.Error(), destination) || !strings.Contains(reported.Error(), "tcp4") {
			t.Fatalf("heard %v, expected the destination and the family", reported)
		}
		if strings.Contains(reported.Error(), clientPort()) {
			t.Fatalf("heard %v, which names the client's port", reported)
		}
	})

	t.Run("datagram forward dial", func(t *testing.T) {
		fixture := newExtenderFixture(t, "127.0.0.1", func(settings *ExtenderSettings) {
			settings.DialPacketContext = testErrorQuotingDial
		})
		connectSettings, clientPort := localPort(t, fixture.connectSettings())
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		conn, _, err := connect.DialExtender(ctx, connectSettings, fixture.extenderConfig(connect.ExtenderCarrierTcp), &connect.ExtenderDial{
			DestinationHost: "dest.example",
			DestinationPort: 443,
			Datagram:        true,
		})
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		reported := waitForFixtureError(t, fixture, "datagram forward dial")
		if !strings.Contains(reported.Error(), destination) || !strings.Contains(reported.Error(), "udp4") {
			t.Fatalf("heard %v, expected the destination and the family", reported)
		}
		if strings.Contains(reported.Error(), clientPort()) {
			t.Fatalf("heard %v, which names the client's port", reported)
		}
	})

	t.Run("nlayer hop dial", func(t *testing.T) {
		fixture := newExtenderFixture(t, "127.0.0.1", func(settings *ExtenderSettings) {
			settings.DialContext = testErrorQuotingDial
			settings.NLayerHops = []*connect.ExtenderConfig{testErrorHop}
			// the hop is dialed with nothing read of the inner stream first
			settings.NLayerClientHelloTimeout = 0
		})
		connectSettings, clientPort := localPort(t, fixture.connectSettings())
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		conn, _, err := connect.DialExtender(ctx, connectSettings, fixture.extenderConfig(connect.ExtenderCarrierTcp), &connect.ExtenderDial{
			DestinationHost: "dest.example",
			DestinationPort: 443,
		})
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		reported := waitForFixtureError(t, fixture, "nlayer hop dial")
		testExpectHopDialHoldsNoClient(t, reported, clientPort())
	})

	t.Run("nlayer probe dial", func(t *testing.T) {
		fixture := newExtenderFixture(t, "127.0.0.1", func(settings *ExtenderSettings) {
			settings.DialContext = testErrorQuotingDial
			settings.NLayerHops = []*connect.ExtenderConfig{testErrorHop}
		})
		connectSettings, clientPort := localPort(t, fixture.connectSettings())
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		_, _, err := connect.DialExtender(ctx, connectSettings, fixture.extenderConfig(connect.ExtenderCarrierTcp), &connect.ExtenderDial{
			Service: connect.ExtenderServiceProbe,
		})
		var refusedErr *connect.ExtenderRefusedError
		if !errors.As(err, &refusedErr) {
			t.Fatalf("the probe ended with %v, expected a refusal", err)
		}
		reported := waitForFixtureError(t, fixture, "nlayer probe dial")
		testExpectHopDialHoldsNoClient(t, reported, clientPort())
	})
}

// A hop that is only ever dialed through testErrorQuotingDial.
var testErrorHop = &connect.ExtenderConfig{
	Profile: connect.ExtenderProfile{
		ConnectMode: connect.ExtenderConnectModeTcpTls,
		ServerName:  testServerName,
		Port:        9443,
	},
	Ip: netip.MustParseAddr("192.0.2.9"),
}

// What a hop dial's report holds: the hop and the dial's error, whole, not the client.
func testExpectHopDialHoldsNoClient(t *testing.T, reported error, clientPort string) {
	t.Helper()
	var opErr *net.OpError
	if !errors.As(reported, &opErr) || !strings.Contains(reported.Error(), "192.0.2.9:9443") {
		t.Fatalf("heard %v, expected the dial's error naming the hop", reported)
	}
	if strings.Contains(reported.Error(), clientPort) {
		t.Fatalf("heard %v, which names the client's port", reported)
	}
}

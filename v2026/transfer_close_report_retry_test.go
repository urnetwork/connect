// A closing client's close reports leave through one out-of-band request
// each, once native delivery has lost the client lifecycle. These regressions
// drive the real ContractManager, ApiOutOfBandControl, client strategy and
// window retirement over a synthetic platform transport. Every request and
// identity is local, and every wait runs on the synctest clock.
package connect

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/urnetwork/connect/v2026/protocol"
)

// One answer of the synthetic platform to a close report's control request.
type closeReportAnswer func(request *http.Request) (*http.Response, error)

// Answers with a status and body, as the api or its load balancer would.
func closeReportStatusAnswer(status int, body string) closeReportAnswer {
	return func(request *http.Request) (*http.Response, error) {
		return authObservationTestResponse(request, status, body), nil
	}
}

// Processes the report: the server answers a close with an empty pack.
func closeReportProcessedAnswer() closeReportAnswer {
	return closeReportStatusAnswer(http.StatusOK, `{"pack":""}`)
}

// Takes the written request and never answers: a black-holed route, which the
// strategy gives up at its RequestTimeout.
func closeReportBlackHoleAnswer(request *http.Request) (*http.Response, error) {
	<-request.Context().Done()
	return nil, request.Context().Err()
}

// The close reports in one control pack.
func closeReportsFromPack(pack string) ([]*protocol.CloseContract, error) {
	packBytes, err := base64.StdEncoding.DecodeString(pack)
	if err != nil {
		return nil, err
	}
	decoded := &protocol.Pack{}
	if err := proto.Unmarshal(packBytes, decoded); err != nil {
		return nil, err
	}
	reports := []*protocol.CloseContract{}
	for _, frame := range decoded.Frames {
		message, err := FromFrame(frame)
		if err != nil {
			return nil, err
		}
		if report, ok := message.(*protocol.CloseContract); ok {
			reports = append(reports, report)
		}
	}
	return reports, nil
}

// The synthetic platform. It answers each close report request with the next
// scripted answer, the last one repeating, and records the request's pack and
// its answer in arrival order. Other control, such as the client's key
// publication, and route discovery are processed without a record.
type closeReportPlatform struct {
	t       *testing.T
	answers []closeReportAnswer

	stateLock sync.Mutex
	packs     []string
	// "control <status>" or "control no answer" for each report request, and
	// the identity retirement's "remove", in order
	events     []string
	eventTimes []time.Time
}

func (self *closeReportPlatform) RoundTrip(request *http.Request) (*http.Response, error) {
	authObservationTestWrote(request)
	if request.Method == http.MethodGet && request.URL.Path == "/hello" {
		return authObservationTestResponse(request, http.StatusOK, ""), nil
	}
	if request.Method != http.MethodPost || request.URL.Path != "/connect/control" {
		self.t.Errorf("unexpected platform request %s %s", request.Method, request.URL.Path)
		return nil, errors.New("unexpected platform request")
	}
	body, err := io.ReadAll(request.Body)
	if err != nil {
		return nil, err
	}
	var args ConnectControlArgs
	if err := json.Unmarshal(body, &args); err != nil {
		self.t.Errorf("control request is not json: %v", err)
		return nil, err
	}
	reports, err := closeReportsFromPack(args.Pack)
	if err != nil {
		self.t.Errorf("control pack does not decode: %v", err)
		return nil, err
	}
	if len(reports) == 0 {
		return closeReportProcessedAnswer()(request)
	}
	self.stateLock.Lock()
	answer := self.answers[min(len(self.packs), len(self.answers)-1)]
	self.packs = append(self.packs, args.Pack)
	self.stateLock.Unlock()
	response, err := answer(request)
	event := "control no answer"
	if response != nil {
		event = fmt.Sprintf("control %d", response.StatusCode)
	}
	self.record(event)
	return response, err
}

// Records one ordered event at the virtual time it happened.
func (self *closeReportPlatform) record(event string) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.events = append(self.events, event)
	self.eventTimes = append(self.eventTimes, time.Now())
}

func (self *closeReportPlatform) snapshot() (packs []string, events []string, eventTimes []time.Time) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return slices.Clone(self.packs), slices.Clone(self.events), slices.Clone(self.eventTimes)
}

// Keeps the contract manager's cleanup outcome lines and drops every other
// line. Level 1 is enabled, so the success line is kept.
type closeReportOutcomeLogger struct {
	stateLock sync.Mutex
	lines     []string
}

// Takes the arguments as a slice: a variadic forward into fmt would make vet
// treat the whole Logger interface as printf-like (see recordingLogger).
func (self *closeReportOutcomeLogger) record(format string, args []any) {
	line := strings.TrimRight(fmt.Sprintf(format, args...), "\n")
	if !strings.Contains(line, "after client close") {
		return
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.lines = append(self.lines, line)
}

func (self *closeReportOutcomeLogger) snapshot() []string {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return slices.Clone(self.lines)
}

func (self *closeReportOutcomeLogger) Info(args ...any) {}

func (self *closeReportOutcomeLogger) Infof(format string, args ...any) {
	self.record(format, args)
}

func (self *closeReportOutcomeLogger) Warningf(format string, args ...any) {}

func (self *closeReportOutcomeLogger) Errorf(format string, args ...any) {}

func (self *closeReportOutcomeLogger) V(level int32) Verbose {
	return closeReportOutcomeVerbose{log: self, enabled: level <= 1}
}

type closeReportOutcomeVerbose struct {
	log     *closeReportOutcomeLogger
	enabled bool
}

func (self closeReportOutcomeVerbose) Enabled() bool { return self.enabled }

func (self closeReportOutcomeVerbose) Info(args ...any) {}

func (self closeReportOutcomeVerbose) Infof(format string, args ...any) {
	if self.enabled {
		self.log.record(format, args)
	}
}

// Transfer-only lifecycle settings whose contract manager keeps its provide
// ping parked and logs through log.
func closeReportRetryClientSettings(log Logger) *ClientSettings {
	settings := closeWaitClientSettings()
	settings.ContractManagerSettings = DefaultContractManagerSettingsNoNetworkEvents()
	settings.ContractManagerSettings.ProvidePingTimeout = time.Hour
	settings.Log = log
	return settings
}

// Requires the platform's events to be wantEvents and every report request to
// carry the same bytes: the one report for contractId.
func requireCloseReportRequests(t *testing.T, name string, platform *closeReportPlatform, contractId Id, wantEvents []string) {
	t.Helper()
	packs, events, _ := platform.snapshot()
	if !slices.Equal(events, wantEvents) {
		t.Fatalf("%s: report requests %q, want %q", name, events, wantEvents)
	}
	for _, pack := range packs {
		if pack != packs[0] {
			t.Fatalf("%s: a retry changed the report bytes", name)
		}
	}
	reports, err := closeReportsFromPack(packs[0])
	if err != nil || len(reports) != 1 {
		t.Fatalf("%s: report request carries %d reports: %v", name, len(reports), err)
	}
	report := reports[0]
	reportContractId, err := IdFromBytes(report.ContractId)
	if err != nil || reportContractId != contractId || report.AckedByteCount != 121 ||
		report.UnackedByteCount != 7 || report.Checkpoint || len(report.ReportId) != 16 {
		t.Fatalf("%s: request carries another report: %v", name, report)
	}
}

// Requires the contract manager to have logged exactly one outcome for the
// report: its success or its failure.
func requireCloseReportOutcome(t *testing.T, name string, outcomes *closeReportOutcomeLogger, contractId Id, succeeded bool) {
	t.Helper()
	lines := outcomes.snapshot()
	want := fmt.Sprintf("[contract]closed %s after client close", contractId)
	if !succeeded {
		want = fmt.Sprintf("[contract]could not close %s after client close = ", contractId)
	}
	if len(lines) != 1 || !strings.HasPrefix(lines[0], want) {
		t.Fatalf("%s: report outcomes %q, want one %q", name, lines, want)
	}
}

// A closing client's report meets one transient failure: a 5xx or 429 from
// the api or its load balancer, a 408, or a route that never answers within
// the strategy's RequestTimeout. The cleanup sends the same bytes again and
// the platform processes the report once. Before the fix the cleanup was one
// attempt, and each of these failures lost the report for good.
func TestCloseReportCleanupRetriesTransientFailureThenDeliversOnce(t *testing.T) {
	GetMessagePoolAggregateStats()
	synctest.Test(t, func(t *testing.T) {
		for _, c := range []struct {
			name    string
			failure closeReportAnswer
			event   string
			// The report is made while the client is live, so its native
			// delivery owns it until the client closes and hands it to cleanup.
			liveAtReport bool
		}{
			{name: "503", failure: closeReportStatusAnswer(http.StatusServiceUnavailable, "no healthy upstream"), event: "control 503"},
			{name: "502", failure: closeReportStatusAnswer(http.StatusBadGateway, "bad gateway"), event: "control 502"},
			{name: "504", failure: closeReportStatusAnswer(http.StatusGatewayTimeout, "gateway timeout"), event: "control 504"},
			{name: "500", failure: closeReportStatusAnswer(http.StatusInternalServerError, "internal error"), event: "control 500"},
			{name: "429", failure: closeReportStatusAnswer(http.StatusTooManyRequests, "too many requests"), event: "control 429"},
			{name: "408", failure: closeReportStatusAnswer(http.StatusRequestTimeout, "request timeout"), event: "control 408"},
			{name: "no answer", failure: closeReportBlackHoleAnswer, event: "control no answer"},
			{name: "503 after native delivery", failure: closeReportStatusAnswer(http.StatusServiceUnavailable, "no healthy upstream"), event: "control 503", liveAtReport: true},
		} {
			platform := &closeReportPlatform{t: t, answers: []closeReportAnswer{c.failure, closeReportProcessedAnswer()}}
			outcomes := &closeReportOutcomeLogger{}
			api, strategy := authObservationTestApi(t.Context(), nil, true, platform)
			oob := newApiOutOfBandControl(t.Context(), strategy, "synthetic-window-token", "https://api.close-report.example", false)
			client := NewClient(t.Context(), NewId(), oob, closeReportRetryClientSettings(outcomes))
			contractId := NewId()
			if c.liveAtReport {
				client.ContractManager().CloseContract(contractId, 121, 7)
				client.Close()
			} else {
				client.Close()
				client.ContractManager().CloseContract(contractId, 121, 7)
			}
			// the owner's order: the client's report producers, then the
			// control's admitted requests
			if err := client.CloseAndWait(t.Context()); err != nil {
				t.Fatalf("%s: client join: %v", c.name, err)
			}
			if err := oob.CloseAndWait(t.Context()); err != nil {
				t.Fatalf("%s: control join: %v", c.name, err)
			}
			api.Close()
			requireCloseReportRequests(t, c.name, platform, contractId, []string{c.event, "control 200"})
			requireCloseReportOutcome(t, c.name, outcomes, contractId, true)
		}
	})
}

// A definitive answer ends a report's cleanup, also after a retry: a 401 from a
// retired identity, another 4xx, or a processed response that rejects the
// frame, such as an unknown or settled contract. The synthetic platform would
// process any later attempt, so a retry past the answer shows as one more
// request and a success. Before the fix the first 503 already ended the
// cleanup.
func TestCloseReportCleanupNeverRetriesDefinitiveAnswer(t *testing.T) {
	GetMessagePoolAggregateStats()
	synctest.Test(t, func(t *testing.T) {
		for _, c := range []struct {
			name   string
			answer closeReportAnswer
			event  string
		}{
			{name: "401", answer: closeReportStatusAnswer(http.StatusUnauthorized, "invalid client token"), event: "control 401"},
			{name: "403", answer: closeReportStatusAnswer(http.StatusForbidden, "forbidden"), event: "control 403"},
			{name: "404", answer: closeReportStatusAnswer(http.StatusNotFound, "not found"), event: "control 404"},
			{name: "400", answer: closeReportStatusAnswer(http.StatusBadRequest, "bad request"), event: "control 400"},
			{name: "402", answer: closeReportStatusAnswer(http.StatusPaymentRequired, "payment required"), event: "control 402"},
			{name: "unknown contract", answer: closeReportStatusAnswer(http.StatusOK, `{"pack":"","error":{"message":"Contract not found"}}`), event: "control 200"},
			{name: "settled contract", answer: closeReportStatusAnswer(http.StatusOK, `{"pack":"","error":{"message":"contract already settled"}}`), event: "control 200"},
		} {
			for _, afterRetry := range []bool{false, true} {
				name := fmt.Sprintf("%s after retry %t", c.name, afterRetry)
				answers := []closeReportAnswer{c.answer, closeReportProcessedAnswer()}
				wantEvents := []string{c.event}
				if afterRetry {
					answers = append([]closeReportAnswer{closeReportStatusAnswer(http.StatusServiceUnavailable, "no healthy upstream")}, answers...)
					wantEvents = []string{"control 503", c.event}
				}
				platform := &closeReportPlatform{t: t, answers: answers}
				outcomes := &closeReportOutcomeLogger{}
				api, strategy := authObservationTestApi(t.Context(), nil, true, platform)
				oob := newApiOutOfBandControl(t.Context(), strategy, "synthetic-window-token", "https://api.close-report.example", false)
				client := NewClient(t.Context(), NewId(), oob, closeReportRetryClientSettings(outcomes))
				client.Close()
				contractId := NewId()
				client.ContractManager().CloseContract(contractId, 121, 7)
				if err := client.CloseAndWait(t.Context()); err != nil {
					t.Fatalf("%s: client join: %v", name, err)
				}
				if err := oob.CloseAndWait(t.Context()); err != nil {
					t.Fatalf("%s: control join: %v", name, err)
				}
				api.Close()
				requireCloseReportRequests(t, name, platform, contractId, wantEvents)
				requireCloseReportOutcome(t, name, outcomes, contractId, false)
			}
		}
	})
}

// A generator over the platform's transport whose identity removal is the
// platform's "remove" event.
func newCloseReportRetryGenerator(t *testing.T, platform *closeReportPlatform) *ApiMultiClientGenerator {
	t.Helper()
	api, strategy := authObservationTestApi(t.Context(), nil, true, platform)
	t.Cleanup(api.Close)
	settings := DefaultApiMultiClientGeneratorSettings()
	settings.ClientCredentials = &generatorCredentialFixture{remove: func(ctx context.Context, args *RemoveNetworkClientArgs) (*RemoveNetworkClientResult, error) {
		platform.record("remove")
		return &RemoveNetworkClientResult{}, nil
	}}
	return NewApiMultiClientGenerator(t.Context(), nil, strategy, nil,
		"https://api.close-report.example", "synthetic-parent-token", "wss://platform.close-report.example",
		"synthetic window", "synthetic", "test", nil, DefaultClientSettings, settings)
}

// Retires a window client the way its channel does and closes one contract
// while the client's join is still running, as a closing sequence does.
// prepare sees the window's control before the client is made. Returns the
// contract and the virtual time of its report.
func retireCloseReportRetryClient(
	t *testing.T,
	generator *ApiMultiClientGenerator,
	settings *ClientSettings,
	prepare func(*ApiOutOfBandControl),
) (Id, time.Time) {
	t.Helper()
	childId := NewId()
	oob := generator.newClientOob(t.Context(), "synthetic-window-token")
	if prepare != nil {
		prepare(oob)
	}
	client := NewClient(t.Context(), childId, oob, settings)
	joinEntered, joinRelease := make(chan struct{}), make(chan struct{})
	client.beforeRunDoneWaitForTest = func() {
		close(joinEntered)
		<-joinRelease
	}
	generator.RemoveClientWithArgs(client, &MultiClientGeneratorClientArgs{ClientId: childId, ClientAuth: &ClientAuth{InstanceId: NewId()}})
	client.Cancel()
	<-joinEntered
	contractId := NewId()
	reportTime := time.Now()
	client.ContractManager().CloseContract(contractId, 121, 7)
	close(joinRelease)
	return contractId, reportTime
}

// A retiring window client keeps its identity until its in-flight close report
// is processed, retries included, so the report never authenticates with a
// revoked credential (connect 6f7adbfb). Before the fix the report's one
// failed attempt released the identity, and the report was lost.
func TestWindowRetirementRevokesIdentityAfterRetriedCloseReport(t *testing.T) {
	GetMessagePoolAggregateStats()
	synctest.Test(t, func(t *testing.T) {
		platform := &closeReportPlatform{t: t, answers: []closeReportAnswer{
			closeReportStatusAnswer(http.StatusServiceUnavailable, "no healthy upstream"),
			closeReportProcessedAnswer(),
		}}
		generator := newCloseReportRetryGenerator(t, platform)
		outcomes := &closeReportOutcomeLogger{}
		contractId, _ := retireCloseReportRetryClient(t, generator, closeReportRetryClientSettings(outcomes), nil)
		if err := generator.CloseAndWait(t.Context()); err != nil {
			t.Fatalf("generator retirement: %v", err)
		}
		requireCloseReportRequests(t, "retirement", platform, contractId, []string{"control 503", "control 200", "remove"})
		requireCloseReportOutcome(t, "retirement", outcomes, contractId, true)
	})
}

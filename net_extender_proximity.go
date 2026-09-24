package connect

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/urnetwork/connect/protocol"
)

// The operator side of extender proximity (DESIGNNOTES4.md, GEOMAP §2.5): the
// continent hint a client reads, and the ping report a pinger posts.

// The hint endpoint, appended to the api url.
const ExtenderHintPath = "/network/extender-hint"

// The hint answer: the continent the operator places the caller's address on,
// upper case, the same mapping the geo dns and the record tag use; empty when
// it cannot place the caller.
type ExtenderHintResult struct {
	ContinentCode string `json:"continent_code"`
}

// GetExtenderHint reads the continent hint (DESIGNNOTES4.md §4). It carries
// no credential: the answer is derived from the caller's address, which the
// operator sees on every request anyway, and a client needs it before it has
// logged in.
func GetExtenderHint(
	ctx context.Context,
	clientStrategy *ClientStrategy,
	apiUrl string,
) (string, error) {
	if clientStrategy == nil {
		return "", fmt.Errorf("the extender hint needs a client strategy")
	}
	apiUrl = strings.TrimRight(strings.TrimSpace(apiUrl), "/")
	if apiUrl == "" {
		return "", fmt.Errorf("the extender hint needs an api url")
	}
	bodyBytes, err := HttpGetWithStrategyRaw(ctx, clientStrategy, apiUrl+ExtenderHintPath, "")
	if err != nil {
		return "", err
	}
	result := &ExtenderHintResult{}
	if err := json.Unmarshal(bodyBytes, result); err != nil {
		return "", err
	}
	return strings.ToUpper(strings.TrimSpace(result.ContinentCode)), nil
}

// The ping report endpoint, appended to the api url (GEOMAP §2.5). It takes
// both pinger kinds. The operator keeps accepting the older
// `/network/extender-latency` for the extenders already in the field, but
// nothing in this module posts to it any more: the pinger reports, not the
// target.
const ExtenderPingReportPath = "/network/ping-report"

// One attested ping as the report carries it: the fields of the signed claim,
// so the operator rebuilds the signing bytes and verifies the pinger's
// signature itself, and what became of it at the target -- the outcome, the
// target's reason and, only when co-signed, the target's co-signature, which
// the operator verifies under the target's key (GEOMAP §2.4). The json is the
// server's contract.
type ExtenderPingReport struct {
	// provider or extender
	PingerKind ExtenderPingerKind `json:"pinger_kind"`
	// the provider's client id, empty for an extender pinger
	PingerClientId string `json:"pinger_client_id"`
	// the pinging extender's identity key, empty for a provider pinger
	PingerExtenderPublicKeyHex string `json:"pinger_extender_public_key_hex"`
	TargetExtenderPublicKeyHex string `json:"target_extender_public_key_hex"`
	// base64
	ProbeNonce  string `json:"probe_nonce"`
	RttMs       uint32 `json:"rtt_ms"`
	TimestampMs uint64 `json:"timestamp_ms"`
	// base64 of the pinger's ed25519 signature
	Signature string `json:"signature"`
	// cosigned, rejected or unknown
	Outcome ExtenderPingOutcome `json:"outcome"`
	// the verdict's reason, 0 without a verdict
	Reason uint32 `json:"reason"`
	// base64 of the target's co-signature, empty unless cosigned
	Cosignature string `json:"cosignature"`
	// How many extenders the probe crossed before its target answered it: 0
	// for a direct ping, and the depth of the chain's end for a ping an
	// NLayer extender relayed, whose target is that end (GEOMAP §2.9). Only a
	// direct ping is a solver term at the operator.
	HopCount uint32 `json:"hop_count"`
}

// The transport form of one attested ping. Only a co-signed ping carries the
// co-signature: a refusal carries nothing to verify, and an acceptance whose
// co-signature did not verify is reported as the refusal it amounts to.
func ExtenderPingReportFromProto(
	attestation *protocol.ExtenderProbeAttestation,
	outcome ExtenderPingOutcome,
	verdict *protocol.ExtenderProbeVerdict,
) *ExtenderPingReport {
	report := &ExtenderPingReport{
		PingerKind:                 ExtenderProbeAttestationPingerKind(attestation),
		TargetExtenderPublicKeyHex: hex.EncodeToString(attestation.ExtenderPublicKey),
		ProbeNonce:                 base64.StdEncoding.EncodeToString(attestation.ProbeNonce),
		RttMs:                      attestation.RttMs,
		TimestampMs:                attestation.TimestampMs,
		Signature:                  base64.StdEncoding.EncodeToString(attestation.Signature),
		Outcome:                    outcome,
	}
	switch report.PingerKind {
	case ExtenderPingerKindProvider:
		if id, err := IdFromBytes(attestation.ProbeClientId); err == nil {
			report.PingerClientId = id.String()
		}
	case ExtenderPingerKindExtender:
		report.PingerExtenderPublicKeyHex = hex.EncodeToString(attestation.PingerExtenderPublicKey)
	}
	if verdict != nil {
		report.Reason = verdict.Reason
		if outcome == ExtenderPingCosigned {
			report.Cosignature = base64.StdEncoding.EncodeToString(verdict.Cosignature)
		}
	}
	return report
}

// The report of one probe, nil unless it attested: a ranking probe and a
// probe whose target issued no nonce made no claim to report. The target is
// the one the claim names, which for a relayed probe is the chain end, and the
// depth is the probe's.
func ExtenderPingReportFromProbe(probe *ExtenderLatencyProbe) *ExtenderPingReport {
	if probe == nil || !probe.Attested || probe.Attestation == nil {
		return nil
	}
	report := ExtenderPingReportFromProto(probe.Attestation, probe.Outcome, probe.Verdict)
	report.HopCount = probe.HopCount
	return report
}

// The signed claim the transport form carries, which is what the
// operator verifies. A field that does not decode -- or a pinger kind the
// identity fields contradict -- is an error here rather than a signature that
// does not verify, so the operator can say which.
func (self *ExtenderPingReport) Proto() (*protocol.ExtenderProbeAttestation, error) {
	attestation := &protocol.ExtenderProbeAttestation{
		RttMs:       self.RttMs,
		TimestampMs: self.TimestampMs,
	}
	switch self.PingerKind {
	case ExtenderPingerKindProvider:
		if self.PingerExtenderPublicKeyHex != "" {
			return nil, fmt.Errorf("a provider ping names a pinger extender key")
		}
		clientId, err := ParseId(self.PingerClientId)
		if err != nil {
			return nil, fmt.Errorf("pinger client id: %w", err)
		}
		attestation.ProbeClientId = clientId.Bytes()
	case ExtenderPingerKindExtender:
		if self.PingerClientId != "" {
			return nil, fmt.Errorf("an extender ping names a pinger client id")
		}
		pingerPublicKey, err := ParseExtenderPublicKeyHex(self.PingerExtenderPublicKeyHex)
		if err != nil {
			return nil, fmt.Errorf("pinger extender public key: %w", err)
		}
		attestation.PingerExtenderPublicKey = pingerPublicKey
	default:
		return nil, fmt.Errorf("pinger kind %q is not known", self.PingerKind)
	}
	targetPublicKey, err := hex.DecodeString(self.TargetExtenderPublicKeyHex)
	if err != nil {
		return nil, fmt.Errorf("target extender public key: %w", err)
	}
	attestation.ExtenderPublicKey = targetPublicKey
	if attestation.ProbeNonce, err = base64.StdEncoding.DecodeString(self.ProbeNonce); err != nil {
		return nil, fmt.Errorf("probe nonce: %w", err)
	}
	if attestation.Signature, err = base64.StdEncoding.DecodeString(self.Signature); err != nil {
		return nil, fmt.Errorf("signature: %w", err)
	}
	return attestation, nil
}

// The verdict the transport form records, which the operator verifies the
// co-signature of: an acceptance with its co-signature when
// co-signed, a refusal with its reason when rejected, and nil when unknown,
// since no verdict arrived. An outcome that is not one of the three, or a
// co-signature on anything but a co-signed ping, is an error.
func (self *ExtenderPingReport) VerdictProto() (*protocol.ExtenderProbeVerdict, error) {
	switch self.Outcome {
	case ExtenderPingCosigned:
		cosignature, err := base64.StdEncoding.DecodeString(self.Cosignature)
		if err != nil {
			return nil, fmt.Errorf("cosignature: %w", err)
		}
		if len(cosignature) == 0 {
			return nil, fmt.Errorf("a cosigned ping carries no cosignature")
		}
		return &protocol.ExtenderProbeVerdict{
			Accepted:    true,
			Reason:      self.Reason,
			Cosignature: cosignature,
		}, nil
	case ExtenderPingRejected:
		if self.Cosignature != "" {
			return nil, fmt.Errorf("a rejected ping carries a cosignature")
		}
		return &protocol.ExtenderProbeVerdict{
			Reason: self.Reason,
		}, nil
	case ExtenderPingUnknown:
		if self.Cosignature != "" {
			return nil, fmt.Errorf("an unknown ping carries a cosignature")
		}
		return nil, nil
	default:
		return nil, fmt.Errorf("ping outcome %q is not known", self.Outcome)
	}
}

// One report as it is posted: a batch of pings.
type ExtenderPingReportArgs struct {
	Pings []*ExtenderPingReport `json:"pings"`
}

// The operator's answer. A refusal of the whole report is a normal answer
// with `Error`, as an activation refusal is; a rejected ping inside an
// accepted report is counted, not named, because the reporting pinger can do
// nothing about it.
type ExtenderPingReportResult struct {
	Accepted int    `json:"accepted"`
	Rejected int    `json:"rejected"`
	Error    string `json:"error,omitempty"`
}

// Posts one batch under the pinger's client credential: the activation credential of an extender, the client credential
// of a provider. It is how the operator attributes the report to its pinger.
func PostExtenderPingReport(
	ctx context.Context,
	clientStrategy *ClientStrategy,
	apiUrl string,
	byJwt string,
	args *ExtenderPingReportArgs,
) (*ExtenderPingReportResult, error) {
	if clientStrategy == nil {
		return nil, fmt.Errorf("the ping report needs a client strategy")
	}
	if args == nil {
		return nil, fmt.Errorf("the ping report needs args")
	}
	apiUrl = strings.TrimRight(strings.TrimSpace(apiUrl), "/")
	if apiUrl == "" {
		return nil, fmt.Errorf("the ping report needs an api url")
	}
	return HttpPostWithStrategy(
		ctx,
		clientStrategy,
		apiUrl+ExtenderPingReportPath,
		args,
		byJwt,
		&ExtenderPingReportResult{},
		NewNoopApiCallback[*ExtenderPingReportResult](),
	)
}

// The pinger's reporter (GEOMAP §2.5): every attested ping is batched and
// posted to the operator off the probe path, so a ping never waits on the
// operator and a burst of pings costs one post.

// Where and how the reporter posts.
type ExtenderPingReporterSettings struct {
	Log Logger

	// The plain api url the report is posted to.
	ApiUrl string
	// The client jwt, read at each post so a refresh is picked up.
	ByJwt func() string
	// The strategy the post goes through. The activation's direct-only
	// strategy serves; a report may cross an extender without harm, but it
	// has no reason to.
	ClientStrategy *ClientStrategy

	// A batch is posted when it reaches MaxBatchCount, or FlushTimeout after
	// its first ping, whichever comes first.
	MaxBatchCount int
	FlushTimeout  time.Duration
	// Pings waiting beyond this are dropped oldest first: an operator that
	// cannot be reached must not grow the pinger's memory.
	MaxPendingCount int
	// Budget of one post. A failed post is retried after FlushTimeout.
	RequestTimeout time.Duration

	// When set, replaces the post. Tests observe batches through it.
	Post func(ctx context.Context, args *ExtenderPingReportArgs) (*ExtenderPingReportResult, error)
}

// A batch of 64 or 30 s, whichever first, with a thousand pings pending at
// most.
func DefaultExtenderPingReporterSettings() *ExtenderPingReporterSettings {
	return &ExtenderPingReporterSettings{
		MaxBatchCount:   64,
		FlushTimeout:    30 * time.Second,
		MaxPendingCount: 1024,
		RequestTimeout:  30 * time.Second,
	}
}

// One pinger's reporter. Safe for concurrent use: a report is queued under
// the state lock and posted by the reporter's own loop.
type ExtenderPingReporter struct {
	ctx       context.Context
	cancel    context.CancelFunc
	closeOnce sync.Once
	done      chan struct{}
	log       Logger

	settings *ExtenderPingReporterSettings

	wakeMonitor *Monitor

	stateLock sync.Mutex
	pending   []*ExtenderPingReport
	// when the oldest pending ping arrived, which starts the flush clock
	firstPendingTime time.Time
	// no post before this, set by a failed post
	retryAfterTime time.Time
	postCount      int
}

// The reporter is running when this returns.
func NewExtenderPingReporter(
	ctx context.Context,
	settings *ExtenderPingReporterSettings,
) *ExtenderPingReporter {
	if settings == nil {
		settings = DefaultExtenderPingReporterSettings()
	}
	copied := *settings
	defaults := DefaultExtenderPingReporterSettings()
	if copied.MaxBatchCount <= 0 {
		copied.MaxBatchCount = defaults.MaxBatchCount
	}
	if copied.FlushTimeout <= 0 {
		copied.FlushTimeout = defaults.FlushTimeout
	}
	if copied.MaxPendingCount < copied.MaxBatchCount {
		copied.MaxPendingCount = max(copied.MaxBatchCount, defaults.MaxPendingCount)
	}
	if copied.RequestTimeout <= 0 {
		copied.RequestTimeout = defaults.RequestTimeout
	}
	settings = &copied

	cancelCtx, cancel := context.WithCancel(ctx)
	self := &ExtenderPingReporter{
		ctx:         cancelCtx,
		cancel:      cancel,
		done:        make(chan struct{}),
		log:         loggerOrDefault(settings.Log),
		settings:    settings,
		wakeMonitor: NewMonitor(),
		pending:     []*ExtenderPingReport{},
	}
	go HandleError(func() {
		defer close(self.done)
		self.run()
	}, cancel)
	return self
}

// Queues one attested ping, whatever its outcome: a refusal and a
// missing verdict are as much the record as a co-signature (GEOMAP §2.5). It
// never blocks, and beyond the pending cap it drops the oldest. A nil report,
// which is what ExtenderPingReportFromProbe makes of a probe that attested
// nothing, is ignored.
func (self *ExtenderPingReporter) Report(report *ExtenderPingReport) {
	if report == nil {
		return
	}
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if self.settings.MaxPendingCount <= len(self.pending) {
			self.pending = slices.Delete(self.pending, 0, len(self.pending)-self.settings.MaxPendingCount+1)
		}
		if len(self.pending) == 0 {
			self.firstPendingTime = time.Now()
		}
		self.pending = append(self.pending, report)
	}()
	self.wakeMonitor.NotifyAll()
}

// Pings queued and not yet posted.
func (self *ExtenderPingReporter) PendingCount() int {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return len(self.pending)
}

// Posts completed, successful or not.
func (self *ExtenderPingReporter) PostCount() int {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.postCount
}

// Ends the loop and joins it. What is still pending is dropped: it is a
// measurement, and a post on the way out would hold the shutdown.
func (self *ExtenderPingReporter) Close() {
	self.closeOnce.Do(func() {
		self.cancel()
		<-self.done
	})
}

// The post loop. It takes a batch when one is due, posts it, and otherwise
// waits for the next ping or the flush clock.
func (self *ExtenderPingReporter) run() {
	for {
		wake := self.wakeMonitor.NotifyChannel()
		batch, wait := self.takeBatch(time.Now())
		if 0 < len(batch) {
			if err := self.post(batch); err != nil {
				self.log.Infof("[extender]ping report err = %s\n", err)
				self.requeue(batch, time.Now())
			}
			// there may be more due; look again before waiting
			continue
		}
		if wait <= 0 {
			// nothing pending: only a ping or the end wakes this
			select {
			case <-self.ctx.Done():
				return
			case <-wake:
			}
			continue
		}
		select {
		case <-self.ctx.Done():
			return
		case <-wake:
		case <-time.After(wait):
		}
	}
}

// The next batch when one is due, else how long until one could be. Zero
// with no batch means nothing is pending.
func (self *ExtenderPingReporter) takeBatch(now time.Time) ([]*ExtenderPingReport, time.Duration) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	if len(self.pending) == 0 {
		return nil, 0
	}
	if now.Before(self.retryAfterTime) {
		return nil, self.retryAfterTime.Sub(now)
	}
	flushTime := self.firstPendingTime.Add(self.settings.FlushTimeout)
	if len(self.pending) < self.settings.MaxBatchCount && now.Before(flushTime) {
		return nil, flushTime.Sub(now)
	}
	count := min(len(self.pending), self.settings.MaxBatchCount)
	batch := slices.Clone(self.pending[0:count])
	self.pending = slices.Clone(self.pending[count:])
	if 0 < len(self.pending) {
		// what remains arrived after the batch's first; its clock starts now
		// rather than being owed the whole timeout again
		self.firstPendingTime = now
	}
	return batch, 0
}

// Puts a failed batch back at the front, bounded by the pending cap, and
// holds the next post for one flush timeout.
func (self *ExtenderPingReporter) requeue(batch []*ExtenderPingReport, now time.Time) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	self.pending = append(slices.Clone(batch), self.pending...)
	if self.settings.MaxPendingCount < len(self.pending) {
		self.pending = slices.Delete(self.pending, 0, len(self.pending)-self.settings.MaxPendingCount)
	}
	self.firstPendingTime = now
	self.retryAfterTime = now.Add(self.settings.FlushTimeout)
}

// Posts one batch. An operator refusal is logged and not retried: the pinger
// cannot change what it measured, and retrying would only repeat the refusal.
func (self *ExtenderPingReporter) post(batch []*ExtenderPingReport) error {
	args := &ExtenderPingReportArgs{
		Pings: slices.Clone(batch),
	}
	ctx, cancel := context.WithTimeout(self.ctx, self.settings.RequestTimeout)
	defer cancel()

	var result *ExtenderPingReportResult
	var err error
	if self.settings.Post != nil {
		result, err = self.settings.Post(ctx, args)
	} else {
		byJwt := ""
		if self.settings.ByJwt != nil {
			byJwt = self.settings.ByJwt()
		}
		result, err = PostExtenderPingReport(ctx, self.settings.ClientStrategy, self.settings.ApiUrl, byJwt, args)
	}
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.postCount += 1
	}()
	if err != nil {
		return err
	}
	if result != nil && result.Error != "" {
		self.log.Infof("[extender]ping report refused = %s\n", result.Error)
	}
	return nil
}

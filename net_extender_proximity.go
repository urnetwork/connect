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

// The operator side of extender proximity (DESIGNNOTES4.md): the continent
// hint a client reads, and the latency report an extender posts.

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

// The latency report endpoint, appended to the api url.
const ExtenderLatencyReportPath = "/network/extender-latency"

// One attestation as the report carries it: the fields of the signed
// message, so the operator rebuilds the signing bytes and verifies the
// provider's signature itself (DESIGNNOTES4.md §3). The json is the server's
// contract.
type ExtenderLatencyAttestation struct {
	// the provider client id
	ClientId             string `json:"client_id"`
	ExtenderPublicKeyHex string `json:"extender_public_key_hex"`
	// base64
	ProbeNonce  string `json:"probe_nonce"`
	RttMs       uint32 `json:"rtt_ms"`
	TimestampMs uint64 `json:"timestamp_ms"`
	// base64 of the provider's ed25519 signature
	Signature string `json:"signature"`
}

// The transport form of one attestation.
func ExtenderLatencyAttestationFromProto(attestation *protocol.ExtenderProbeAttestation) *ExtenderLatencyAttestation {
	clientId := ""
	if id, err := IdFromBytes(attestation.ProbeClientId); err == nil {
		clientId = id.String()
	}
	return &ExtenderLatencyAttestation{
		ClientId:             clientId,
		ExtenderPublicKeyHex: hex.EncodeToString(attestation.ExtenderPublicKey),
		ProbeNonce:           base64.StdEncoding.EncodeToString(attestation.ProbeNonce),
		RttMs:                attestation.RttMs,
		TimestampMs:          attestation.TimestampMs,
		Signature:            base64.StdEncoding.EncodeToString(attestation.Signature),
	}
}

// Proto is the signed message the transport form carries, which is what the
// operator verifies. A field that does not decode is an error here rather
// than a signature that does not verify, so the operator can say which.
func (self *ExtenderLatencyAttestation) Proto() (*protocol.ExtenderProbeAttestation, error) {
	clientId, err := ParseId(self.ClientId)
	if err != nil {
		return nil, fmt.Errorf("client id: %w", err)
	}
	publicKey, err := hex.DecodeString(self.ExtenderPublicKeyHex)
	if err != nil {
		return nil, fmt.Errorf("extender public key: %w", err)
	}
	nonce, err := base64.StdEncoding.DecodeString(self.ProbeNonce)
	if err != nil {
		return nil, fmt.Errorf("probe nonce: %w", err)
	}
	signature, err := base64.StdEncoding.DecodeString(self.Signature)
	if err != nil {
		return nil, fmt.Errorf("signature: %w", err)
	}
	return &protocol.ExtenderProbeAttestation{
		ProbeClientId:     clientId.Bytes(),
		ExtenderPublicKey: publicKey,
		ProbeNonce:        nonce,
		RttMs:             self.RttMs,
		TimestampMs:       self.TimestampMs,
		Signature:         signature,
	}, nil
}

type ExtenderLatencyReportArgs struct {
	Attestations []*ExtenderLatencyAttestation `json:"attestations"`
}

// The operator's answer. A refusal of the whole report is a normal answer
// with `Error`, as an activation refusal is; a rejected attestation inside an
// accepted report is counted, not named, because the reporting extender can
// do nothing about a provider's bad signature.
type ExtenderLatencyReportResult struct {
	Accepted int    `json:"accepted"`
	Rejected int    `json:"rejected"`
	Error    string `json:"error,omitempty"`
}

// PostExtenderLatencyReport posts one batch under the extender's client
// credential, the same one its activation uses, which is how the operator
// attributes the report to an extender it has activated.
func PostExtenderLatencyReport(
	ctx context.Context,
	clientStrategy *ClientStrategy,
	apiUrl string,
	byJwt string,
	args *ExtenderLatencyReportArgs,
) (*ExtenderLatencyReportResult, error) {
	if clientStrategy == nil {
		return nil, fmt.Errorf("the extender latency report needs a client strategy")
	}
	if args == nil {
		return nil, fmt.Errorf("the extender latency report needs args")
	}
	apiUrl = strings.TrimRight(strings.TrimSpace(apiUrl), "/")
	if apiUrl == "" {
		return nil, fmt.Errorf("the extender latency report needs an api url")
	}
	return HttpPostWithStrategy(
		ctx,
		clientStrategy,
		apiUrl+ExtenderLatencyReportPath,
		args,
		byJwt,
		&ExtenderLatencyReportResult{},
		NewNoopApiCallback[*ExtenderLatencyReportResult](),
	)
}

// The extender's reporter (DESIGNNOTES4.md §3): accepted attestations are
// batched and posted to the operator off the request path, so a probe never
// waits on the operator and a burst of probes costs one post.

type ExtenderLatencyReporterSettings struct {
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
	// its first attestation, whichever comes first.
	MaxBatchCount int
	FlushTimeout  time.Duration
	// Attestations waiting beyond this are dropped oldest first: an operator
	// that cannot be reached must not grow the extender's memory.
	MaxPendingCount int
	// Budget of one post. A failed post is retried after FlushTimeout.
	RequestTimeout time.Duration

	// Post, when set, replaces the post. Tests observe batches through it.
	Post func(ctx context.Context, args *ExtenderLatencyReportArgs) (*ExtenderLatencyReportResult, error)
}

func DefaultExtenderLatencyReporterSettings() *ExtenderLatencyReporterSettings {
	return &ExtenderLatencyReporterSettings{
		MaxBatchCount:   64,
		FlushTimeout:    30 * time.Second,
		MaxPendingCount: 1024,
		RequestTimeout:  30 * time.Second,
	}
}

type ExtenderLatencyReporter struct {
	ctx       context.Context
	cancel    context.CancelFunc
	closeOnce sync.Once
	done      chan struct{}
	log       Logger

	settings *ExtenderLatencyReporterSettings

	wakeMonitor *Monitor

	stateLock sync.Mutex
	pending   []*protocol.ExtenderProbeAttestation
	// when the oldest pending attestation arrived, which starts the flush
	// clock
	firstPendingTime time.Time
	// no post before this, set by a failed post
	retryAfterTime time.Time
	postCount      int
}

// The reporter is running when this returns.
func NewExtenderLatencyReporter(
	ctx context.Context,
	settings *ExtenderLatencyReporterSettings,
) *ExtenderLatencyReporter {
	if settings == nil {
		settings = DefaultExtenderLatencyReporterSettings()
	}
	copied := *settings
	defaults := DefaultExtenderLatencyReporterSettings()
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
	self := &ExtenderLatencyReporter{
		ctx:         cancelCtx,
		cancel:      cancel,
		done:        make(chan struct{}),
		log:         loggerOrDefault(settings.Log),
		settings:    settings,
		wakeMonitor: NewMonitor(),
		pending:     []*protocol.ExtenderProbeAttestation{},
	}
	go HandleError(func() {
		defer close(self.done)
		self.run()
	}, cancel)
	return self
}

// Report queues one attestation that passed the extender's gate. It is the
// extender's ProbeAttestationHandler: it never blocks, and beyond the pending
// cap it drops the oldest.
func (self *ExtenderLatencyReporter) Report(attestation *protocol.ExtenderProbeAttestation) {
	if attestation == nil {
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
		self.pending = append(self.pending, attestation)
	}()
	self.wakeMonitor.NotifyAll()
}

// Attestations queued and not yet posted.
func (self *ExtenderLatencyReporter) PendingCount() int {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return len(self.pending)
}

// Posts completed, successful or not.
func (self *ExtenderLatencyReporter) PostCount() int {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.postCount
}

// Ends the loop and joins it. What is still pending is dropped: it is a
// measurement, and a post on the way out would hold the shutdown.
func (self *ExtenderLatencyReporter) Close() {
	self.closeOnce.Do(func() {
		self.cancel()
		<-self.done
	})
}

// The post loop. It takes a batch when one is due, posts it, and otherwise
// waits for the next attestation or the flush clock.
func (self *ExtenderLatencyReporter) run() {
	for {
		wake := self.wakeMonitor.NotifyChannel()
		batch, wait := self.takeBatch(time.Now())
		if 0 < len(batch) {
			if err := self.post(batch); err != nil {
				self.log.Infof("[extender]latency report err = %s\n", err)
				self.requeue(batch, time.Now())
			}
			// there may be more due; look again before waiting
			continue
		}
		if wait <= 0 {
			// nothing pending: only an attestation or the end wakes this
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
func (self *ExtenderLatencyReporter) takeBatch(now time.Time) ([]*protocol.ExtenderProbeAttestation, time.Duration) {
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
func (self *ExtenderLatencyReporter) requeue(batch []*protocol.ExtenderProbeAttestation, now time.Time) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	self.pending = append(slices.Clone(batch), self.pending...)
	if self.settings.MaxPendingCount < len(self.pending) {
		self.pending = slices.Delete(self.pending, 0, len(self.pending)-self.settings.MaxPendingCount)
	}
	self.firstPendingTime = now
	self.retryAfterTime = now.Add(self.settings.FlushTimeout)
}

// Posts one batch. An operator refusal is logged and not retried: the
// extender cannot fix a provider's signature.
func (self *ExtenderLatencyReporter) post(batch []*protocol.ExtenderProbeAttestation) error {
	args := &ExtenderLatencyReportArgs{
		Attestations: []*ExtenderLatencyAttestation{},
	}
	for _, attestation := range batch {
		args.Attestations = append(args.Attestations, ExtenderLatencyAttestationFromProto(attestation))
	}
	ctx, cancel := context.WithTimeout(self.ctx, self.settings.RequestTimeout)
	defer cancel()

	var result *ExtenderLatencyReportResult
	var err error
	if self.settings.Post != nil {
		result, err = self.settings.Post(ctx, args)
	} else {
		byJwt := ""
		if self.settings.ByJwt != nil {
			byJwt = self.settings.ByJwt()
		}
		result, err = PostExtenderLatencyReport(ctx, self.settings.ClientStrategy, self.settings.ApiUrl, byJwt, args)
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
		self.log.Infof("[extender]latency report refused = %s\n", result.Error)
	}
	return nil
}

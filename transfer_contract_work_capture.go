// The SDK lifecycle owns unattended request polling, one-time cut capture and
// immutable outbox delivery. A transport retry only resends already signed bytes.
package connect

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/urnetwork/connect/protocol"
)

const maximumOriginalWorkOutboxRecords = 256
const maximumOriginalWorkOutboxBytes = 128 * 1024 * 1024

// Provision one private outbox per SDK client. The independent request key and
// endpoint are explicit configuration; network responses cannot replace them.
type OriginalWorkCaptureSettings struct {
	ApiUrl           string
	OutboxDirectory  string
	RequestPublicKey [32]byte
	// The approved provider launch profile pins this retained signing identity.
	// Historical rotated keys need their separately approved original profile.
	PublicKey    [32]byte
	PollInterval time.Duration
	// The owning launcher supplies its isolated proxy/trust transport. The worker
	// copies the client and always enforces its own timeout and redirect refusal.
	HttpClient *http.Client
	httpClient *http.Client
	now        func() time.Time
	afterCycle func()
}

// The window phase identity excludes a caller's request id. A newly signed
// retry cannot make the same SDK recapture an already retained boundary.
func originalWorkOutboxName(request protocol.OriginalWorkRequest) string {
	raw := append([]byte("urnetwork-sdk-whole-work-outbox-v1"), request.DomainHash[:]...)
	raw = append(raw, request.ClientId[:]...)
	raw = append(raw, request.Generation[:]...)
	raw = binary.BigEndian.AppendUint64(raw, request.Epoch)
	raw = append(raw, request.Kind...)
	hash := sha256.Sum256(raw)
	return hex.EncodeToString(hash[:]) + ".json"
}

// A complete request cycle uses one original 300-second owner. Every individual
// HTTP attempt is capped at 60 seconds and cancellation joins response custody.
func originalWorkCaptureHttp(ctx context.Context, client *http.Client, method, endpoint string, body []byte, limit int64) ([]byte, error) {
	if ctx == nil || client == nil || limit <= 0 {
		return nil, errors.New("whole-work HTTP read owner is unavailable")
	}
	owner, cancel := context.WithTimeout(ctx, 300*time.Second)
	defer cancel()
	for {
		attempt, stop := context.WithTimeout(owner, 60*time.Second)
		request, err := http.NewRequestWithContext(attempt, method, endpoint, bytes.NewReader(body))
		if err != nil {
			stop()
			return nil, err
		}
		request.Header.Set("Accept", "application/json")
		if body != nil {
			request.Header.Set("Content-Type", "application/json")
		}
		response, sendErr := client.Do(request)
		retry := sendErr != nil
		var raw []byte
		hardRefusal := false
		if response != nil {
			if sendErr == nil && response.StatusCode >= 200 && response.StatusCode < 300 {
				if response.ContentLength > limit {
					sendErr = errors.New("whole-work response exceeds capacity")
					hardRefusal = true
				} else {
					raw, sendErr = io.ReadAll(io.LimitReader(response.Body, limit+1))
					if int64(len(raw)) > limit {
						sendErr = errors.New("whole-work response exceeds capacity")
						hardRefusal = true
					}
				}
				retry = sendErr != nil && !hardRefusal
			} else if sendErr == nil {
				sendErr = fmt.Errorf("whole-work HTTP status %d", response.StatusCode)
				retry = response.StatusCode == 408 || response.StatusCode == 429 || response.StatusCode >= 500
			}
			if closeErr := response.Body.Close(); closeErr != nil {
				sendErr = errors.Join(sendErr, closeErr)
				if !hardRefusal && response.StatusCode >= 200 && response.StatusCode < 300 {
					retry = true
				}
			}
		}
		stop()
		if owner.Err() != nil {
			return nil, errors.Join(owner.Err(), context.Cause(owner), sendErr)
		}
		if sendErr == nil {
			return raw, nil
		}
		if !retry {
			return nil, sendErr
		}
		select {
		case <-owner.Done():
			return nil, errors.Join(owner.Err(), context.Cause(owner), sendErr)
		case <-time.After(time.Second):
		}
	}
}

// Original outbox bytes are verified again before every network handoff.
func (self *ContractManager) deliverOriginalWork(ctx context.Context, settings OriginalWorkCaptureSettings, client *http.Client, base *url.URL, raw []byte) error {
	if len(raw) > protocol.MaximumOriginalWorkSubmissionBytes {
		return errors.New("whole-work submission exceeds capacity")
	}
	var submission protocol.OriginalWorkCutSubmission
	if err := json.Unmarshal(raw, &submission); err != nil {
		return err
	}
	canonical, _ := json.Marshal(submission)
	if !bytes.Equal(canonical, raw) {
		return errors.New("whole-work outbox submission is not canonical")
	}
	receipt, err := protocol.VerifyOriginalWorkSubmission(ctx, submission, settings.RequestPublicKey)
	if err != nil {
		return err
	}
	request, _ := protocol.DecodeOriginalWorkRequest(submission.Request, settings.RequestPublicKey)
	if request.ClientId != [16]byte(self.client.ClientId()) || request.DomainHash != self.closeReportDomainHash {
		return errors.New("whole-work outbox belongs to a different client or domain")
	}
	if settings.PublicKey != ([32]byte{}) && request.PublicKey != settings.PublicKey {
		return errors.New("whole-work outbox signing key differs from its approved capture profile")
	}
	endpoint := base.ResolveReference(&url.URL{Path: "/provider-work/v1/cuts"}).String()
	response, err := originalWorkCaptureHttp(ctx, client, http.MethodPost, endpoint, raw, 8*1024)
	if err != nil {
		return err
	}
	var actual protocol.OriginalWorkCutReceipt
	if json.Unmarshal(response, &actual) != nil || actual != receipt {
		return errors.New("whole-work receipt differs from retained originals")
	}
	return nil
}

// Capture permission is checked only for a new original; retained originals are
// still delivered after expiry or restart without signing a replacement cut.
func (self *ContractManager) captureOriginalWork(ctx context.Context, settings OriginalWorkCaptureSettings, outbox *originalWorkOutbox, requestRaw []byte) ([]byte, error) {
	request, err := protocol.DecodeOriginalWorkRequest(requestRaw, settings.RequestPublicKey)
	if err != nil {
		return nil, err
	}
	if request.ClientId != [16]byte(self.client.ClientId()) || request.DomainHash != self.closeReportDomainHash {
		return nil, errors.New("whole-work request belongs to a different owner")
	}
	if settings.PublicKey != ([32]byte{}) && request.PublicKey != settings.PublicKey {
		return nil, errors.New("whole-work request signing key differs from its approved capture profile")
	}
	name := originalWorkOutboxName(request)
	if prior, err := outbox.read(ctx, name); err == nil {
		var retained protocol.OriginalWorkCutSubmission
		if json.Unmarshal(prior, &retained) != nil {
			return nil, errors.New("whole-work retained submission is malformed")
		}
		canonical, marshalErr := json.Marshal(retained)
		if marshalErr != nil || !bytes.Equal(prior, canonical) {
			return nil, errors.Join(errors.New("whole-work retained submission is not canonical"), marshalErr)
		}
		if _, err := protocol.VerifyOriginalWorkSubmission(ctx, retained, settings.RequestPublicKey); err != nil {
			return nil, err
		}
		originalRequest, err := protocol.DecodeOriginalWorkRequest(retained.Request, settings.RequestPublicKey)
		if err != nil {
			return nil, err
		}
		if originalWorkOutboxName(originalRequest) != name {
			return nil, errors.New("whole-work retained original has another phase identity")
		}
		cut, err := protocol.DecodeOriginalWorkCut(ctx, retained.Cut)
		if err != nil {
			return nil, err
		}
		if !request.Matches(cut) {
			return nil, errors.New("whole-work request reinterprets retained boundary")
		}
		return prior, nil
	} else if !errors.Is(err, errOriginalWorkOutboxUncaptured) {
		return nil, err
	}
	now := time.Now()
	if settings.now != nil {
		now = settings.now()
	}
	if now.Unix() < request.IssuedAtUnix || now.Unix() >= request.ExpiresAtUnix {
		return nil, errors.New("whole-work capture permission is not current")
	}
	if request.Generation != [16]byte(self.wholeWorkInventory.generation) {
		return nil, errors.New("whole-work request targets a prior SDK generation")
	}
	cut, err := self.OriginalWorkCut(ctx, request.Epoch, request.Block, request.BlockHash)
	if err != nil {
		return nil, err
	}
	if !cut.Complete {
		return nil, errors.New("whole-work original capture is waiting for complete current obligations")
	}
	if !request.Matches(cut) {
		return nil, errors.New("whole-work requested signing owner differs")
	}
	cutRaw, err := cut.Bytes(ctx)
	if err != nil {
		return nil, err
	}
	if ctx.Err() != nil || func() int64 {
		if settings.now != nil {
			return settings.now().Unix()
		}
		return time.Now().Unix()
	}() >= request.ExpiresAtUnix {
		return nil, errors.Join(ctx.Err(), errors.New("whole-work capture permission expired before retention"))
	}
	raw, err := json.Marshal(protocol.OriginalWorkCutSubmission{Request: bytes.Clone(requestRaw), Cut: cutRaw})
	if err != nil {
		return nil, err
	}
	if err := outbox.retain(ctx, name, raw); err != nil {
		return nil, err
	}
	return raw, nil
}

// The constructor starts this lifecycle worker only for an explicit capture
// profile. Errors hold optional evidence; they never restart ordinary contracts.
func (self *ContractManager) runOriginalWorkCapture() {
	select {
	case <-self.ctx.Done():
		return
	case <-self.client.ReadyNotify():
	}
	settings := *self.originalWorkCapture
	base, err := url.Parse(settings.ApiUrl)
	if err != nil || base.Scheme != "https" || base.Hostname() == "" || base.User != nil || base.Fragment != "" || base.RawQuery != "" || settings.RequestPublicKey == ([32]byte{}) {
		self.client.log.Errorf("[contract]whole-work capture profile is invalid")
		return
	}
	transport := &http.Transport{Proxy: http.ProxyFromEnvironment, ForceAttemptHTTP2: true, MaxIdleConns: 4, MaxIdleConnsPerHost: 2, IdleConnTimeout: 90 * time.Second, TLSHandshakeTimeout: 10 * time.Second}
	client := &http.Client{Timeout: 60 * time.Second, Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	configuredClient := settings.HttpClient
	if configuredClient == nil {
		configuredClient = settings.httpClient
	}
	if configuredClient != nil {
		copy := *configuredClient
		copy.Timeout = 60 * time.Second
		copy.CheckRedirect = client.CheckRedirect
		client = &copy
	}
	defer client.CloseIdleConnections()
	interval := settings.PollInterval
	if interval < 5*time.Second || interval > time.Minute {
		interval = 15 * time.Second
	}
	var outbox *originalWorkOutbox
	defer func() {
		if outbox != nil {
			if err := outbox.close(); err != nil {
				self.client.log.Errorf("[contract]whole-work outbox custody close failed: %v", err)
			}
		}
	}()
	for self.ctx.Err() == nil {
		err := func() error {
			owner, cancel := context.WithTimeout(self.ctx, 300*time.Second)
			defer cancel()
			if outbox == nil {
				opened, err := openOriginalWorkOutboxContext(owner, settings.OutboxDirectory)
				if err != nil {
					return err
				}
				outbox = opened
			}
			entries, err := outbox.entries(owner)
			if err != nil {
				return err
			}
			for _, name := range entries {
				raw, err := outbox.read(owner, name)
				if err != nil {
					return err
				}
				if err := self.deliverOriginalWork(owner, settings, client, base, raw); err != nil {
					return err
				}
			}
			identity, err := self.OriginalWorkIdentity(owner)
			if err != nil {
				return err
			}
			identityRaw, err := identity.Bytes(owner)
			if err != nil {
				return err
			}
			identityEndpoint := base.ResolveReference(&url.URL{Path: "/provider-work/v1/owners"}).String()
			identityResponse, err := originalWorkCaptureHttp(owner, client, http.MethodPost, identityEndpoint, identityRaw, 8*1024)
			if err != nil {
				return err
			}
			var identityReceipt protocol.OriginalWorkOwnerReceipt
			if json.Unmarshal(identityResponse, &identityReceipt) != nil || identityReceipt.Schema != protocol.OriginalWorkOwnerReceiptSchema || identityReceipt.OwnerHash != sha256.Sum256(identityRaw) {
				return errors.New("whole-work owner receipt differs from original enrollment")
			}
			keyOwner := self.client.ClientKeyManager()
			if keyOwner == nil {
				return errors.New("whole-work client key owner unavailable")
			}
			keyOwner.stateLock.RLock()
			var publicKey []byte
			if len(keyOwner.privateKey) == 64 {
				publicKey = bytes.Clone(keyOwner.privateKey[32:])
			}
			keyOwner.stateLock.RUnlock()
			if len(publicKey) != 32 {
				return errors.New("whole-work client key owner malformed")
			}
			query := url.Values{"domain": {hex.EncodeToString(self.closeReportDomainHash[:])}, "client": {hex.EncodeToString(self.client.clientId[:])}, "generation": {hex.EncodeToString(self.wholeWorkInventory.generation[:])}, "key": {hex.EncodeToString(publicKey)}}
			endpoint := base.ResolveReference(&url.URL{Path: "/provider-work/v1/requests", RawQuery: query.Encode()}).String()
			raw, err := originalWorkCaptureHttp(owner, client, http.MethodGet, endpoint, nil, 128*1024)
			if err != nil {
				return err
			}
			var requests protocol.OriginalWorkRequests
			if json.Unmarshal(raw, &requests) != nil || requests.Schema != protocol.OriginalWorkRequestsSchema || requests.Requests == nil || len(requests.Requests) > protocol.MaximumOriginalWorkRequests {
				return errors.New("whole-work request list is incomplete or oversized")
			}
			for _, request := range requests.Requests {
				raw, err := self.captureOriginalWork(owner, settings, outbox, request)
				if err != nil {
					return err
				}
				if err := self.deliverOriginalWork(owner, settings, client, base, raw); err != nil {
					return err
				}
			}
			return owner.Err()
		}()
		if settings.afterCycle != nil {
			settings.afterCycle()
		}
		if err != nil && self.ctx.Err() == nil {
			self.client.log.Errorf("[contract]whole-work evidence pending: %v", err)
		}
		select {
		case <-self.ctx.Done():
			return
		case <-time.After(interval):
		}
	}
}

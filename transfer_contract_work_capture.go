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
	"os"
	"path/filepath"
	"strings"
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
	PollInterval     time.Duration
	// The owning launcher supplies its isolated proxy/trust transport. The worker
	// copies the client and always enforces its own timeout and redirect refusal.
	HttpClient *http.Client
	httpClient *http.Client
	now        func() time.Time
	afterCycle func()
}

// A descriptor and exclusive process lease protect the complete retained set.
// No cut is evicted to make a failed or full generation look like an empty one.
type originalWorkOutbox struct {
	root *os.Root
	lock *os.File
}

// Private descriptor anchoring refuses symlinks, public directories and two
// simultaneous owners. A partial write remains visible and is never recaptured.
func openOriginalWorkOutbox(directory string) (*originalWorkOutbox, error) {
	if !filepath.IsAbs(directory) || filepath.Clean(directory) != directory {
		return nil, errors.New("whole-work outbox requires a canonical absolute directory")
	}
	if err := os.MkdirAll(directory, 0700); err != nil {
		return nil, err
	}
	info, err := os.Lstat(directory)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() || info.Mode().Perm() != 0700 {
		return nil, errors.New("whole-work outbox directory is not private")
	}
	root, err := os.OpenRoot(directory)
	if err != nil {
		return nil, err
	}
	opened, err := root.Stat(".")
	if err != nil || !os.SameFile(info, opened) {
		root.Close()
		return nil, errors.New("whole-work outbox directory changed")
	}
	lock, err := lockOriginalWorkOutbox(root)
	if err != nil {
		root.Close()
		return nil, err
	}
	return &originalWorkOutbox{root: root, lock: lock}, nil
}

// The SDK joins this owner before releasing its retained directory lease.
func (self *originalWorkOutbox) close() error {
	return errors.Join(self.lock.Close(), self.root.Close())
}

// Every retained name is checked; unknown or oversized entries hold evidence.
func (self *originalWorkOutbox) entries(ctx context.Context) ([]string, error) {
	file, err := self.root.Open(".")
	if err != nil {
		return nil, err
	}
	entries, readErr := file.ReadDir(maximumOriginalWorkOutboxRecords + 2)
	closeErr := file.Close()
	if err := errors.Join(readErr, closeErr); err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	result := make([]string, 0, len(entries))
	var total int64
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if entry.Name() == ".owner.lock" {
			continue
		}
		name := entry.Name()
		if len(name) != 69 || !strings.HasSuffix(name, ".json") {
			return nil, errors.New("whole-work outbox has an unrecognized original")
		}
		if _, err := hex.DecodeString(name[:64]); err != nil || strings.ToLower(name) != name {
			return nil, errors.New("whole-work outbox name is not canonical")
		}
		info, err := self.root.Lstat(name)
		if err != nil {
			return nil, err
		}
		if !info.Mode().IsRegular() || info.Mode().Perm() != 0400 || info.Size() <= 0 || info.Size() > protocol.MaximumOriginalWorkSubmissionBytes {
			return nil, errors.New("whole-work outbox original is partial or unprotected")
		}
		total += info.Size()
		if total > maximumOriginalWorkOutboxBytes || len(result) >= maximumOriginalWorkOutboxRecords {
			return nil, errors.New("whole-work outbox complete inventory exceeds capacity")
		}
		result = append(result, name)
	}
	return result, nil
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

// Retained public envelopes remain bounded while reading and retain all errors.
func (self *originalWorkOutbox) read(ctx context.Context, name string) ([]byte, error) {
	info, err := self.root.Lstat(name)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0400 || info.Size() <= 0 || info.Size() > protocol.MaximumOriginalWorkSubmissionBytes {
		return nil, errors.New("whole-work retained original is not complete and protected")
	}
	file, err := self.root.Open(name)
	if err != nil {
		return nil, err
	}
	opened, statErr := file.Stat()
	if statErr != nil || !os.SameFile(info, opened) {
		file.Close()
		return nil, errors.New("whole-work retained original changed")
	}
	var value bytes.Buffer
	buffer := make([]byte, 32*1024)
	for value.Len() <= protocol.MaximumOriginalWorkSubmissionBytes {
		if err := ctx.Err(); err != nil {
			file.Close()
			return nil, err
		}
		n, readErr := file.Read(buffer)
		value.Write(buffer[:n])
		if readErr != nil {
			closeErr := file.Close()
			if !errors.Is(readErr, io.EOF) {
				return nil, errors.Join(readErr, closeErr)
			}
			if closeErr != nil {
				return nil, closeErr
			}
			if value.Len() > protocol.MaximumOriginalWorkSubmissionBytes {
				return nil, errors.New("whole-work retained original exceeds capacity")
			}
			return value.Bytes(), nil
		}
	}
	file.Close()
	return nil, errors.New("whole-work retained original exceeds capacity")
}

// Create once, sync bytes and metadata, then sync the parent before publication.
// A failed partial original is retained as a visible hold; never unlink/retry it.
func (self *originalWorkOutbox) retain(ctx context.Context, name string, raw []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	entries, err := self.entries(ctx)
	if err != nil {
		return err
	}
	if len(entries) >= maximumOriginalWorkOutboxRecords || len(raw) > protocol.MaximumOriginalWorkSubmissionBytes {
		return errors.New("whole-work outbox has no complete-record capacity")
	}
	var total int64
	for _, entry := range entries {
		info, err := self.root.Stat(entry)
		if err != nil {
			return err
		}
		total += info.Size()
	}
	if int64(len(raw)) > maximumOriginalWorkOutboxBytes-total {
		return errors.New("whole-work outbox has no complete-byte capacity")
	}
	file, err := self.root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	_, writeErr := file.Write(raw)
	if writeErr == nil {
		writeErr = file.Chmod(0400)
	}
	if writeErr == nil {
		writeErr = file.Sync()
	}
	if err := errors.Join(writeErr, file.Close()); err != nil {
		return err
	}
	parent, err := self.root.Open(".")
	if err != nil {
		return err
	}
	return errors.Join(parent.Sync(), parent.Close(), ctx.Err())
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
	name := originalWorkOutboxName(request)
	if prior, err := outbox.read(ctx, name); err == nil {
		var retained protocol.OriginalWorkCutSubmission
		if json.Unmarshal(prior, &retained) != nil {
			return nil, errors.New("whole-work retained submission is malformed")
		}
		cut, err := protocol.DecodeOriginalWorkCut(ctx, retained.Cut)
		if err != nil || !request.Matches(cut) {
			return nil, errors.New("whole-work request reinterprets retained boundary")
		}
		return prior, nil
	} else if !errors.Is(err, os.ErrNotExist) {
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
			if outbox == nil {
				opened, err := openOriginalWorkOutbox(settings.OutboxDirectory)
				if err != nil {
					return err
				}
				outbox = opened
			}
			owner, cancel := context.WithTimeout(self.ctx, 300*time.Second)
			defer cancel()
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

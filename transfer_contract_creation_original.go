// Original contract capture belongs to the real request/callback owner. Optional
// evidence failure preserves transfer obligations and leaves attribution unknown.
package connect

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"path/filepath"
	"strings"
	"sync"

	"github.com/urnetwork/connect/protocol"
	"google.golang.org/protobuf/proto"
)

// Provision a private directory distinct from the whole-work cut outbox. These
// leaves retain individual originals; only the independent cut roster certifies
// complete contract coverage. Empty settings never mean an empty participant set.
type OriginalContractCaptureSettings struct {
	Directory        string
	PublicKey        [32]byte
	SourceGeneration [16]byte
}

// File publication is serialized locally and uses a process lease per write.
// No descriptor survives a request hook or the owned response callback.
type originalContractCreationOwner struct {
	stateLock sync.Mutex
	directory string
	scope     OriginalContractStoreScope
}

// Separate roots prevent an optional leaf from corrupting a complete cut-outbox
// namespace. Configuration cannot place either root inside the other one.
func originalContractCreationDirectory(settings *ContractManagerSettings) (string, error) {
	if settings.OriginalContractCapture == nil {
		return "", nil
	}
	directory := settings.OriginalContractCapture.Directory
	if !filepath.IsAbs(directory) || filepath.Clean(directory) != directory || directory == string(filepath.Separator) {
		return "", errors.New("original contract custody directory is not canonical")
	}
	if settings.OriginalWorkCapture != nil {
		outbox := settings.OriginalWorkCapture.OutboxDirectory
		for _, pair := range [][2]string{{directory, outbox}, {outbox, directory}} {
			relative, err := filepath.Rel(pair[0], pair[1])
			if err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
				return "", errors.New("original contract and whole-work custody directories overlap")
			}
		}
	}
	return directory, nil
}

// The original request is signed and durably retained before SendControl takes
// its frame. The returned bytes are an owned copy retained by that callback.
func (self *ContractManager) captureOriginalContractRequest(frame *protocol.Frame) []byte {
	if self.closeReportDomainHash == ([32]byte{}) || self.contractCreation.directory == "" {
		return nil
	}
	keyOwner := self.client.ClientKeyManager()
	if keyOwner == nil {
		return nil
	}
	frameRaw, err := proto.Marshal(frame)
	if err != nil {
		return nil
	}
	request, err := func() (protocol.OriginalContractRequest, error) {
		keyOwner.stateLock.RLock()
		defer keyOwner.stateLock.RUnlock()
		return protocol.SignOriginalContractRequest(self.ctx, protocol.OriginalContractRequest{
			DomainHash: self.closeReportDomainHash, ClientId: [16]byte(self.client.ClientId()),
			Generation: [16]byte(self.wholeWorkInventory.generation), RequestId: [16]byte(NewId()),
			RequestFrame: frameRaw,
		}, keyOwner.privateKey)
	}()
	if err != nil {
		self.client.log.Errorf("[contract]original request unavailable: %v", err)
		return nil
	}
	raw, err := request.Bytes(self.ctx)
	if err == nil {
		err = self.contractCreation.retain(self.ctx, "request", raw)
	}
	if err != nil {
		self.client.log.Errorf("[contract]original request custody unavailable: %v", err)
		return nil
	}
	return raw
}

// Only the callback holding this original request can bind its returned result.
// Unexpected multi-result or legacy non-echo responses remain unproved while the
// ordinary queue consumer still owns their existing transfer/cleanup behavior.
func (self *ContractManager) captureOriginalContractAdmission(requestRaw []byte, frames []*protocol.Frame, resultErr error) {
	if len(requestRaw) == 0 || resultErr != nil || len(frames) != 1 || frames[0] == nil {
		return
	}
	keyOwner := self.client.ClientKeyManager()
	if keyOwner == nil {
		return
	}
	resultRaw, err := proto.Marshal(frames[0])
	if err != nil {
		return
	}
	admission, err := func() (protocol.OriginalContractAdmission, error) {
		keyOwner.stateLock.RLock()
		defer keyOwner.stateLock.RUnlock()
		return protocol.SignOriginalContractAdmission(self.ctx, protocol.OriginalContractAdmission{
			Request: requestRaw, ResultFrame: resultRaw,
		}, keyOwner.privateKey)
	}()
	if err != nil {
		// Explicit rejections have no earned reservation to bind.
		var response protocol.CreateContractResult
		if frames[0].MessageType == protocol.MessageType_TransferCreateContractResult && ProtoUnmarshal(frames[0].MessageBytes, &response) == nil && response.Error != nil && response.Contract == nil {
			return
		}
		self.client.log.Errorf("[contract]original admission unavailable: %v", err)
		return
	}
	raw, err := admission.Bytes(self.ctx)
	if err == nil {
		err = self.contractCreation.retainAdmission(self.ctx, requestRaw, raw)
	}
	if err != nil {
		self.client.log.Errorf("[contract]original admission custody unavailable: %v", err)
		return
	}
	facts, err := admission.Facts(self.ctx)
	if err != nil {
		return
	}
	self.retainOriginalWorkCreation(facts.StoredContract, raw)
}

// Attach only after durable response retention, before the queue can expose the
// reservation. A conflicting second original permanently breaks this generation.
func (self *ContractManager) retainOriginalWorkCreation(stored, original []byte) {
	self.admitOriginalWork(stored)
	var reservation protocol.StoredContract
	if ProtoUnmarshal(stored, &reservation) != nil || len(reservation.ContractId) != 16 {
		return
	}
	owner := &self.wholeWorkInventory
	owner.stateLock.Lock()
	defer owner.stateLock.Unlock()
	id := Id(reservation.ContractId)
	previous, exists := owner.contractKVs[id]
	if !exists || !bytes.Equal(previous.StoredContract, stored) {
		owner.broken = true
		return
	}
	if len(previous.OriginalCreation) != 0 {
		if !bytes.Equal(previous.OriginalCreation, original) {
			owner.broken = true
		}
		return
	}
	if len(original) > protocol.MaximumOriginalWorkCutBytes/2-owner.used {
		owner.broken = true
		return
	}
	previous.OriginalCreation = bytes.Clone(original)
	owner.contractKVs[id] = previous
	owner.used += len(original)
	owner.advanceWithLock()
}

// A leaf's name commits to its entire signed content; a repeated callback may
// reuse only the exact already retained bytes, never overwrite its original.
func originalContractLeafName(kind string, raw []byte) (string, error) {
	if kind != "request" && kind != "admission" {
		return "", errors.New("original contract leaf kind is invalid")
	}
	hash := sha256.Sum256(raw)
	return kind + "-" + hex.EncodeToString(hash[:]) + ".json", nil
}

// Preserve the pre-send file before binding a response. A removed or changed
// request cannot be recreated from a callback's memory and presented as custody.
func (self *originalContractCreationOwner) retainAdmission(ctx context.Context, requestRaw, admissionRaw []byte) (resultErr error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	store, err := openOriginalContractStore(ctx, self.directory)
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, store.close()) }()
	if err := self.scope.Validate(); err != nil {
		return err
	}
	if store.checkpoint.Scope != self.scope {
		return originalContractStoreLoss("original contract namespace differs from independently approved source", nil)
	}
	name, err := originalContractLeafName("request", requestRaw)
	if err != nil {
		return err
	}
	retained, err := store.read(ctx, name)
	if err != nil {
		return originalContractStoreObservation("original contract request custody disappeared before admission", err)
	}
	if !bytes.Equal(requestRaw, retained) {
		return originalContractStoreLoss("original contract request custody changed before admission", nil)
	}
	name, err = originalContractLeafName("admission", admissionRaw)
	if err != nil {
		return err
	}
	return store.retain(ctx, name, admissionRaw)
}

// Each write retains its own lease and all physical directory ancestors until
// publication finishes. This store never supplies a completeness assertion.
func (self *originalContractCreationOwner) retain(ctx context.Context, kind string, raw []byte) (resultErr error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	store, err := openOriginalContractStore(ctx, self.directory)
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, store.close()) }()
	if err := self.scope.Validate(); err != nil {
		return err
	}
	if store.checkpoint.Scope != self.scope {
		return originalContractStoreLoss("original contract namespace differs from independently approved source", nil)
	}
	name, err := originalContractLeafName(kind, raw)
	if err != nil {
		return err
	}
	return store.retain(ctx, name, raw)
}

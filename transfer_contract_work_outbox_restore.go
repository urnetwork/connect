package connect

// Offline callers supply a complete protected census and borrowed reads. These
// helpers neither open live owners nor publish files, checkpoints or signatures.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"

	"github.com/urnetwork/connect/protocol"
)

// This public scope comes from the independently approved capture profile.
// Neither a restored request's signer nor its cut chooses these expected values.
type OriginalWorkOutboxScope struct {
	DomainHash       [32]byte `json:"domain_hash"`
	ClientId         [16]byte `json:"client_id"`
	PublicKey        [32]byte `json:"public_key"`
	RequestPublicKey [32]byte `json:"request_public_key"`
}

// Names are direct children. The root uses an empty name and mode 0700; the
// physical index uses mode 0600 and immutable signed originals use mode 0400.
type OriginalWorkOutboxFile struct {
	Device uint64 `json:"device"`
	Name   string `json:"name"`
	Inode  uint64 `json:"inode"`
	Mode   uint32 `json:"mode"`
	Bytes  uint64 `json:"bytes"`
	Sha256 string `json:"sha256"`
}

// Only these unsigned physical bytes change during a copied-inode restoration.
type OriginalWorkOutboxRebound struct {
	Checkpoint []byte
	Index      []byte
}

// The caller retains descriptor custody and returns complete original bytes.
// Errors abort before semantic comparisons, including cancellation/read causes.
type OriginalWorkOutboxRead func(context.Context, string) ([]byte, error)

func originalWorkOutboxRestoreFiles(files []OriginalWorkOutboxFile) (map[string]OriginalWorkOutboxFile, error) {
	if len(files) < 2 || len(files) > maximumOriginalWorkOutboxRecords+2 {
		return nil, originalWorkOutboxLoss("restore outbox census is incomplete or exceeds capacity", nil)
	}
	fileKVs := map[string]OriginalWorkOutboxFile{}
	inodeKVs := map[uint64]bool{}
	for _, file := range files {
		if _, exists := fileKVs[file.Name]; exists || file.Inode == 0 || inodeKVs[file.Inode] {
			return nil, originalWorkOutboxLoss("restore outbox census repeats a name or physical inode", nil)
		}
		fileKVs[file.Name], inodeKVs[file.Inode] = file, true
		switch file.Name {
		case "":
			if file.Mode != 0700 || file.Bytes != 0 || file.Sha256 != "" {
				return nil, originalWorkOutboxLoss("restore outbox root differs from its fixed profile", nil)
			}
		case originalWorkOutboxIndexName:
			if file.Mode != 0600 || file.Bytes > maximumOriginalWorkOutboxIndexBytes {
				return nil, originalWorkOutboxLoss("restore outbox inventory differs from its fixed profile", nil)
			}
		default:
			if file.Mode != 0400 || !originalWorkOutboxValidMember(OriginalWorkOutboxMember{Name: file.Name, Inode: file.Inode, Bytes: file.Bytes, Sha256: file.Sha256}) {
				return nil, originalWorkOutboxLoss("restore outbox contains an unknown or partial original", nil)
			}
		}
	}
	if _, ok := fileKVs[""]; !ok {
		return nil, originalWorkOutboxLoss("restore outbox root is absent", nil)
	}
	if _, ok := fileKVs[originalWorkOutboxIndexName]; !ok {
		return nil, originalWorkOutboxLoss("restore outbox index is absent", nil)
	}
	for _, file := range files {
		if file.Device != fileKVs[""].Device {
			return nil, originalWorkOutboxLoss("restore outbox census crosses physical devices", nil)
		}
	}
	return fileKVs, nil
}

// All phases, names, counts, signed identities and original digests are checked
// before returning either transformed metadata image. Pending work stays pending.
func RebindOriginalWorkOutboxInventory(ctx context.Context, scope OriginalWorkOutboxScope, checkpointRaw, indexRaw []byte, originals, targets []OriginalWorkOutboxFile, read OriginalWorkOutboxRead) (OriginalWorkOutboxRebound, error) {
	return rebindOriginalWorkOutboxInventory(ctx, scope, checkpointRaw, indexRaw, originals, targets, read, true)
}

// Staging knows copied leaf coordinates before its final index/root exist.
// This form derives only index bytes and never fabricates a checkpoint identity.
func RebindOriginalWorkOutboxIndex(ctx context.Context, scope OriginalWorkOutboxScope, checkpointRaw, indexRaw []byte, originals, targets []OriginalWorkOutboxFile, read OriginalWorkOutboxRead) ([]byte, error) {
	result, err := rebindOriginalWorkOutboxInventory(ctx, scope, checkpointRaw, indexRaw, originals, targets, read, false)
	return result.Index, err
}

func rebindOriginalWorkOutboxInventory(ctx context.Context, scope OriginalWorkOutboxScope, checkpointRaw, indexRaw []byte, originals, targets []OriginalWorkOutboxFile, read OriginalWorkOutboxRead, complete bool) (OriginalWorkOutboxRebound, error) {
	if ctx == nil || read == nil || scope.DomainHash == ([32]byte{}) || scope.ClientId == ([16]byte{}) || scope.PublicKey == ([32]byte{}) || scope.RequestPublicKey == ([32]byte{}) || scope.PublicKey == scope.RequestPublicKey {
		return OriginalWorkOutboxRebound{}, errors.New("outbox restore requires its independent capture scope and borrowed reads")
	}
	if err := ctx.Err(); err != nil {
		return OriginalWorkOutboxRebound{}, err
	}
	checkpoint, members, err := DecodeOriginalWorkOutboxInventory(checkpointRaw, indexRaw)
	if err != nil {
		return OriginalWorkOutboxRebound{}, err
	}
	originalKVs, err := originalWorkOutboxRestoreFiles(originals)
	if err != nil {
		return OriginalWorkOutboxRebound{}, err
	}
	targetKVs := map[string]OriginalWorkOutboxFile{}
	if complete {
		targetKVs, err = originalWorkOutboxRestoreFiles(targets)
		if err != nil {
			return OriginalWorkOutboxRebound{}, err
		}
	} else {
		inodeKVs := map[uint64]bool{}
		for _, target := range targets {
			if _, exists := targetKVs[target.Name]; exists || target.Mode != 0400 || !originalWorkOutboxValidMember(OriginalWorkOutboxMember{Name: target.Name, Inode: target.Inode, Bytes: target.Bytes, Sha256: target.Sha256}) || inodeKVs[target.Inode] || target.Device != targets[0].Device {
				return OriginalWorkOutboxRebound{}, originalWorkOutboxLoss("restore outbox copied originals are incomplete or aliased", nil)
			}
			targetKVs[target.Name], inodeKVs[target.Inode] = target, true
		}
	}
	index := originalKVs[originalWorkOutboxIndexName]
	if checkpoint.DirectoryDevice != originalKVs[""].Device || checkpoint.DirectoryInode != originalKVs[""].Inode || checkpoint.IndexInode != index.Inode || index.Bytes != uint64(len(indexRaw)) || index.Sha256 != originalWorkOutboxDigest(indexRaw) || complete && len(originalKVs) != len(targetKVs) || !complete && len(originalKVs) != len(targetKVs)+2 {
		return OriginalWorkOutboxRebound{}, originalWorkOutboxLoss("restore outbox index or root differs from original physical custody", nil)
	}
	all := append([]OriginalWorkOutboxMember(nil), members...)
	if checkpoint.Pending != nil {
		all = append(all, *checkpoint.Pending)
	}
	if len(originalKVs) != len(all)+2 {
		return OriginalWorkOutboxRebound{}, originalWorkOutboxLoss("restore outbox omits or adds an original", nil)
	}
	for _, member := range all {
		if err := ctx.Err(); err != nil {
			return OriginalWorkOutboxRebound{}, err
		}
		original, ok := originalKVs[member.Name]
		if !ok || original.Inode != member.Inode || original.Bytes != member.Bytes || original.Sha256 != member.Sha256 {
			return OriginalWorkOutboxRebound{}, originalWorkOutboxLoss("restore outbox original differs from its checkpoint member", nil)
		}
		target, ok := targetKVs[member.Name]
		if !ok || target.Mode != original.Mode || target.Bytes != original.Bytes || target.Sha256 != original.Sha256 {
			return OriginalWorkOutboxRebound{}, originalWorkOutboxLoss("restore outbox target changes signed original bytes", nil)
		}
		raw, err := read(ctx, member.Name)
		if err != nil {
			return OriginalWorkOutboxRebound{}, err
		}
		if uint64(len(raw)) != member.Bytes || originalWorkOutboxDigest(raw) != member.Sha256 {
			return OriginalWorkOutboxRebound{}, originalWorkOutboxLoss("restore outbox borrowed original differs", nil)
		}
		var submission protocol.OriginalWorkCutSubmission
		if err := json.Unmarshal(raw, &submission); err != nil {
			return OriginalWorkOutboxRebound{}, err
		}
		canonical, err := json.Marshal(submission)
		if err != nil || !bytes.Equal(raw, canonical) {
			return OriginalWorkOutboxRebound{}, originalWorkOutboxLoss("restore outbox submission is not original canonical bytes", err)
		}
		if _, err := protocol.VerifyOriginalWorkSubmission(ctx, submission, scope.RequestPublicKey); err != nil {
			return OriginalWorkOutboxRebound{}, err
		}
		request, err := protocol.DecodeOriginalWorkRequest(submission.Request, scope.RequestPublicKey)
		if err != nil {
			return OriginalWorkOutboxRebound{}, err
		}
		if request.DomainHash != scope.DomainHash || request.ClientId != scope.ClientId || request.PublicKey != scope.PublicKey || originalWorkOutboxName(request) != member.Name {
			return OriginalWorkOutboxRebound{}, originalWorkOutboxLoss("restore outbox original belongs to another capture scope or boundary", nil)
		}
	}
	var reboundIndex []byte
	for _, member := range members {
		member.Inode = targetKVs[member.Name].Inode
		line, err := json.Marshal(member)
		if err != nil {
			return OriginalWorkOutboxRebound{}, err
		}
		reboundIndex = append(reboundIndex, append(line, '\n')...)
	}
	committedBytes := len(reboundIndex)
	if complete {
		checkpoint.DirectoryDevice = targetKVs[""].Device
		checkpoint.DirectoryInode, checkpoint.IndexInode = targetKVs[""].Inode, targetKVs[originalWorkOutboxIndexName].Inode
	}
	if checkpoint.Pending != nil {
		pending := *checkpoint.Pending
		pending.Inode = targetKVs[pending.Name].Inode
		checkpoint.Pending = &pending
		if uint64(len(indexRaw)) > checkpoint.IndexBytes {
			line, err := json.Marshal(pending)
			if err != nil {
				return OriginalWorkOutboxRebound{}, err
			}
			reboundIndex = append(reboundIndex, append(line, '\n')...)
		}
	}
	checkpoint.IndexBytes, checkpoint.IndexSha256 = uint64(committedBytes), originalWorkOutboxDigest(reboundIndex[:committedBytes])
	raw, err := json.Marshal(checkpoint)
	if err != nil {
		return OriginalWorkOutboxRebound{}, err
	}
	if _, _, err := DecodeOriginalWorkOutboxInventory(raw, reboundIndex); err != nil {
		return OriginalWorkOutboxRebound{}, err
	}
	if err := ctx.Err(); err != nil {
		return OriginalWorkOutboxRebound{}, err
	}
	if !complete {
		return OriginalWorkOutboxRebound{Index: reboundIndex}, nil
	}
	return OriginalWorkOutboxRebound{Checkpoint: raw, Index: reboundIndex}, nil
}

// Inspection reverses only member inode fields before checking the original
// index digest. This does not normalize malformed or historical index grammar.
func OriginalWorkOutboxRestoreOriginalIndex(ctx context.Context, indexRaw []byte, originals []OriginalWorkOutboxFile) ([]byte, error) {
	if ctx == nil || len(indexRaw) > maximumOriginalWorkOutboxIndexBytes {
		return nil, errors.New("outbox original inventory reconstruction is unbounded")
	}
	fileKVs, err := originalWorkOutboxRestoreFiles(originals)
	if err != nil {
		return nil, err
	}
	var result []byte
	seenKVs := map[string]bool{}
	for remaining := indexRaw; len(remaining) != 0; {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		end := bytes.IndexByte(remaining, '\n')
		if end < 0 || end > 511 || len(seenKVs) >= maximumOriginalWorkOutboxRecords {
			return nil, originalWorkOutboxLoss("restored outbox index has an incomplete or oversized record", nil)
		}
		var member OriginalWorkOutboxMember
		if err := json.Unmarshal(remaining[:end], &member); err != nil {
			return nil, err
		}
		canonical, err := json.Marshal(member)
		original, ok := fileKVs[member.Name]
		if err != nil || !bytes.Equal(canonical, remaining[:end]) || !originalWorkOutboxValidMember(member) || !ok || seenKVs[member.Name] || original.Bytes != member.Bytes || original.Sha256 != member.Sha256 {
			return nil, originalWorkOutboxLoss("restored outbox index changes logical original custody", err)
		}
		seenKVs[member.Name] = true
		member.Inode = original.Inode
		line, err := json.Marshal(member)
		if err != nil {
			return nil, err
		}
		result = append(result, append(line, '\n')...)
		remaining = remaining[end+1:]
	}
	index := fileKVs[originalWorkOutboxIndexName]
	if uint64(len(result)) != index.Bytes || originalWorkOutboxDigest(result) != index.Sha256 {
		return nil, originalWorkOutboxLoss("restored outbox index does not preserve its original inventory ancestry", nil)
	}
	return result, ctx.Err()
}

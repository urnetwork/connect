// Explicit deployment storage declarations are independent of signed protocol
// objects. Runtime opening never enrolls a new volume, marker or durable root.
package durablevolume

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
)

const Schema = "urnetwork-durable-volumes-v2"
const OwnerLocalSchema = "urnetwork-owner-local-volumes-v2"
const RootGenerationAttribute = "user.urnetwork.durable-root-generation"
const RootGenerationBytes = 32
const maximumConfigBytes = 64 * 1024
const maximumMarkerBytes = 4 * 1024

// The caller selects a scope independently of declaration contents; a
// declaration can never choose its own scope.
type ownerScope uint8

const (
	daemonScope ownerScope = iota + 1
	ownerLocalScope
)

// The independently selected operational declaration has exact retained bytes.
type Reference struct {
	Path   string `json:"path" yaml:"path"`
	Sha256 string `json:"sha256" yaml:"sha256"`
}

// Each declared filesystem contains a finite set of precreated owner roots.
type Config struct {
	Schema  string       `json:"schema"`
	Volumes []VolumeSpec `json:"volumes"`
}

// Device names may change across reboot; the filesystem uuid and provisioned
// marker remain stable. Reserve floors are admission checks, not reservations.
type VolumeSpec struct {
	MountPath          string          `json:"mount_path"`
	FilesystemUuid     string          `json:"filesystem_uuid"`
	FilesystemType     string          `json:"filesystem_type"`
	MarkerPath         string          `json:"marker_path"`
	MarkerSha256       string          `json:"marker_sha256"`
	StateRoots         []StateRootSpec `json:"state_roots"`
	MinAvailableBytes  uint64          `json:"min_available_bytes"`
	MinAvailableInodes uint64          `json:"min_available_inodes"`
}

// Separate leases let unrelated roots run during a stopped-root inventory.
// The external inode/nonce binding is enrolled before use, never by runtime.
type StateRootSpec struct {
	Path             string `json:"path"`
	LeasePath        string `json:"lease_path"`
	LeaseSha256      string `json:"lease_sha256"`
	RootInode        uint64 `json:"root_inode"`
	GenerationSha256 string `json:"generation_sha256"`
}

// Only the policy reference is inherited; every descendant owns its own guard.
type referenceContextKey struct{}

// Attaches an immutable value without selecting a volume or granting authority.
func WithReference(ctx context.Context, reference Reference) context.Context {
	return context.WithValue(ctx, referenceContextKey{}, reference)
}

// Absence stays explicit; there is no environment or home-directory fallback.
func ReferenceFromContext(ctx context.Context) (Reference, bool) {
	if ctx == nil {
		return Reference{}, false
	}
	reference, ok := ctx.Value(referenceContextKey{}).(Reference)
	return reference, ok
}

// Canonical absolute paths cannot hide aliases or relative traversal.
func canonical(path string) bool {
	return filepath.IsAbs(path) && filepath.Clean(path) == path && path != "/" && !strings.ContainsAny(path, "\x00\n\r") && len(path) <= 4096
}

// A separator boundary prevents one sibling prefix from admitting another.
func beneath(parent, path string) bool {
	if parent == string(filepath.Separator) {
		return filepath.IsAbs(path)
	}
	return path == parent || strings.HasPrefix(path, parent+string(filepath.Separator))
}

// Digests use one unambiguous lowercase wire representation.
func validDigest(value string) bool {
	if len(value) != 71 || !strings.HasPrefix(value, "sha256:") || strings.ToLower(value) != value {
		return false
	}
	_, err := hex.DecodeString(value[7:])
	return err == nil
}

// Bounds, ambiguity and unsupported filesystems are rejected before opening.
func (self Config) validate() error {
	return self.validateForScope(daemonScope)
}

// Either scope may use the declared system filesystem. Its protected
// precreated root, external lease and identity marker remain mandatory.
func (self Config) validateForScope(scope ownerScope) error {
	expectedSchema := Schema
	if scope == ownerLocalScope {
		expectedSchema = OwnerLocalSchema
	}
	if scope != daemonScope && scope != ownerLocalScope || self.Schema != expectedSchema || len(self.Volumes) == 0 || len(self.Volumes) > 16 {
		return errors.New("durable volume schema or volume count is invalid")
	}
	var roots []string
	var identityPaths []string
	leasePaths := map[string]bool{}
	mounts := map[string]bool{}
	for _, volume := range self.Volumes {
		canonicalMount := canonical(volume.MountPath) || volume.MountPath == "/"
		if !canonicalMount || mounts[volume.MountPath] || !canonical(volume.MarkerPath) || !beneath(volume.MountPath, volume.MarkerPath) ||
			!validDigest(volume.MarkerSha256) || len(volume.StateRoots) == 0 || len(volume.StateRoots) > 64 || volume.MinAvailableBytes == 0 || volume.MinAvailableInodes == 0 {
			return errors.New("durable volume identity, roots or positive reserve is incomplete")
		}
		mounts[volume.MountPath] = true
		identityPaths = append(identityPaths, volume.MarkerPath)
		// The declaration format is platform-neutral; each host admits only
		// its own qualified types (ext4/xfs/btrfs on Linux, apfs on Darwin).
		if volume.FilesystemType != "ext4" && volume.FilesystemType != "xfs" && volume.FilesystemType != "btrfs" && volume.FilesystemType != "apfs" {
			return errors.New("durable volume filesystem type is unsupported")
		}
		if len(volume.FilesystemUuid) < 4 || len(volume.FilesystemUuid) > 64 {
			return errors.New("durable volume filesystem uuid is invalid")
		}
		for _, character := range volume.FilesystemUuid {
			if !(character >= '0' && character <= '9' || character >= 'a' && character <= 'f' || character == '-') {
				return errors.New("durable volume filesystem uuid is not canonical")
			}
		}
		for _, root := range volume.StateRoots {
			if !canonical(root.Path) || !beneath(volume.MountPath, root.Path) || root.Path == volume.MountPath || len(roots) >= 256 {
				return errors.New("durable owner root is absent, unbounded or outside its mount")
			}
			if root.RootInode == 0 || !validDigest(root.GenerationSha256) {
				return errors.New("durable owner root requires preprovisioned inode and generation authority")
			}
			if !canonical(root.LeasePath) || !beneath(volume.MountPath, root.LeasePath) || root.LeasePath == volume.MarkerPath || !validDigest(root.LeaseSha256) || leasePaths[root.LeasePath] {
				return errors.New("durable owner lease identity is absent, ambiguous or outside its mount")
			}
			leasePaths[root.LeasePath] = true
			identityPaths = append(identityPaths, root.LeasePath)
			for _, prior := range roots {
				if beneath(prior, root.Path) || beneath(root.Path, prior) {
					return errors.New("durable owner roots overlap")
				}
			}
			roots = append(roots, root.Path)
		}
	}
	for _, path := range identityPaths {
		for _, root := range roots {
			if beneath(root, path) {
				return errors.New("durable identity or lease is inside a journal root")
			}
		}
	}
	return nil
}

// Duplicate keys cannot change the declaration selected by a different parser.
func decodeStrict(raw []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	var scan func(int) error
	scan = func(depth int) error {
		if depth > 32 {
			return errors.New("durable JSON nesting exceeds its bound")
		}
		token, err := decoder.Token()
		if err != nil {
			return err
		}
		delimiter, ok := token.(json.Delim)
		if !ok {
			return nil
		}
		if delimiter != '{' && delimiter != '[' {
			return errors.New("durable JSON delimiter is invalid")
		}
		keys := map[string]bool{}
		for decoder.More() {
			if delimiter == '{' {
				key, err := decoder.Token()
				if err != nil {
					return err
				}
				name, ok := key.(string)
				if !ok || keys[name] {
					return errors.New("durable JSON contains a duplicate key")
				}
				keys[name] = true
			}
			if err := scan(depth + 1); err != nil {
				return err
			}
		}
		_, err = decoder.Token()
		return err
	}
	if err := scan(0); err != nil {
		return err
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return errors.New("durable JSON has trailing input")
	}
	decoder = json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	return decoder.Decode(target)
}

// Reads only the exact protected external declaration; never mutates a path.
func Load(reference Reference) (Config, error) {
	return loadForScope(reference, daemonScope)
}

// This entry point accepts only the independently reviewed owner-local schema;
// daemon declarations cannot silently change meaning at the signing boundary.
func LoadOwnerLocal(reference Reference) (Config, error) {
	return loadForScope(reference, ownerLocalScope)
}

// Hash and protected-file checks are identical for both explicit scopes.
func loadForScope(reference Reference, scope ownerScope) (Config, error) {
	if !canonical(reference.Path) || !validDigest(reference.Sha256) {
		return Config{}, errors.New("durable volume declaration path and hash are required")
	}
	raw, err := readProtectedFile(reference.Path, maximumConfigBytes)
	if err != nil {
		return Config{}, err
	}
	digest := sha256.Sum256(raw)
	if "sha256:"+hex.EncodeToString(digest[:]) != reference.Sha256 {
		return Config{}, errors.New("durable volume declaration bytes differ")
	}
	var config Config
	if err := decodeStrict(raw, &config); err != nil {
		return Config{}, fmt.Errorf("durable volume declaration: %w", err)
	}
	if err := config.validateForScope(scope); err != nil {
		return Config{}, err
	}
	return config, nil
}

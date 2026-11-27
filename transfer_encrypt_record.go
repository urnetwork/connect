package connect

// TLS exporter derivation, authenticated record bytes and their pooled wire envelope.
import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/tls"
	"errors"

	"github.com/urnetwork/connect/protocol"
)

const (
	sequenceTlsKeyLabel      = "urnetwork-connect-aead"
	sequenceTlsKeyLength     = 32
	sequenceTlsAeadNonceSize = 12
	// sequenceTlsIdentityProofLabel is the exporter label both peers use to
	// derive the 32-byte payload signed with the long-lived client identity
	// key. Distinct from `sequenceTlsKeyLabel` so the two derivations are
	// cryptographically separated (per RFC 5705 exporter rules).
	sequenceTlsIdentityProofLabel  = "urnetwork-connect-identity-proof"
	sequenceTlsIdentityProofLength = 32
)

// sequenceCipher is the AEAD used to outer-wrap a whole TransferFrame after
// the per-peer TLS handshake completes. Nonce is random per-message and
// prepended to the ciphertext.
type sequenceCipher struct {
	aead cipher.AEAD
}

// Derive both secrets from the same completed TLS state. Publish neither
// when one export fails. The labels and record format are wire commitments.
func deriveSequenceSecrets(state tls.ConnectionState) (*sequenceCipher, []byte, error) {
	if !state.HandshakeComplete {
		return nil, nil, errors.New("sequence secrets require a completed TLS handshake")
	}
	key, err := state.ExportKeyingMaterial(sequenceTlsKeyLabel, nil, sequenceTlsKeyLength)
	if err != nil {
		return nil, nil, err
	}
	defer clear(key)
	proof, err := state.ExportKeyingMaterial(sequenceTlsIdentityProofLabel, nil, sequenceTlsIdentityProofLength)
	if err != nil {
		return nil, nil, err
	}
	record, err := newSequenceRecordCipher(key)
	if err != nil {
		return nil, nil, err
	}
	return record, proof, nil
}

// Construct the wire record primitive independently of TLS or a peer session.
func newSequenceRecordCipher(key []byte) (*sequenceCipher, error) {
	if len(key) != sequenceTlsKeyLength {
		return nil, errors.New("sequence records require an AES-256 key")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &sequenceCipher{aead: aead}, nil
}

// Seal one record using the nonce already appended at the end of dst.
func (self *sequenceCipher) sealRecord(dst, plaintext []byte) []byte {
	return self.aead.Seal(dst, dst[len(dst)-sequenceTlsAeadNonceSize:], plaintext, nil)
}

// Produce an independent wire record using a fresh nonce.
func (self *sequenceCipher) Seal(plaintext []byte) ([]byte, error) {
	out := make([]byte, sequenceTlsAeadNonceSize, sequenceTlsAeadNonceSize+len(plaintext)+self.aead.Overhead())
	if _, err := rand.Read(out); err != nil {
		return nil, err
	}
	return self.sealRecord(out, plaintext), nil
}

// Return authenticated plaintext in a pooled buffer owned by the caller.
func (self *sequenceCipher) Open(ciphertext []byte) ([]byte, error) {
	if len(ciphertext) < sequenceTlsAeadNonceSize+self.aead.Overhead() {
		return nil, errors.New("sequence cipher: ciphertext too short")
	}
	nonce := ciphertext[:sequenceTlsAeadNonceSize]
	sealed := ciphertext[sequenceTlsAeadNonceSize:]
	plaintextLen := len(sealed) - self.aead.Overhead()
	buf := MessagePoolGet(plaintextLen)
	out, err := self.aead.Open(buf[:0], nonce, sealed, nil)
	if err != nil {
		MessagePoolReturn(buf)
		return nil, err
	}
	if cap(out) != cap(buf) {
		MessagePoolReturn(buf)
	}
	return out, nil
}

// SealOuterFrame writes the complete encrypted outer TransferFrame directly
// into one pooled buffer. AEAD.Seal appends to dst, so placing the random nonce
// at the end of the protobuf header and sealing from there avoids both the
// standalone ciphertext allocation and the subsequent ciphertext copy that
// buildEncryptedOuterFrameBytes would otherwise require.
func (self *sequenceCipher) SealOuterFrame(
	path TransferPath,
	plaintext []byte,
	sessionRole protocol.SequenceRole,
	sessionCompanion bool,
) ([]byte, error) {
	ciphertextLen := sequenceTlsAeadNonceSize + len(plaintext) + self.aead.Overhead()
	size := sizeEncryptedOuterTransferFrame(path, ciphertextLen, sessionRole)
	buf := MessagePoolGet(size)
	out := appendEncryptedOuterTransferFrameHeader(buf[:0], path, ciphertextLen)

	nonceStart := len(out)
	out = out[:nonceStart+sequenceTlsAeadNonceSize]
	nonce := out[nonceStart:]
	if _, err := rand.Read(nonce); err != nil {
		MessagePoolReturn(buf)
		return nil, err
	}
	out = self.sealRecord(out, plaintext)
	out = appendEncryptedOuterTransferFrameFooter(out, sessionRole, sessionCompanion)
	if cap(out) != cap(buf) {
		MessagePoolReturn(buf)
	}
	return out, nil
}

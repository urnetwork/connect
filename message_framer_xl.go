package connect

import (
	"encoding/binary"
	"fmt"
	"io"
	"math"
)

const (
	framerXlHeaderLen       = 4
	framerXlWritePrefixLen  = 2 * 1024
	framerXlDefaultBatchLen = 16 * 1024
)

// FramerXlSettings configures the urnetwork-framerxl/1 stream format. Its
// four-byte header is a uint32 big-endian payload length, with no split hint.
// This wire format is different from Framer and must be negotiated explicitly.
type FramerXlSettings struct {
	// Log defaults to DefaultLogger when nil.
	Log Logger

	// MaxMessageLen is the explicit site-local payload limit. It must be
	// nonnegative, fit uint32, and leave room in int for framing and pool
	// metadata. A zero limit permits only empty frames. A uint32 wire field
	// does not itself grant permission to allocate a uint32-sized message.
	MaxMessageLen int

	// MaxBatchLen bounds the total wire bytes in a coalesced multi-message
	// batch, including every four-byte header. It must be at least four.
	// It bounds WriteBatch's temporary allocation independently of the
	// maximum message size; it is not a per-message limit. A large singleton
	// uses Write's bounded-prefix path, or caller-owned storage if supplied.
	MaxBatchLen int
}

// DefaultFramerXlSettings requires the endpoint's maximum payload explicitly.
// Multi-message coalescing is limited to 16 KiB by default; no message-sized
// scratch or receive storage is retained by the framer.
func DefaultFramerXlSettings(maxMessageLen int) *FramerXlSettings {
	return &FramerXlSettings{
		MaxMessageLen: maxMessageLen,
		MaxBatchLen:   framerXlDefaultBatchLen,
	}
}

// FramerXl preserves message boundaries on a stream using a four-byte uint32
// payload length. One reader and one writer may operate concurrently, with a
// single owner in each direction. Concurrent reads or concurrent writes are
// unsupported. FramerXl retains no payload, scratch buffer, or stream reference.
// Settings are snapshotted at construction and must not be mutated concurrently
// with construction. Per-direction in-flight and queue budgets remain the
// endpoint's responsibility.
type FramerXl struct {
	maxMessageLen int
	maxBatchLen   int
	settingsErr   error
	log           Logger
}

// NewFramerXl snapshots settings. Invalid settings make every operation fail
// before reading, writing, or allocating a message; they never silently widen
// the endpoint's limit.
func NewFramerXl(settings *FramerXlSettings) *FramerXl {
	f := &FramerXl{}
	if settings == nil {
		f.settingsErr = fmt.Errorf("FramerXl settings are required")
		return f
	}
	f.maxMessageLen = settings.MaxMessageLen
	f.maxBatchLen = settings.MaxBatchLen
	f.log = loggerOrDefault(settings.Log)
	if settings.MaxMessageLen < 0 || uint64(settings.MaxMessageLen) > math.MaxUint32 ||
		settings.MaxMessageLen > math.MaxInt-framerXlHeaderLen-MessagePoolMetaByteCount {
		f.settingsErr = fmt.Errorf("invalid FramerXl MaxMessageLen %d", settings.MaxMessageLen)
	} else if settings.MaxBatchLen < framerXlHeaderLen || settings.MaxBatchLen > math.MaxInt-MessagePoolMetaByteCount {
		f.settingsErr = fmt.Errorf("invalid FramerXl MaxBatchLen %d", settings.MaxBatchLen)
	}
	return f
}

// ReadHeader reads exactly one four-byte header and validates its declared
// payload length before any payload allocation. The caller must consume exactly
// the returned number of payload bytes before reading another header. A short
// header or payload is terminal for this stream; do not resume at another frame.
// It does not read ahead, allocate payload storage, or consume the frame body.
func (f *FramerXl) ReadHeader(r io.Reader) (int, error) {
	if f.settingsErr != nil {
		return 0, f.settingsErr
	}
	var header [framerXlHeaderLen]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return 0, err
	}
	wireLen := binary.BigEndian.Uint32(header[:])
	// Compare before converting to int, including on 32-bit platforms.
	if uint64(wireLen) > uint64(f.maxMessageLen) {
		if f.log != nil {
			f.log.Infof("[framerxl][reject]read messageLen=%d > MaxMessageLen=%d\n", wireLen, f.maxMessageLen)
		}
		return 0, fmt.Errorf("FramerXl message length %d exceeds maximum %d", wireLen, f.maxMessageLen)
	}
	return int(wireLen), nil
}

// Read returns exactly one message. The caller owns a successful result and
// must call MessagePoolReturn when finished (also for zero-length messages).
// On failure no buffer is returned and any partially filled allocation is
// released. Frames larger than a MessagePool size class are not retained in
// the pool after release. Use ReadHeader with bounded streaming consumption
// when the endpoint need not materialize a large message.
func (f *FramerXl) Read(r io.Reader) ([]byte, error) {
	messageLen, err := f.ReadHeader(r)
	if err != nil {
		return nil, err
	}
	message := MessagePoolGet(messageLen)
	if _, err := io.ReadFull(r, message); err != nil {
		MessagePoolReturn(message)
		return nil, err
	}
	return message, nil
}

// Write emits one frame. It copies at most 2 KiB (including the header) into a
// temporary pooled prefix; any remaining payload is passed directly to the
// writer in a second write. This bounds scratch independently of message size.
// WriteBatchWithStorage can avoid that second stream handoff when sufficiently
// large caller-owned storage is already available. Neither method takes
// ownership of message or changes its bytes. These APIs are for byte streams,
// not packet transports whose individual Write boundaries carry meaning.
//
// A short write or any write error is terminal, even with full byte progress;
// the caller must close the stream, not retry a partially emitted frame.
func (f *FramerXl) Write(w io.Writer, message []byte) error {
	if err := f.validateMessageLen(len(message)); err != nil {
		return err
	}
	prefixLen := min(framerXlWritePrefixLen, len(message)+framerXlHeaderLen)
	prefix := MessagePoolGet(prefixLen)
	defer MessagePoolReturn(prefix)
	binary.BigEndian.PutUint32(prefix[:framerXlHeaderLen], uint32(len(message)))
	copy(prefix[framerXlHeaderLen:], message)
	if err := writeFramerXlBytes(w, prefix); err != nil {
		return err
	}
	if copied := prefixLen - framerXlHeaderLen; copied < len(message) {
		return writeFramerXlBytes(w, message[copied:])
	}
	return nil
}

// WriteBatch validates the entire batch before emitting bytes. Multiple frames
// are coalesced into one write using at most MaxBatchLen bytes of temporary
// pooled scratch. A singleton uses Write and may exceed MaxBatchLen, up to its
// independent MaxMessageLen. Empty batches do no I/O. Input ownership always
// stays with the caller, including on errors.
func (f *FramerXl) WriteBatch(w io.Writer, messages [][]byte) error {
	if f.settingsErr != nil {
		return f.settingsErr
	}
	if len(messages) == 0 {
		return nil
	}
	if len(messages) == 1 {
		return f.Write(w, messages[0])
	}
	byteCount, err := f.writeBatchByteCount(messages)
	if err != nil {
		return err
	}
	storage := MessagePoolGet(byteCount)
	defer MessagePoolReturn(storage)
	return writeFramerXlBatch(w, messages, storage)
}

// WriteBatchWithStorage coalesces the frames into exclusive caller-owned
// scratch and performs one stream write. Storage must not overlap any input
// message and must remain exclusive until the call returns; input messages may
// share read-only backing with each other. The framer retains neither.
// Insufficient singleton scratch falls back to Write's bounded prefix; an
// undersized multi-message batch fails before any output. MaxBatchLen applies
// to multi-message batches even when the caller supplies larger storage.
func (f *FramerXl) WriteBatchWithStorage(w io.Writer, messages [][]byte, storage []byte) error {
	if f.settingsErr != nil {
		return f.settingsErr
	}
	if len(messages) == 0 {
		return nil
	}
	byteCount, err := f.writeBatchByteCount(messages)
	if err != nil {
		return err
	}
	if len(storage) < byteCount {
		if len(messages) == 1 {
			return f.Write(w, messages[0])
		}
		return fmt.Errorf("FramerXl batch storage too small (%d<%d)", len(storage), byteCount)
	}
	return writeFramerXlBatch(w, messages, storage[:byteCount])
}

func (f *FramerXl) validateMessageLen(messageLen int) error {
	if f.settingsErr != nil {
		return f.settingsErr
	}
	if messageLen < 0 || messageLen > f.maxMessageLen {
		if f.log != nil {
			f.log.Infof("[framerxl][reject]write messageLen=%d > MaxMessageLen=%d\n", messageLen, f.maxMessageLen)
		}
		return fmt.Errorf("FramerXl message length %d exceeds maximum %d", messageLen, f.maxMessageLen)
	}
	return nil
}

// Validate every length and checked sum before any output or allocation. The
// subtraction form avoids overflowing while adding headers on 32-bit hosts.
func (f *FramerXl) writeBatchByteCount(messages [][]byte) (int, error) {
	if f.settingsErr != nil {
		return 0, f.settingsErr
	}
	total := 0
	for _, message := range messages {
		if err := f.validateMessageLen(len(message)); err != nil {
			return 0, err
		}
		var err error
		total, err = framerXlAddFrameByteCount(total, len(message))
		if err != nil {
			return 0, err
		}
		if len(messages) > 1 && total > f.maxBatchLen {
			return 0, fmt.Errorf("FramerXl batch length %d exceeds maximum %d", total, f.maxBatchLen)
		}
	}
	return total, nil
}

func framerXlAddFrameByteCount(total, messageLen int) (int, error) {
	if total < 0 || messageLen < 0 || math.MaxInt-framerXlHeaderLen-total < messageLen {
		return 0, fmt.Errorf("FramerXl batch byte count overflow")
	}
	return total + framerXlHeaderLen + messageLen, nil
}

func writeFramerXlBatch(w io.Writer, messages [][]byte, storage []byte) error {
	offset := 0
	for _, message := range messages {
		binary.BigEndian.PutUint32(storage[offset:offset+framerXlHeaderLen], uint32(len(message)))
		offset += framerXlHeaderLen
		copy(storage[offset:offset+len(message)], message)
		offset += len(message)
	}
	return writeFramerXlBytes(w, storage)
}

func writeFramerXlBytes(w io.Writer, message []byte) error {
	if n, err := w.Write(message); err != nil {
		return err
	} else if n != len(message) {
		return io.ErrShortWrite
	}
	return nil
}

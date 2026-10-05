// The package's complete error contract. Every failure path returns one of these
// sentinels, possibly joined with another for context. Callers compare with
// errors.Is and never parse a string, so the messages are for humans only.
package syntax

import "errors"

var (
	// ErrTruncated fires when input ended before a value finished decoding.
	ErrTruncated = errors.New("mls syntax: input truncated")
	// ErrTrailingBytes fires when a top-level decode left bytes unconsumed.
	ErrTrailingBytes = errors.New("mls syntax: trailing bytes after top level value")
	// ErrVarintReserved fires when a varint prefix 0b11 is encountered.
	ErrVarintReserved = errors.New("mls syntax: varint prefix 0b11 is reserved")
	// ErrVarintNotMinimal fires when a varint used more octets than the value needs.
	ErrVarintNotMinimal = errors.New("mls syntax: varint is not minimally encoded")
	// ErrVarintOverflow fires on encode when a value exceeds MaxVarint.
	ErrVarintOverflow = errors.New("mls syntax: varint value exceeds 2^30-1")
	// ErrLengthExceedsInput fires when a declared length exceeds bytes remaining in input.
	ErrLengthExceedsInput = errors.New("mls syntax: declared length exceeds remaining input")
	// ErrLengthExceedsMax fires when a declared length exceeds the configured maximum.
	ErrLengthExceedsMax = errors.New("mls syntax: declared length exceeds the configured maximum")
	// ErrOptionalPresence fires when a presence octet is neither 0 nor 1.
	ErrOptionalPresence = errors.New("mls syntax: optional presence octet is neither 0 nor 1")
	// ErrZeroLengthElement fires when a vector element decoder consumed zero bytes.
	ErrZeroLengthElement = errors.New("mls syntax: vector element consumed zero bytes")
	// ErrNegativeLength fires on API misuse: caller passed a negative maximum to a limit-taking entry point, or an int conversion underflowed on a 32-bit target.
	ErrNegativeLength = errors.New("mls syntax: negative length")
	// ErrRoundTripNotByteExact fires when re-encoding an accepted value did not reproduce its bytes.
	ErrRoundTripNotByteExact = errors.New("mls syntax: re-encoding an accepted value did not reproduce its bytes")
	// ErrRoundTripNotStable fires when decoding a re-encoded value did not reproduce the original value.
	ErrRoundTripNotStable = errors.New("mls syntax: decoding a re-encoded value did not reproduce the value")
)

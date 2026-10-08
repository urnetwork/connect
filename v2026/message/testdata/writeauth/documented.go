//go:build ignore

// The negative control beside violations.go: every banned act named in prose and nowhere
// else. Without this file a matcher that answered yes to everything would pass the
// positive control and fail nothing, and the gates would report a clean package because
// they report the same thing for a clean package and for a broken rule.
//
// The gates parse without comments, so nothing here is visible to them at all — which is
// the property this file exists to assert rather than assume. It matters because the
// comment that teaches a rule is the comment that names what the rule bans, and a gate
// that fires on the sentence teaching it is a gate the next contributor deletes.
package writeauth

import "crypto/subtle"

// A verifier must never decide with bytes.Equal, and must never compare two tags with ==,
// because either one answers in a time that depends on how many leading octets matched.
func VerifyDocumented(tag []byte, carried []byte) bool {
	return subtle.ConstantTimeCompare(tag, carried) == 1
}

// The read path must never derive a key under "write/v1" — the label belongs to the write
// key, and a request macd under it is one the server cannot resolve for a member that was
// offline across a commit.
func ComputeRequestAuthDocumented(readKey []byte) []byte {
	return readKey
}

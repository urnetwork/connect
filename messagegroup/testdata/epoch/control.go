// The positive control for epoch_test.go's two walks, in the shape message/testdata/writeauth is
// the control for TestReadAuthNeverUsesWriteKey.
//
// Without it the gates over the real package prove nothing: they report clean, and a walk that
// followed no edge at all reports clean too. Every shape below is one the real rule has to see, and
// each is present in BOTH directions -- a sampler that must be flagged and a sampler with the same
// number of hops that must not.
//
// It lives under testdata so the go tool cannot build it, which is what lets it hold declarations
// that would fail this package's own gates -- a second parameter beside the entropy source, a
// package level reader, an argument that is never used.
package control

import "io"

// The stand in for keyschedule.go's crypto provider. The derivation class is derived off the
// OPERATION -- an Expand, an Extract or an hkdf entry point -- so the identical rule runs over this
// file and over the real one with nothing named on either side.
var controlCrypto = struct{}{}

// Three hops down to the provider, because a rule that only looked at the immediate callee would
// clear every sampler that derived through one helper.
func expandFrom(root []byte) []byte {
	return derive(root)
}

func derive(root []byte) []byte {
	return kdf(root)
}

func kdf(root []byte) []byte {
	return controlCrypto.Expand(root, "control/v1", 32)
}

// A package level source, which is the fallback shape: a sampler that draws from this rather than
// from its argument produces a perfectly good secret and honours nothing the caller asked for.
var processSource io.Reader

// CLEAN. Reads the source it was handed, directly.
func SamplerThatReads(random io.Reader) ([]byte, error) {
	secret := make([]byte, 32)
	if _, err := io.ReadFull(random, secret); err != nil {
		return nil, err
	}
	return secret, nil
}

// CLEAN. Reads the source it was handed, two hops away and under two different parameter names,
// which is what says the carrier is followed through the call graph rather than matched by
// spelling.
func SamplerThatReadsViaHelper(random io.Reader) ([]byte, error) {
	return drawFrom(random)
}

func drawFrom(source io.Reader) ([]byte, error) {
	return fill(source)
}

func fill(from io.Reader) ([]byte, error) {
	secret := make([]byte, 32)
	if _, err := from.Read(secret); err != nil {
		return nil, err
	}
	return secret, nil
}

// FLAGGED by the reads-its-source half. It ignores its argument and answers a constant, which is
// the shape a sampler takes when somebody replaces the draw with a fixture.
func SamplerThatIgnoresItsReader(random io.Reader) ([]byte, error) {
	return make([]byte, 32), nil
}

// FLAGGED by the reads-its-source half. It draws thirty two good octets out of a source the caller
// never named, which is the substitution every behavioural test passes.
func SamplerThatReadsAnotherSource(random io.Reader) ([]byte, error) {
	secret := make([]byte, 32)
	if _, err := io.ReadFull(processSource, secret); err != nil {
		return nil, err
	}
	return secret, nil
}

// FLAGGED by the reaches-no-derivation half and by the signature half. It really does read its
// source -- so the two halves are independent, and a rule that only checked the read would clear
// it -- and it also reaches the provider three hops down, from a storage root it should never have
// been able to name.
func SamplerThatDerives(random io.Reader, storageRoot []byte) ([]byte, error) {
	secret := make([]byte, 32)
	if _, err := io.ReadFull(random, secret); err != nil {
		return nil, err
	}
	return expandFrom(storageRoot), nil
}

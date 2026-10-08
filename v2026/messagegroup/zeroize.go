// Best effort erasure of key material for the client half of the record layer, in one function
// so every caller in this package erases the same way and an audit reads one place rather than
// a loop at each drop site.
//
// What this can promise and what it cannot, because the name promises more than go delivers.
// The write lands on the backing array the caller's slice header points at, so the caller's own
// slice and every other live slice over that same array observe zeros afterwards; that much is
// observable and is what the test asserts, through a second header over the same array so the
// check cannot be satisfied by reslicing. It cannot reach a COPY -- an append that grew, a
// slice literal built from the secret, a string conversion, a struct field assigned from it, an
// interface boxing it, and every value the garbage collector moved during a growth are separate
// arrays this function was never handed. Nor can any go program guarantee a secret was never
// left in a register or in a dead stack frame. So this removes the obvious copy, not every
// copy, and the discipline that makes it worth anything is upstream: derive a secret into a
// slice, pass that slice, do not copy it.
//
// It is a second implementation of connect/mls's zeroizeSecret and that is a choice rather than
// an oversight. mls.zeroizeSecret is unexported and cannot be called from here; exporting it is
// a one character change and this package already imports connect/mls in production, so the
// alternative was available. It was not taken because connect/mls's exported surface belongs to
// the plan that built it and secret_zeroize.go's own comment argues against additions at length,
// and a second package's convenience is the weakest argument there is for widening another
// package's API. The cost is visible and bounded: four lines and a pragma, in a shape the two
// copies cannot drift apart in without the noinline gate below noticing. Open item M1-44
// records the choice so it is made once and on the record rather than re-argued at each
// consumer.
//
// There is no unsafe.Pointer here, which spec A section 5.5 specifies. connect/mls answered the
// same requirement with the pragma and a plain loop, and its production code contains no unsafe
// at all; matching the tree is worth more than matching a sentence of the spec that the tree
// already declined once, and open item M1-37 records the divergence so a reader of section 5.5
// does not restore it. There is no runtime.KeepAlive either: KeepAlive extends an object's
// reachability for the collector, and what this needs is for the stores not to be optimised
// away, which is what the pragma is for. Importing runtime to get it would widen the set of
// packages this one is built from, and that set is pinned by a gate in connect/mls.
package messagegroup

// zeroize overwrites secret's bytes with zero, in place.
//
// nil and a zero length slice are accepted rather than guarded against at the call site,
// because a guard needed at every call site is a guard that will be missing at one of them, and
// "erase this optional secret" is the common shape here. A key that was already erased is
// erased again on every path that erases, so a helper that panicked on the empty case would
// turn a double erase into a crash on the receive path.
//
// The noinline directive is why the stores are likely to reach memory. A compiler may delete a
// write to memory it can prove is never read again, and in a caller that drops the secret
// immediately afterwards that is exactly what these writes are; across a call it cannot inline,
// it cannot make that proof. That is a property of today's compiler rather than a guarantee the
// language makes, which is why the file comment calls this best effort -- and it is why the
// pragma is asserted off the source rather than off behaviour, since no test can observe a
// store the compiler kept.
//
//go:noinline
func zeroize(secret []byte) {
	for i := range secret {
		secret[i] = 0
	}
}

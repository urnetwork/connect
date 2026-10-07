// Best effort erasure of key material, in one function so every caller in the key
// schedule erases the same way and an audit reads one place rather than nine loops.
//
// What this can promise and what it cannot, stated here because the name promises more
// than go can deliver. The write lands on the backing array the caller's slice header
// points at, so the caller's own slice and every other live slice over that same array
// observe zeros afterwards — that much is observable and is what the tests assert. It
// cannot reach a COPY: an append that grew, a slice literal built from the secret, a
// string conversion, a struct field assigned from it, an interface boxing it, and every
// value the garbage collector moved during a growth are separate arrays this function was
// never handed, and go offers no way to enumerate or reach them. Nor can any go program
// guarantee the secret was never left in a register or in a stack frame that is now
// garbage.
//
// So this removes the obvious copy, not every copy. The discipline that makes it worth
// anything is upstream of it: derive a secret into a slice, pass that slice, and do not
// copy it. A test claiming more than the paragraph above would be claiming something go
// cannot observe.
//
// There is no runtime.KeepAlive here, which the first draft of this file carried. It
// would buy nothing: KeepAlive extends an object's reachability for the garbage
// collector, and what this function needs is for the stores not to be optimised away,
// which is what the noinline directive is for. Importing runtime to get it would widen
// the set of packages this one is built from — a set another gate pins exactly, and
// which is the record of everything the crypto can reach.
package mls

// zeroizeSecret overwrites secret's bytes with zero, in place.
//
// nil and a zero length slice are accepted rather than guarded against at the call site,
// because a guard needed at every call site is a guard that will be missing at one of
// them, and "erase this optional secret" is the common shape here.
//
// The noinline directive is why the stores are likely to reach memory. A compiler may
// delete a write to memory it can prove is never read again, and in a caller that drops
// the secret immediately afterwards that is exactly what these writes are; across a call
// it cannot inline, it cannot make that proof. That is a property of today's compiler
// rather than a guarantee the language makes, which is why the file comment above calls
// this best effort.
//
//go:noinline
func zeroizeSecret(secret []byte) {
	for i := range secret {
		secret[i] = 0
	}
}

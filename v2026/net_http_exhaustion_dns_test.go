// Concrete DNS leaves retain their own transport meaning through public HTTP
// exhaustion, while explicit children and permanent metadata retain priority.
package connect

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"testing"
)

// A nil DNS UnwrapErr is a complete standard-library leaf, not a foreign
// wrapper with a missing cause. Both public owners preserve it and cancellation.
func TestHttpRequestExhaustionPublicRetainsTerminalDnsTransport(t *testing.T) {
	for _, serial := range []bool{false, true} {
		for _, end := range []error{context.DeadlineExceeded, context.Canceled} {
			for _, dns := range []*net.DNSError{
				{Err: "synthetic resolver timeout", Name: "timeout.example", IsTimeout: true},
				{Err: "synthetic resolver outage", Name: "temporary.example", IsTemporary: true},
			} {
				err := runHttpExhaustionTest(t, serial, end, dns)
				exhausted, ok := err.(*HttpRequestExhaustedError)
				if !ok {
					t.Fatalf("public DNS exhaustion lost its owner: %T", err)
				}
				foundDns, foundEnd := false, false
				for _, cause := range exhausted.causes {
					foundDns = foundDns || cause == dns
					foundEnd = foundEnd || cause == end
					if cause == errHttpExhaustionCauseIncomplete || cause == errHttpExhaustionCauseTraversal {
						t.Fatalf("serial=%t concrete DNS leaf acquired a false graph refusal", serial)
					}
				}
				if !foundDns || !foundEnd {
					t.Fatalf("serial=%t DNS leaf or actual caller end was lost", serial)
				}
				retained := flattenHttpRequestCauses(dns)
				if len(retained) != 1 || retained[0].err != dns || retained[0].kind == 0 {
					t.Fatal("publicly retained concrete DNS leaf lost transport authority")
				}
			}
		}
	}
}

// Contradictory DNS flags cannot soften not-found. A complete unflagged leaf
// also remains hard, without inferring permanence from absent flags on a wrapper.
func TestHttpRequestExhaustionPublicKeepsPermanentDnsCause(t *testing.T) {
	for _, serial := range []bool{false, true} {
		for _, hard := range []*net.DNSError{
			{Err: "synthetic absent name", Name: "absent.example", IsNotFound: true, IsTimeout: true},
			{Err: "synthetic absent name", Name: "absent.example", IsNotFound: true, IsTemporary: true, UnwrapErr: io.EOF},
			{Err: "synthetic permanent resolver failure", Name: "permanent.example"},
		} {
			transient := &net.DNSError{Err: "synthetic prior resolver timeout", Name: "timeout.example", IsTimeout: true}
			err := runHttpExhaustionTest(t, serial, context.DeadlineExceeded, errors.Join(transient, hard))
			exhausted, ok := err.(*HttpRequestExhaustedError)
			if !ok {
				t.Fatalf("public DNS exhaustion lost its owner: %T", err)
			}
			foundTransient, foundHard := false, false
			for _, cause := range exhausted.causes {
				foundTransient = foundTransient || cause == transient
				foundHard = foundHard || cause == hard
			}
			if !foundTransient || !foundHard {
				t.Fatalf("serial=%t hard DNS cause borrowed a transient sibling's bucket", serial)
			}
		}
	}
}

// The concrete DNS exception applies only to an absent optional child. Real
// children remain in the bounded graph, including custody and typed nil causes.
func TestHttpRequestExhaustionPublicInspectsExplicitDnsChild(t *testing.T) {
	for _, serial := range []bool{false, true} {
		hard := &os.PathError{Op: "read", Path: "synthetic-dns-custody", Err: context.DeadlineExceeded}
		for _, fixture := range []struct {
			child    error
			expected error
		}{
			{child: errors.Join(io.ErrUnexpectedEOF, hard), expected: hard},
			{child: (*net.DNSError)(nil), expected: errHttpExhaustionCauseIncomplete},
		} {
			dns := &net.DNSError{Err: "synthetic resolver timeout", Name: "timeout.example", IsTimeout: true, UnwrapErr: fixture.child}
			err := runHttpExhaustionTest(t, serial, context.Canceled, dns)
			exhausted, ok := err.(*HttpRequestExhaustedError)
			if !ok {
				t.Fatalf("public DNS exhaustion lost its owner: %T", err)
			}
			foundChild, foundCancel := false, false
			for _, cause := range exhausted.causes {
				foundChild = foundChild || cause == fixture.expected
				foundCancel = foundCancel || cause == context.Canceled
			}
			if !foundChild || !foundCancel {
				t.Fatalf("serial=%t DNS flags hid an explicit hard child or caller cancellation", serial)
			}
		}
	}
}

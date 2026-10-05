// The RFC 9420 section 12.4 Commit wire type and its codec.
//
// Codec only: proposal-list validation and commit application are the group lifecycle's, and
// nothing here decides whether a commit is one this member will accept.
//
// path is optional<UpdatePath>: a presence octet, then the value when present. The RFC's
// optional<T> has exactly TWO encodings and a third would be a second encoding of one object --
// which is the signature-bypass primitive, because a signature is over one serialization and a
// receiver that accepted two readings of the same bytes has two objects claiming one signature.
// The presence octet is therefore syntax's ReadOptional/WriteOptional and its refusal is
// syntax.ErrOptionalPresence: nothing about presence is spelled out here, so there is one place
// in the system that decides what optional<T> means.
//
// The proposals vector is syntax's free generic WriteVector/ReadVector over a typed slice rather
// than an index loop of this file's own. The element type is what makes ReadVector safe, and an
// untyped loop is exactly where it would be lost.
package mls

import (
	"github.com/urnetwork/connect/mls/syntax"
)

// Commit is the message that ends an epoch: the proposals it applies, and the path that
// re-keys the sender's direct path.
//
// A commit with no path is legal -- an add-only commit needs no fresh path secrets -- and
// encodes to an empty proposals vector plus the absent-optional octet.
type Commit struct {
	Proposals []ProposalOrRef
	Path      *UpdatePath
}

// The element codecs of the proposals vector, as named functions rather than as closures at
// the call site -- tree.go's and treekem.go's spelling. A closure renders as its whole body
// wherever this package's source is read, and the gate that pins every syntax entry point to
// the default vector limit reads exactly that text: a named pair keeps one decision on one
// line where a reviewer can see which limit it runs under.
func writeOneProposalOrRef(w *syntax.Writer, item ProposalOrRef) error {
	return item.MarshalMLS(w)
}

func readOneProposalOrRef(r *syntax.Reader) (ProposalOrRef, error) {
	proposalOrRef := ProposalOrRef{}
	if err := proposalOrRef.UnmarshalMLS(r); err != nil {
		return ProposalOrRef{}, err
	}
	return proposalOrRef, nil
}

func (self *Commit) MarshalMLS(w *syntax.Writer) error {
	err := syntax.WriteVector(w, self.Proposals, writeOneProposalOrRef)
	if err != nil {
		return err
	}
	return w.WriteOptional(self.Path != nil, func(w *syntax.Writer) error {
		return self.Path.MarshalMLS(w)
	})
}

// UnmarshalMLS reads the proposals vector, then the optional path.
//
// The path is decoded into a local and attached only when the presence octet said present, so a
// commit that carries no path leaves Path nil rather than pointing at a zero valued UpdatePath.
// The two are not the same commit: a zero valued path has no nodes and an empty leaf node, and a
// receiver that merged one would blank the sender's direct path rather than leaving it alone.
func (self *Commit) UnmarshalMLS(r *syntax.Reader) error {
	proposals, err := syntax.ReadVector(r, readOneProposalOrRef)
	if err != nil {
		return err
	}
	path := &UpdatePath{}
	present, err := r.ReadOptional(func(r *syntax.Reader) error {
		return path.UnmarshalMLS(r)
	})
	if err != nil {
		return err
	}
	decoded := Commit{Proposals: proposals}
	if present {
		decoded.Path = path
	}
	*self = decoded
	return nil
}

var _ syntax.Codec = (*Commit)(nil)

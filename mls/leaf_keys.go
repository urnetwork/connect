// Reading the urmessage_leaf_keys leaf node extension off a leaf, MASTER section 5.3 and
// spec A section 3.4.
//
// The extension type, its struct, its codec and the two range checks on it all belong to the
// leaf node file family, because LeafNode.Validate is what holds a leaf's X-Wing key to
// XwingPublicKeyLen and it has to do that in the same wave the leaf lands in. What is here is
// only the group lifecycle's accessor, and nothing else belongs here: a second alg_id check or
// a second length check written beside this call is the drift extension.go's own briefing warns
// about, since a length check written against a different constant is how two constants come
// to disagree.
//
// A member with no X-Wing key cannot receive the epoch wrap, which is why 0xF002 is in this
// profile's required_capabilities and why absence is an error here rather than a nil result.
package mls

import "fmt"

// LeafKeysOf extracts and parses the urmessage_leaf_keys extension of a leaf node.
//
// The lookup is by extension TYPE and the entry it finds is handed to ParseLeafKeysFrom whole,
// which is the documented path on LeafKeysExtension and not an ornament on it. ParseLeafKeysFrom
// is the only entry point of that pair that is given the tag, so it is the only one that can
// refuse a body arriving under somebody else's; handing it the entry rather than the entry's
// body means the tag this accessor selected on and the tag the parse is held to are the same
// value, checked twice, rather than one value trusted once. An accessor that read
// leaf.Extensions[0] would answer a wrap target out of whatever extension happened to sit in
// that slot, and a commit secret wrapped to it goes to nobody.
//
// A leaf carrying urmessage_leaf_keys TWICE is refused rather than answered from, and the
// refusal is made by the LOOKUP rather than restated here. That is the package's position and
// not this accessor's: RFC 9420 forbids a repeated extension type, ValSem209 -- the validation
// rule the comments here used to hand it to -- is implemented nowhere, and LeafNode.Validate
// walks every entry and range checks every urmessage_leaf_keys body it finds, which is a
// stronger check than a lookup makes and is still not a refusal of the repeat: a leaf carrying
// two well formed entries passes it, deliberately, because a rule applied to every entry is a
// different rule from a refusal of the pair.
//
// So an accessor that answered one of two picks the group's wrap target by iteration order. That
// choice was not observable -- the walk was reversed and the whole suite passed -- and the two
// entries name two different devices, so half a group could wrap to one and half to the other
// with neither half able to tell. FindExtensionEntry refuses it for every caller rather than
// each accessor refusing it again, which is what stops a fourth accessor from being the one that
// does not, and that lookup's doc is where the argument for the position is written out. What is
// left here is naming the TYPE, which the lookup has only a uint16 for.
//
// Absence is a refusal rather than a nil result, because every v1 leaf carries one and a caller
// that treats "no wrap key" as a normal case is a caller that silently drops a member out of
// the epoch wrap -- which is not observable at the commit that did it, only at the first
// message that member cannot read.
//
// Both refusals carry ErrMalformedExtension, which is what the group lifecycle asks this
// question with, and the parse failure carries the codec's own ErrLeafKeysExtensionInvalid
// underneath it as well: the two name different repairs -- a leaf with no extension at all
// versus a leaf whose extension this profile cannot act on -- and joining them keeps both
// reachable through errors.Is from one result.
func LeafKeysOf(leaf *LeafNode) (*LeafKeysExtension, error) {
	if leaf == nil {
		return nil, fmt.Errorf("%w: nil leaf", ErrMalformedExtension)
	}
	entry, found, err := FindExtensionEntry(leaf.Extensions, ExtensionTypeUrmessageLeafKeys)
	if err != nil {
		// the lookup's refusal already carries ErrMalformedExtension and the two entry
		// positions; what it cannot carry is the human name of the type, so that is added here
		// and nothing else is. No digits: groupPolicyPositionsNamedBy reads every digit run of
		// the message as a position.
		return nil, fmt.Errorf("leaf carries urmessage_leaf_keys more than once: %w", err)
	}
	if !found {
		return nil, fmt.Errorf("%w: leaf carries no urmessage_leaf_keys extension", ErrMalformedExtension)
	}
	keys, err := ParseLeafKeysFrom(entry)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrMalformedExtension, err)
	}
	return keys, nil
}

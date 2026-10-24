//go:build !linux && !darwin

// Unsupported hosts cannot produce a weaker backup completeness claim.
package durablevolume

import "context"

// No pathname-only traversal substitutes for the Linux descriptor contract.
func (self *Owner) inventory(context.Context, *Inventory) error { return ErrUnsupported }

//go:build !linux && !darwin

// Other platforms have no qualified custody syscalls. Every primitive refuses;
// none substitutes an overwriting rename or an unconditional attribute write.
package durablesys

import (
	"errors"
	"syscall"
)

// Placeholder condition values; every write below refuses.
const AttributeCreate = 1
const AttributeReplace = 2

// No platform errno identifies an absent attribute here.
const ErrNoAttribute = syscall.Errno(0)

func RenameNoReplace(int, string, int, string) error { return errors.ErrUnsupported }

func RenameExchange(int, string, int, string) error { return errors.ErrUnsupported }

func SetAttribute(int, string, []byte, int) error { return errors.ErrUnsupported }

func GetAttribute(int, string, []byte) (int, error) { return 0, errors.ErrUnsupported }

func ListAttributes(int, []byte) (int, error) { return 0, errors.ErrUnsupported }

func AttributeUnsupported(err error) bool { return errors.Is(err, errors.ErrUnsupported) }

//go:build !linux && !darwin && !freebsd

// Unsupported physical custody leaves original attribution unavailable.
package connect

import (
	"errors"
	"os"
)

// Ordinary contract traffic does not depend on optional original retention.
func lockOriginalContractStore(*os.Root) (*os.File, error) {
	return nil, errors.New("original contract custody is unavailable on this platform")
}

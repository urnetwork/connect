//go:build !linux && !darwin && !freebsd

// Unsupported physical custody leaves original attribution unavailable.
package connect

import (
	"context"
	"errors"
	"os"
)

// Ordinary contract traffic does not depend on optional original retention.
func lockOriginalContractStore(*os.Root) (*os.File, error) {
	return nil, errors.New("original contract custody is unavailable on this platform")
}

func readOriginalContractStoreAttribute(*os.File) ([]byte, error) {
	return nil, errors.New("original contract prepared custody is unavailable on this platform")
}
func BuildFreshOriginalContractStoreCheckpoint(context.Context, *os.File, OriginalContractStoreScope) ([]byte, error) {
	return nil, errors.New("original contract prepared custody is unavailable on this platform")
}

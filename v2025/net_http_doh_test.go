package connect

import (
	"context"
	"fmt"
	"net/netip"
	"time"

	"testing"

	"github.com/go-playground/assert/v2"
)

func TestDohQuery(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	settings := DefaultDohSettings()

	testIp1, err := netip.ParseAddr("1.1.1.1")
	assert.Equal(t, err, nil)
	testIp2, err := netip.ParseAddr("10.10.10.10")
	assert.Equal(t, err, nil)

	for range 10 {
		ips := DohQuery(ctx, 4, "A", settings, "test1.bringyour.com")
		if len(ips) == 0 {
			// timeout, try again
			fmt.Printf("[doh]timeout. Will wait 1s and try again ...\n")
			select {
			case <-time.After(1 * time.Second):
				continue
			}
		}
		assert.Equal(t, ips, map[netip.Addr]bool{
			testIp1: true,
			testIp2: true,
		})
	}

}

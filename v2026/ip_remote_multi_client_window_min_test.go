package connect

import "testing"

// A user-selected network peer is a fixed single-destination window. When that
// sole destination entered a warning state — draining, ulimit, or a health
// warning — the minimum counted only unwarned clients, so `1 <= 0` was false
// and the connect screen reported "Connecting to providers" for the whole
// session while the tunnel was up.
//
// Physically observed on 2026-07-29 with the client connected to one network
// peer: `[grid]1->2(false) points=1 CONNECTING`, where the target of 2 is the
// fixed size of 1 plus the one warned client.
func TestWindowMinSatisfiedCountsWarnedFixedDestination(t *testing.T) {
	cases := []struct {
		name             string
		windowSizeMin    int
		clientCount      int
		warnedCount      int
		fixedDestination bool
		want             bool
	}{
		{
			name:             "fixed sole destination warned still satisfies",
			windowSizeMin:    1,
			clientCount:      0,
			warnedCount:      1,
			fixedDestination: true,
			want:             true,
		},
		{
			name:             "fixed sole destination healthy satisfies",
			windowSizeMin:    1,
			clientCount:      1,
			warnedCount:      0,
			fixedDestination: true,
			want:             true,
		},
		{
			name:             "fixed destination with no client at all is unsatisfied",
			windowSizeMin:    1,
			clientCount:      0,
			warnedCount:      0,
			fixedDestination: true,
			want:             false,
		},
		{
			name:             "fixed multi destination counts warned toward the minimum",
			windowSizeMin:    3,
			clientCount:      1,
			warnedCount:      2,
			fixedDestination: true,
			want:             true,
		},
		{
			name:             "fixed multi destination short of the minimum",
			windowSizeMin:    3,
			clientCount:      1,
			warnedCount:      1,
			fixedDestination: true,
			want:             false,
		},
		{
			// An expanding window can replace a warned destination, so a
			// warning there genuinely means the minimum is not met yet.
			name:             "expanding window does not count warned clients",
			windowSizeMin:    2,
			clientCount:      1,
			warnedCount:      4,
			fixedDestination: false,
			want:             false,
		},
		{
			name:             "expanding window satisfied by unwarned clients",
			windowSizeMin:    2,
			clientCount:      2,
			warnedCount:      4,
			fixedDestination: false,
			want:             true,
		},
	}

	for _, c := range cases {
		got := windowMinSatisfied(
			c.windowSizeMin,
			c.clientCount,
			c.warnedCount,
			c.fixedDestination,
		)
		if got != c.want {
			t.Errorf(
				"%s: windowMinSatisfied(min=%d, clients=%d, warned=%d, fixed=%t) = %t, want %t",
				c.name,
				c.windowSizeMin,
				c.clientCount,
				c.warnedCount,
				c.fixedDestination,
				got,
				c.want,
			)
		}
	}
}

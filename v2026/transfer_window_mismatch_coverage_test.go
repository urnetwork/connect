// The path sweep covers interactions without multiplying packet work by the
// full Cartesian product. Its coverage and work bound are executable contracts.
package connect

import (
	"slices"
	"testing"
	"time"
)

// Every pair of dimension values and each explicit boundary interaction must
// remain represented when the expensive real-worker matrix is edited.
func TestWindowPathWindowMismatchMatrixCoverage(t *testing.T) {
	dimensions := [5][]int64{
		{int64(kib(256)), int64(mib(2)), int64(mib(48))},
		{int64(kib(256)), int64(mib(2)), int64(mib(48))},
		{int64(300 * time.Microsecond), int64(100 * time.Millisecond), int64(400 * time.Millisecond)},
		{0, int64(10 * time.Millisecond)},
		{1, 8},
	}
	names := [5]string{"send window", "receive window", "rtt", "compression", "flows"}
	values := func(cell windowPathCell) [5]int64 {
		return [5]int64{int64(cell.SendWindow), int64(cell.ReceiveWindow), int64(cell.RoundTrip), int64(cell.Compression), int64(cell.Flows)}
	}
	rows := map[[5]int64]bool{}
	pairs := map[[4]int64]bool{}
	cells := windowMismatchMatrixCells()
	if len(cells) > 16 {
		t.Errorf("mismatch matrix has %d real-worker cells, want at most 16", len(cells))
	}
	for _, cell := range cells {
		row := values(cell)
		if rows[row] || cell.Rate != 125000000 {
			t.Fatalf("duplicate row or changed path rate: %+v", cell)
		}
		rows[row] = true
		for a, value := range row {
			if !slices.Contains(dimensions[a], value) {
				t.Fatalf("unexpected %s=%d", names[a], value)
			}
			for b := a + 1; b < len(row); b++ {
				pairs[[4]int64{int64(a), value, int64(b), row[b]}] = true
			}
		}
	}
	for a, choices := range dimensions {
		for b := a + 1; b < len(dimensions); b++ {
			for _, x := range choices {
				for _, y := range dimensions[b] {
					if !pairs[[4]int64{int64(a), x, int64(b), y}] {
						t.Errorf("missing %s=%d with %s=%d", names[a], x, names[b], y)
					}
				}
			}
		}
	}
	// Keep the largest residence/flight, tight limits in both directions,
	// short-path startup, and the compressed large-window throughput corner.
	for _, corner := range []windowPathCell{
		{SendWindow: kib(256), ReceiveWindow: kib(256), RoundTrip: 300 * time.Microsecond, Flows: 1},
		{SendWindow: mib(48), ReceiveWindow: mib(48), RoundTrip: 400 * time.Millisecond, Compression: 10 * time.Millisecond, Flows: 8},
		{SendWindow: mib(48), ReceiveWindow: kib(256), RoundTrip: 400 * time.Millisecond, Compression: 10 * time.Millisecond, Flows: 8},
		{SendWindow: kib(256), ReceiveWindow: mib(48), RoundTrip: 400 * time.Millisecond, Compression: 10 * time.Millisecond, Flows: 8},
		{SendWindow: mib(48), ReceiveWindow: kib(256), RoundTrip: 300 * time.Microsecond, Flows: 1},
		{SendWindow: kib(256), ReceiveWindow: mib(48), RoundTrip: 300 * time.Microsecond, Flows: 1},
		{SendWindow: mib(48), ReceiveWindow: mib(48), RoundTrip: 300 * time.Microsecond, Compression: 10 * time.Millisecond, Flows: 8},
		{SendWindow: kib(256), ReceiveWindow: kib(256), RoundTrip: 400 * time.Millisecond, Flows: 8},
	} {
		if !rows[values(corner)] {
			t.Errorf("missing boundary cell: %+v", corner)
		}
	}
}

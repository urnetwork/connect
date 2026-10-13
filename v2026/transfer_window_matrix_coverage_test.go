// Coverage contracts keep bounded worker matrices honest without measuring a
// second Cartesian product. Every retained row still runs both original arms.
package connect

import (
	"testing"
	"time"
)

// Enumerate required interactions independently of the matrix selection rule.
func assertWindowMatrixCoverage(t *testing.T, rows [][4]int, domains [][]int, strength int) {
	t.Helper()
	checked := 0
	var axes, values []int
	var checkValues func(int)
	checkValues = func(depth int) {
		if depth < len(axes) {
			for _, value := range domains[axes[depth]] {
				values = append(values, value)
				checkValues(depth + 1)
				values = values[:len(values)-1]
			}
			return
		}
		checked++
		for _, row := range rows {
			matches := true
			for index, axis := range axes {
				matches = matches && row[axis] == values[index]
			}
			if matches {
				return
			}
		}
		t.Errorf("missing %d-way coverage: axes=%v values=%v", strength, axes, values)
	}
	var chooseAxes func(int)
	chooseAxes = func(next int) {
		if len(axes) == strength {
			checkValues(0)
			return
		}
		for axis := next; axis < len(domains); axis++ {
			axes = append(axes, axis)
			chooseAxes(axis + 1)
			axes = axes[:len(axes)-1]
		}
	}
	chooseAxes(0)
	t.Logf("%d rows cover all %d required %d-way interactions", len(rows), checked, strength)
}

// Each feedback scale meets both flow counts and compression states; both
// boundary round trips retain all four combinations and their deep-flight work.
func TestWindowPathDeterministicMatrixCoverage(t *testing.T) {
	var rows [][4]int
	seen := map[[4]int]bool{}
	for _, cell := range windowDeterministicPerformanceCells(windowDeterministicRoundTrips(), false) {
		row := [4]int{int(cell.RoundTrip), cell.Flows, int(cell.Compression)}
		if seen[row] || cell.Payload != 1280 || cell.Budget != mib(48) || cell.Rate != 125000000 || cell.RoundRobinOffer {
			t.Fatalf("duplicate or changed deterministic workload: %+v", cell)
		}
		seen[row] = true
		rows = append(rows, row)
	}
	// Pin the required domain independently: removing a configured delay
	// must not silently shrink the coverage assertion with it.
	roundTrips := []int{int(300 * time.Microsecond), int(time.Millisecond), int(2 * time.Millisecond), int(5 * time.Millisecond), int(10 * time.Millisecond), int(25 * time.Millisecond), int(100 * time.Millisecond), int(200 * time.Millisecond), int(400 * time.Millisecond)}
	assertWindowMatrixCoverage(t, rows, [][]int{roundTrips, {1, 8}, {0, int(10 * time.Millisecond)}}, 2)
	for _, roundTrip := range []time.Duration{300 * time.Microsecond, 400 * time.Millisecond} {
		for _, flows := range []int{1, 8} {
			for _, compression := range []time.Duration{0, 10 * time.Millisecond} {
				if !seen[[4]int{int(roundTrip), flows, int(compression)}] {
					t.Fatalf("missing boundary: rtt=%s flows=%d compression=%s", roundTrip, flows, compression)
				}
			}
		}
	}
	// The environment override is a diagnostic, not a reduced default sweep.
	if got := len(windowDeterministicPerformanceCells([]time.Duration{37 * time.Millisecond}, true)); got != 4 {
		t.Fatalf("diagnostic round trip lost its full grid: %d cells", got)
	}
}

// Every constructor meets each direction, feedback delay and sharing level in
// all three-way combinations. Keep explicit h1 senders and startup binders too.
func TestWindowPathSdkMatrixCoverage(t *testing.T) {
	profiles, digest := windowSdkProfiles(t)
	profileIndices := map[*windowPathEndpointProfile]int{}
	var profileDomain []int
	for index := range profiles {
		profileIndices[&profiles[index]] = index
		profileDomain = append(profileDomain, index)
	}
	var rows [][4]int
	physicalRows := map[[4]int]bool{}
	logicalRows := map[[4]int]bool{}
	for _, cell := range windowSdkProfileCells(profiles, digest) {
		sender, senderKnown := profileIndices[cell.SenderProfile]
		receiver, receiverKnown := profileIndices[cell.ReceiverProfile]
		physical := [4]int{sender, receiver, int(cell.RoundTrip), cell.Flows}
		if !senderKnown || !receiverKnown || (sender != 1 && receiver != 1) || physicalRows[physical] ||
			cell.ProfileFixtureSha256 != digest || cell.Payload != 1280 || cell.Rate != 125000000 ||
			!cell.RoundRobinOffer || cell.Bidirectional || cell.Upload ||
			cell.SenderProfile.AckCompressionNs != 10*time.Millisecond || cell.ReceiverProfile.AckCompressionNs != 10*time.Millisecond {
			t.Fatalf("duplicate or changed SDK workload: %+v", cell)
		}
		physicalRows[physical] = true
		for direction := range 2 {
			profile := receiver
			if direction == 1 {
				profile = sender
			}
			if (direction == 0 && sender != 1) || (direction == 1 && receiver != 1) {
				continue
			}
			// Server-to-itself is one physical row with both direction labels.
			row := [4]int{profile, direction, int(cell.RoundTrip), cell.Flows}
			logicalRows[row] = true
			rows = append(rows, row)
		}
	}
	roundTrips := []int{int(300 * time.Microsecond), int(100 * time.Millisecond), int(400 * time.Millisecond)}
	assertWindowMatrixCoverage(t, rows, [][]int{profileDomain, {0, 1}, roundTrips, {1, 8}}, 3)
	for index, profile := range profiles {
		startup := profile.MobilePolicy && (profile.Name == "sdk-provider-default" || !profile.Providing)
		for _, roundTrip := range roundTrips {
			for _, flows := range []int{1, 8} {
				need := profile.ExplicitH1 || (startup && roundTrip != int(100*time.Millisecond)) ||
					(profile.MobilePolicy && profile.Name == "sdk-device-default" && !profile.Providing && flows == 1)
				if need && !logicalRows[[4]int{index, 1, roundTrip, flows}] {
					t.Fatalf("missing SDK sender boundary: profile=%d rtt=%s flows=%d", index, time.Duration(roundTrip), flows)
				}
			}
		}
	}
}

// Preserve every three-way service interaction and the sixteen full extreme
// corners, including the slow uncompressed ceiling's genuine timeout recovery.
func TestWindowPathServiceMatrixCoverage(t *testing.T) {
	var rows [][4]int
	seen := map[[4]int]bool{}
	for _, cell := range windowServicePerformanceCells() {
		row := [4]int{int(cell.Rate), int(cell.RoundTrip), cell.Flows, int(cell.Compression)}
		warmup := max(300*time.Millisecond+5*cell.RoundTrip, time.Duration(2*int64(mib(2))*int64(time.Second)/int64(cell.Rate))+5*cell.RoundTrip)
		if seen[row] || cell.Payload != 1280 || cell.Budget != mib(48) || !cell.RoundRobinOffer || cell.Warmup != warmup {
			t.Fatalf("duplicate or changed service workload: %+v", cell)
		}
		seen[row] = true
		rows = append(rows, row)
	}
	assertWindowMatrixCoverage(t, rows, [][]int{{125000, 1250000, 12500000}, {int(300 * time.Microsecond), int(100 * time.Millisecond), int(400 * time.Millisecond)}, {1, 8}, {0, int(10 * time.Millisecond), int(50 * time.Millisecond)}}, 3)
	for _, rate := range []int{125000, 12500000} {
		for _, roundTrip := range []int{int(300 * time.Microsecond), int(400 * time.Millisecond)} {
			for _, flows := range []int{1, 8} {
				for _, compression := range []int{0, int(50 * time.Millisecond)} {
					if !seen[[4]int{rate, roundTrip, flows, compression}] {
						t.Fatalf("missing service corner: rate=%d rtt=%s flows=%d compression=%s", rate, time.Duration(roundTrip), flows, time.Duration(compression))
					}
				}
			}
		}
	}
}

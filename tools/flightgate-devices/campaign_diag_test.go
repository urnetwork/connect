package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseDiagIgnoresOtherLogcatStreamsBetweenFragments(t *testing.T) {
	// Both observed phone failures split a quoted field name. Appending an
	// unrelated Android log then produces valid JSON with the wrong key;
	// merely checking that JSON decoding succeeds does not detect corruption.
	for _, field := range []string{
		"device_transport_budget_pair_slots",
		"transport_budget_active_handoff_h1_bytes",
	} {
		t.Run(field, func(t *testing.T) {
			first, last := field[:12], field[12:]
			log := `1789700329.154 6442 6510 I GoLog   : I0918 02:58:49 [flightgate] {"part":"memory","unix_millis":123456,"` + first + "\n" +
				"1789700329.154 6442 6442 I View    : setRequestedFrameRate frameRate=NaN\n" +
				"1789700329.154 4157 4617 I bluetooth: Dumping btsnooz log data\n" +
				"1789700329.154 9999 9999 I GoLog   : unrelated process continuation\n" +
				`1789700329.155 6442 6511 I GoLog   : ` + last + `":0}` + "\n"
			path := filepath.Join(t.TempDir(), "logcat")
			if err := os.WriteFile(path, []byte(log), 0o600); err != nil {
				t.Fatal(err)
			}
			samples, err := parseDiag(path)
			if err != nil || len(samples) != 1 {
				t.Fatalf("samples = %+v, %v", samples, err)
			}
			if value, present := samples[0].Payload[field]; !present || value != float64(0) {
				t.Fatalf("field corrupted across logcat fragments: %+v", samples[0].Payload)
			}
		})
	}
}

func TestParseDiagFreshGoLogRecordEndsPendingContinuation(t *testing.T) {
	log := strings.Join([]string{
		`1789700329.154 6442 6510 I GoLog   : I0918 02:58:49 [flightgate] {"part":"memory","unix_millis":123456,"missing`,
		`1789700329.155 6442 6510 I GoLog   : I0918 02:58:49 unrelated record`,
		`1789700329.156 6442 6510 I GoLog   : _field":0}`,
		`1789700329.157 6442 6510 I GoLog   : I0918 02:58:49 [flightgate] {"part":"memory","unix_millis":123457,"go_total_bytes":20971520}`,
	}, "\n")
	path := filepath.Join(t.TempDir(), "logcat")
	if err := os.WriteFile(path, []byte(log), 0o600); err != nil {
		t.Fatal(err)
	}
	samples, err := parseDiag(path)
	if err != nil || len(samples) != 1 || samples[0].Millis != 123457 {
		t.Fatalf("a different glog record completed a truncated diagnostic: %+v, %v", samples, err)
	}
}

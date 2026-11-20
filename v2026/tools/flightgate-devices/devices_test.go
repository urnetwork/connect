// The allowlisted phones come from tests.yml, and connect writes no serial.
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Synthetic stand-ins for the phones that tests.yml lists.
const (
	testDeviceASerial = "FLIGHTDEVA01"
	testDeviceBSerial = "FLIGHTDEVB02"
)

// Pins the synthetic phones for every test, so no test reads a real tests.yml.
func TestMain(m *testing.M) {
	pinTestAllowedDevices()
	os.Exit(m.Run())
}

// Sets the allowlist to the synthetic phones.
func pinTestAllowedDevices() {
	allowedDevices = map[string]string{testDeviceASerial: "device-a", testDeviceBSerial: "device-b"}
}

// Returns a workspace root whose tests/read-tests-config.sh logs each request
// and prints output, failing instead when output is "<fail>".
func allowedDevicesReaderRoot(t *testing.T, output string) (string, string) {
	t.Helper()
	root := t.TempDir()
	logPath := filepath.Join(root, "reader.log")
	script := "#!/bin/sh\nprintf '%s|%s\\n' \"$URNETWORK_ROOT\" \"$*\" >>" + logPath + "\n"
	if output == "<fail>" {
		script += "exit 1\n"
	} else {
		script += "printf '%s' '" + output + "'\n"
	}
	if err := os.MkdirAll(filepath.Join(root, "tests"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "tests", "read-tests-config.sh"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return root, logPath
}

// The allowlist is android.performance_device_serials from the workspace's
// tests.yml, device-a first, and the memsteady defaults follow it.
func TestAllowedDevicesComeFromTestsYml(t *testing.T) {
	t.Cleanup(pinTestAllowedDevices)
	root, logPath := allowedDevicesReaderRoot(t, "READPHONEA1 READPHONEB2\n")
	if err := loadAllowedDevices(root); err != nil {
		t.Fatal(err)
	}
	if len(allowedDevices) != 2 || allowedDevices["READPHONEA1"] != "device-a" || allowedDevices["READPHONEB2"] != "device-b" {
		t.Fatalf("allowedDevices = %v", allowedDevices)
	}
	if serialForRole("device-a") != "READPHONEA1" || serialForRole("device-b") != "READPHONEB2" || serialForRole("device-c") != "" {
		t.Fatalf("serialForRole disagrees with %v", allowedDevices)
	}
	requests, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(requests) != root+"|get android.performance_device_serials\n" {
		t.Fatalf("reader requests = %q", requests)
	}
}

// An unreadable, missing, placeholder or malformed value is refused with the
// key named, and the allowlist keeps what it had.
func TestAllowedDevicesRefuseUnusableValues(t *testing.T) {
	t.Cleanup(pinTestAllowedDevices)
	for _, output := range []string{"<fail>", "", "   \n", "ONLYONE", "READPHONEA1 READPHONEB2 THIRDPHONE3", "READPHONEA1 READPHONEA1",
		"READPHONEA1 bad/serial", "REPLACE_ME READPHONEB2"} {
		pinTestAllowedDevices()
		root, _ := allowedDevicesReaderRoot(t, output)
		err := loadAllowedDevices(root)
		if err == nil || !strings.Contains(err.Error(), performanceDeviceSerialsKey) {
			t.Errorf("reader output %q: err = %v, want a refusal naming %s", output, err, performanceDeviceSerialsKey)
		}
		if len(allowedDevices) != 2 || allowedDevices[testDeviceASerial] != "device-a" || allowedDevices[testDeviceBSerial] != "device-b" {
			t.Errorf("reader output %q replaced the allowlist: %v", output, allowedDevices)
		}
	}
}

// No tracked file in connect writes a reserved phone's serial. The two serials
// connect used to carry are known here only by their SHA-256 digests.
func TestNoReservedPhoneSerialInConnect(t *testing.T) {
	retiredSerialDigests := map[string]bool{
		"8fada973387220c809a4d3f9ce787e63c968c765eab8d0e8e4c98f661b7408f6": true,
		"71c6850bba24293be4428a4cbd5344ee1f67b012e946a8897967ecff0286e88b": true,
	}
	top, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		t.Fatalf("the serial check needs the connect checkout: %v", err)
	}
	tokens, err := exec.Command("git", "-C", strings.TrimSpace(string(top)), "grep", "-I", "-h", "-o", "-w", "-E", "[0-9A-Z]{8,20}").Output()
	if err != nil {
		t.Fatal(err)
	}
	hits := 0
	for _, candidate := range strings.Split(string(tokens), "\n") {
		digest := sha256.Sum256([]byte(candidate))
		if retiredSerialDigests[hex.EncodeToString(digest[:])] {
			hits++
		}
	}
	if hits != 0 {
		t.Fatalf("connect writes a reserved performance phone's serial %d times; name the phone model or android.performance_device_serials", hits)
	}
}

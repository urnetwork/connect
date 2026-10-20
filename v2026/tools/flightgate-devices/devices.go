// The two allowlisted phones' adb serials, read from tests.yml.
package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"
)

// The tests.yml path listing the reserved performance phones' adb serials,
// separated by spaces: device-a (the Pixel 8 Pro) first, then device-b (the
// Galaxy S24 Ultra). The android harness and the PERF runner read the same key.
const performanceDeviceSerialsKey = "android.performance_device_serials"

var performanceDeviceSerialPattern = regexp.MustCompile(`^[A-Za-z0-9._:-]+$`)

// The workspace whose tests/read-tests-config.sh reads tests.yml: URNETWORK_ROOT
// when set, else the checkout this source sits in, as build-item assumes.
func devicesWorkspaceRoot() string {
	if root := os.Getenv("URNETWORK_ROOT"); root != "" {
		return root
	}
	_, source, _, _ := runtime.Caller(0)
	return filepath.Clean(filepath.Join(filepath.Dir(source), "../../.."))
}

// Fills allowedDevices from tests.yml through the workspace's canonical config
// reader (its default is vault/main/tests.yml; UR_ACCEPT_VAULT overrides). Every
// device command depends on the allowlist, so an unreadable, missing,
// placeholder or malformed value is an error naming the key and leaves the
// allowlist unchanged. Called once, before any device work.
func loadAllowedDevices(root string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	command := exec.CommandContext(ctx, filepath.Join(root, "tests", "read-tests-config.sh"), "get", performanceDeviceSerialsKey)
	command.Env = append(os.Environ(), "URNETWORK_ROOT="+root)
	output, err := command.Output()
	if err != nil {
		return fmt.Errorf("could not read %s from tests.yml: %w", performanceDeviceSerialsKey, err)
	}
	serials := strings.Fields(string(output))
	valid := len(serials) == 2 && serials[0] != serials[1]
	for _, serial := range serials {
		valid = valid && performanceDeviceSerialPattern.MatchString(serial) && !strings.HasPrefix(serial, "REPLACE_ME")
	}
	if !valid {
		return fmt.Errorf("set %s in tests.yml to the device-a (Pixel 8 Pro) adb serial, then the device-b (Galaxy S24 Ultra) serial, separated by a space", performanceDeviceSerialsKey)
	}
	allowedDevices = map[string]string{serials[0]: "device-a", serials[1]: "device-b"}
	return nil
}

// The allowlisted serial for a role, or "" when none is loaded.
func serialForRole(role string) string {
	for serial, candidate := range allowedDevices {
		if candidate == role {
			return serial
		}
	}
	return ""
}

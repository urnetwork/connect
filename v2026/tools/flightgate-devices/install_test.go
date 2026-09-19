package main

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

type installTestArtifact struct {
	apk, manifestPath string
	manifest          acceptanceManifest
}

func newInstallTestArtifact(t *testing.T) installTestArtifact {
	t.Helper()
	dir := t.TempDir()
	artifact := installTestArtifact{
		apk:          filepath.Join(dir, "audit-debug.apk"),
		manifestPath: filepath.Join(dir, "build-manifest.json"),
		manifest: acceptanceManifest{
			BuildID: "fixture-build", AcceptanceEligible: true,
			MemoryProfile: iosMemoryAuditProfile, DeviceMemoryTargetBytes: iosDeviceTargetBytes,
			ProcessMemoryLimitBytes: iosProcessSoftLimitBytes, ProcessTransportBytes: iosCarrierRootBytes,
			ProcessTransportCount: iosCarrierRootMaxCount,
		},
	}
	if err := os.WriteFile(artifact.apk, []byte("fixture acceptance APK"), 0o600); err != nil {
		t.Fatal(err)
	}
	var err error
	artifact.manifest.APKSHA256, err = fileSHA256(artifact.apk)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeJson(artifact.manifestPath, artifact.manifest); err != nil {
		t.Fatal(err)
	}
	return artifact
}

func TestInstallDowngradeRequiresVerifiedAcceptanceUpdate(t *testing.T) {
	for _, tc := range []struct {
		name             string
		update, manifest bool
	}{
		{"ordinary replacement", false, false},
		{"ordinary update", true, false},
		{"verified acceptance update", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			artifact := newInstallTestArtifact(t)
			args := []string{"--apk", artifact.apk}
			if tc.update {
				args = append(args, "--update")
			}
			if tc.manifest {
				args = append(args, "--build-manifest", artifact.manifestPath)
			}
			serials := allowedDeviceSerials()
			args = append(args, serials...)
			var calls [][]string
			if err := installWithADB(args, func(serial string, args ...string) (string, error) {
				calls = append(calls, append([]string{serial}, args...))
				return "Success", nil
			}); err != nil {
				t.Fatal(err)
			}
			var want [][]string
			for _, serial := range serials {
				status := []string{serial, "shell", "dumpsys package " + appPackage + " | grep -m1 versionName"}
				want = append(want, status)
				if !tc.update {
					want = append(want, []string{serial, "uninstall", appPackage})
				}
				command := []string{serial, "install", "-r", "-g"}
				if tc.manifest {
					command = append(command, "-d")
				}
				want = append(want, append(command, artifact.apk), status)
			}
			if !reflect.DeepEqual(calls, want) {
				t.Fatalf("ADB calls = %q, want %q", calls, want)
			}
		})
	}
}

func TestInstallRefusesUnverifiedAcceptanceBeforeADB(t *testing.T) {
	changeManifest := func(change func(*acceptanceManifest)) func(*testing.T, *installTestArtifact) {
		return func(t *testing.T, artifact *installTestArtifact) {
			t.Helper()
			change(&artifact.manifest)
			if err := writeJson(artifact.manifestPath, artifact.manifest); err != nil {
				t.Fatal(err)
			}
		}
	}
	for _, tc := range []struct {
		name, want string
		noUpdate   bool
		change     func(*testing.T, *installTestArtifact)
	}{
		{"requires explicit update", "requires --update", true, nil},
		{"missing manifest", "no such file", false, func(_ *testing.T, a *installTestArtifact) { a.manifestPath += ".missing" }},
		{"missing APK", "read acceptance APK", false, func(_ *testing.T, a *installTestArtifact) { a.apk += ".missing" }},
		{"malformed manifest", "unexpected end", false, func(t *testing.T, a *installTestArtifact) {
			if err := os.WriteFile(a.manifestPath, []byte("{"), 0o600); err != nil {
				t.Fatal(err)
			}
		}},
		{"wrong APK hash", "does not match", false, changeManifest(func(m *acceptanceManifest) { m.APKSHA256 = strings.Repeat("a", 64) })},
		{"incomplete artifact", "completed acceptance artifact", false, changeManifest(func(m *acceptanceManifest) { m.APKSHA256 = "" })},
		{"missing build ID", "completed acceptance artifact", false, changeManifest(func(m *acceptanceManifest) { m.BuildID = "" })},
		{"diagnostic artifact", "completed acceptance artifact", false, changeManifest(func(m *acceptanceManifest) { m.MemProfileRate = 65536 })},
		{"ineligible artifact", "completed acceptance artifact", false, changeManifest(func(m *acceptanceManifest) { m.AcceptanceEligible = false })},
		{"wrong profile", "explicit iOS memory audit profile", false, changeManifest(func(m *acceptanceManifest) { m.MemoryProfile = "android" })},
		{"wrong device target", "explicit iOS memory audit profile", false, changeManifest(func(m *acceptanceManifest) { m.DeviceMemoryTargetBytes += 1 })},
	} {
		t.Run(tc.name, func(t *testing.T) {
			artifact := newInstallTestArtifact(t)
			if tc.change != nil {
				tc.change(t, &artifact)
			}
			args := []string{"--apk", artifact.apk, "--build-manifest", artifact.manifestPath}
			if !tc.noUpdate {
				args = append(args, "--update")
			}
			calls := 0
			err := installWithADB(args, func(string, ...string) (string, error) {
				calls++
				return "", errors.New("unexpected device command")
			})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want %q", err, tc.want)
			}
			if calls != 0 {
				t.Fatalf("unverified artifact issued %d device commands", calls)
			}
		})
	}
}

func TestInstallRevalidatesAcceptanceArtifactAtUpdate(t *testing.T) {
	artifact := newInstallTestArtifact(t)
	if _, err := verifyAcceptanceArtifact(artifact.apk, artifact.manifestPath); err != nil {
		t.Fatal(err)
	}
	// Series preflight can be well before installation. It is not authority to
	// downgrade using different bytes which have since replaced the file.
	if err := os.WriteFile(artifact.apk, []byte("changed after preflight"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := installWithADB([]string{"--update", "--apk", artifact.apk, "--build-manifest", artifact.manifestPath},
		func(string, ...string) (string, error) {
			t.Error("changed artifact reached ADB")
			return "", nil
		})
	if err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("changed artifact error = %v", err)
	}
}

func TestInstallVerifiedUpdateDoesNotUninstallOnFailure(t *testing.T) {
	artifact := newInstallTestArtifact(t)
	installCalls := 0
	err := installWithADB([]string{"--update", "--apk", artifact.apk, "--build-manifest", artifact.manifestPath, allowedDeviceSerials()[0]},
		func(_ string, args ...string) (string, error) {
			switch args[0] {
			case "shell":
				return "versionName=newer", nil
			case "install":
				installCalls++
				return "INSTALL_FAILED_UPDATE_INCOMPATIBLE", errors.New("exit status 1")
			default:
				t.Errorf("unexpected fallback command: %q", args)
				return "", nil
			}
		})
	if err == nil || !strings.Contains(err.Error(), "INSTALL_FAILED_UPDATE_INCOMPATIBLE") || installCalls != 1 {
		t.Fatalf("install attempts = %d, error = %v", installCalls, err)
	}
}

//go:build fingerprint_capture

// capture_local.go — the local-browser half of Layer B (fingerprint/README.md).
//
// Docker is not available everywhere, and the owner's note asks for a real
// browser's hello (EXTENDER-UPDATE2 §3.4). This captures the TLS ClientHello of
// a browser already installed on the machine — Chrome or another Chromium
// browser — run headless against the shared local endpoint, no Docker and no
// root. It is still Layer B: gated out of `go test` by the fingerprint_capture
// tag, run once by a maintainer to refresh the committed real-browser golden
// and its version metadata. The routine Layer-A gate then reads that committed
// golden offline.
//
// It writes two files per capture: the version-stamped golden
// chrome-<version>-clienthello.bin (the raw ClientHello handshake message) and
// a sidecar chrome-<version>-clienthello.meta.json with the provenance, so the
// fixture carries its own version metadata.

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"

	"github.com/urnetwork/connect/v2026/fingerprint"
)

// GoldenMeta is the sidecar provenance written beside a captured golden, so the
// fixture records which browser and version produced it and when
// (EXTENDER-UPDATE2 §3.4 "versioned").
type GoldenMeta struct {
	ChromeVersion string `json:"chrome_version"`
	CaptureDate   string `json:"capture_date"`
	Source        string `json:"source"`
	BrowserPath   string `json:"browser_path"`
	ServerName    string `json:"server_name"`
	RecordCount   int    `json:"record_count"`
	MessageBytes  int    `json:"message_bytes"`
	Synthetic     bool   `json:"synthetic"`
}

// The browsers to try when -local-browser is not an explicit path, by platform.
// Each is a real Chromium browser whose hello is Chrome-family.
func defaultLocalBrowserPaths() []string {
	switch runtime.GOOS {
	case "darwin":
		return []string{
			"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
			"/Applications/Chromium.app/Contents/MacOS/Chromium",
			"/Applications/Brave Browser.app/Contents/MacOS/Brave Browser",
			"/Applications/Microsoft Edge.app/Contents/MacOS/Microsoft Edge",
		}
	case "linux":
		return []string{
			"/usr/bin/google-chrome", "/usr/bin/google-chrome-stable",
			"/usr/bin/chromium", "/usr/bin/chromium-browser",
			"/usr/bin/brave-browser", "/usr/bin/microsoft-edge",
		}
	default:
		return nil
	}
}

// resolveLocalBrowser returns the explicit path when given, else the first
// default path that exists.
func resolveLocalBrowser(explicit string) (string, error) {
	if explicit != "" {
		if _, err := os.Stat(explicit); err != nil {
			return "", fmt.Errorf("browser %q: %w", explicit, err)
		}
		return explicit, nil
	}
	for _, path := range defaultLocalBrowserPaths() {
		if _, err := os.Stat(path); err == nil {
			return path, nil
		}
	}
	return "", fmt.Errorf("no local Chromium browser found; pass -local-browser <path>")
}

var browserVersionPattern = regexp.MustCompile(`([0-9]+(?:\.[0-9]+){1,3})`)

// localBrowserVersion reads the browser's own version ("Google Chrome 156.0..."
// from --version), falling back to the override when the browser does not print
// one.
func localBrowserVersion(browserPath string, override string) (string, error) {
	if override != "" {
		return override, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, browserPath, "--version").Output()
	if err != nil {
		return "", fmt.Errorf("read browser version (pass -chrome-version): %w", err)
	}
	if match := browserVersionPattern.FindString(string(out)); match != "" {
		// the major version alone stamps the golden, as the Docker path does
		return strings.SplitN(match, ".", 2)[0], nil
	}
	return "", fmt.Errorf("could not parse browser version from %q (pass -chrome-version)", strings.TrimSpace(string(out)))
}

// captureLocalBrowserTlsClientHello runs a locally installed browser headless
// against the tcp endpoint, captures its ClientHello, writes the version-stamped
// golden and its metadata sidecar, and diffs the capture against the committed
// synthetic golden so a maintainer sees where the parrot trails real Chrome.
func captureLocalBrowserTlsClientHello(browserPathFlag string, chromeVersion string, goldenDir string, timeout time.Duration) error {
	browserPath, err := resolveLocalBrowser(browserPathFlag)
	if err != nil {
		return err
	}
	version, err := localBrowserVersion(browserPath, chromeVersion)
	if err != nil {
		return err
	}

	endpoint, err := fingerprint.NewEndpoint(fingerprint.EndpointOptions{BindHost: "127.0.0.1"})
	if err != nil {
		return err
	}
	defer endpoint.Close()
	url := fmt.Sprintf("https://%s:%d/", fingerprint.ServerName, endpoint.Port())

	fmt.Printf("endpoint on :%d, running %s (%s) against %s\n", endpoint.Port(), browserPath, version, url)
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	if err := runLocalBrowser(ctx, browserPath, endpoint.Port(), url); err != nil {
		return fmt.Errorf("run local browser: %w", err)
	}

	captures := endpoint.CapturedClientHellos()
	if len(captures) == 0 {
		return fmt.Errorf("the browser connected but no client hello was captured")
	}
	capture := captures[0]
	if _, err := fingerprint.ParseClientHello(capture.Message); err != nil {
		return fmt.Errorf("the captured hello did not parse: %w", err)
	}

	base := fmt.Sprintf("chrome-%s-clienthello", version)
	goldenPath := filepath.Join(goldenDir, base+".bin")
	if err := fingerprint.WriteGoldenFile(goldenPath, capture.Message); err != nil {
		return err
	}
	meta := GoldenMeta{
		ChromeVersion: version,
		CaptureDate:   time.Now().UTC().Format("2006-01-02"),
		Source:        fmt.Sprintf("real %s, captured at the local fingerprint endpoint (no Docker)", filepath.Base(browserPath)),
		BrowserPath:   browserPath,
		ServerName:    fingerprint.ServerName,
		RecordCount:   capture.RecordCount,
		MessageBytes:  len(capture.Message),
		Synthetic:     false,
	}
	metaBytes, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return err
	}
	metaPath := filepath.Join(goldenDir, base+".meta.json")
	if err := os.WriteFile(metaPath, append(metaBytes, '\n'), 0o644); err != nil {
		return fmt.Errorf("write golden metadata %s: %w", metaPath, err)
	}
	fmt.Printf("wrote real golden %s (%d bytes, %d records) and %s\n", goldenPath, len(capture.Message), capture.RecordCount, metaPath)

	// diff the real capture against the committed synthetic golden
	golden, err := fingerprint.LoadGolden(fingerprint.GoldenChrome133Synthetic)
	if err != nil {
		return err
	}
	got, err := fingerprint.ParseClientHello(capture.Message)
	if err != nil {
		return err
	}
	got.RecordCount = capture.RecordCount
	opts := fingerprint.DiffOptions{
		ExpectedServerName:        fingerprint.ServerName,
		ExpectedAlpnProtocols:     got.AlpnProtocols,
		ExpectApplicationSettings: len(got.AlpsProtocols) != 0,
	}
	fmt.Println(fingerprint.FormatDrift(fingerprint.GoldenChrome133Synthetic, fingerprint.Diff(golden.Fingerprint, got, opts)))
	return nil
}

// runLocalBrowser launches the browser headless against url, mapping the
// endpoint name to loopback and trusting the endpoint's cert. It opens a throw
// away profile so a running browser of the same kind is untouched, and exits on
// its own after loading; the capture happens at the endpoint during the
// handshake regardless of the page result.
func runLocalBrowser(ctx context.Context, browserPath string, port int, url string) error {
	profileDir, err := os.MkdirTemp("", "fingerprint-capture-profile")
	if err != nil {
		return err
	}
	defer os.RemoveAll(profileDir)
	args := []string{
		"--headless=new",
		"--no-sandbox",
		"--disable-gpu",
		"--no-first-run",
		"--no-default-browser-check",
		"--disable-extensions",
		"--user-data-dir=" + profileDir,
		"--ignore-certificate-errors",
		fmt.Sprintf("--host-resolver-rules=MAP %s 127.0.0.1", fingerprint.ServerName),
		url,
	}
	cmd := exec.CommandContext(ctx, browserPath, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	_ = cmd.Start()
	// the browser may stay open; the hello is captured during its handshake, so
	// wait only briefly then stop it.
	done := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(done)
	}()
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	select {
	case <-ctx.Done():
	case <-done:
	case <-timer.C:
	}
	if cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
	return nil
}

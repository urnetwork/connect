//go:build fingerprint_capture

// Command capture is Layer B of the fingerprint-drift harness: it refreshes the
// committed goldens from real Chrome and diffs the current Chrome against them,
// printing exactly which fields drifted so the owner knows when to bump the
// uTLS parrot and the profiles.
//
// It is gated out of the normal `go test` by the fingerprint_capture build tag
// (../README.md): it needs Docker, and -syn needs root. Build and run it with
//
//	go run -tags fingerprint_capture ./fingerprint/capture [flags]
//
// Modes:
//
//	-synthetic   regenerate the committed SYNTHETIC golden from the uTLS
//	             HelloChrome_133 profile (no Docker); reproduces what this
//	             package ships.
//	(default)    run headless Docker Chrome against the shared endpoint, capture
//	             its TLS ClientHello (and, with -quic, its QUIC Initial) at the
//	             endpoint, write the version-stamped golden, and diff the
//	             capture against the committed golden.
//
// The TCP SYN / IP TTL (JA4T) layer is NOT captured here: its ground truth is
// the OS kernel, Docker-Chrome yields only the host-Linux SYN, and it needs
// root. capture.sh wraps this tool with the tcpdump step for that layer.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/urnetwork/connect/fingerprint"
)

func main() {
	synthetic := flag.Bool("synthetic", false, "regenerate the synthetic golden from uTLS (no Docker)")
	chromeImage := flag.String("chrome-image", "chromedp/headless-shell:stable", "pinned headless Chrome Docker image")
	chromeVersion := flag.String("chrome-version", "", "the Chrome version to stamp the golden with (e.g. 141); required outside -synthetic")
	goldenDir := flag.String("golden-dir", "fingerprint/testdata/fingerprints", "directory to write goldens into")
	captureQuic := flag.Bool("quic", false, "also capture the QUIC Initial (forces Chrome onto h3)")
	timeout := flag.Duration("timeout", 60*time.Second, "how long to wait for Chrome to connect")
	flag.Parse()

	if err := run(*synthetic, *chromeImage, *chromeVersion, *goldenDir, *captureQuic, *timeout); err != nil {
		fmt.Fprintf(os.Stderr, "capture: %s\n", err)
		os.Exit(1)
	}
}

func run(synthetic bool, chromeImage string, chromeVersion string, goldenDir string, captureQuic bool, timeout time.Duration) error {
	if synthetic {
		return writeSyntheticGolden(goldenDir)
	}
	if chromeVersion == "" {
		return fmt.Errorf("-chrome-version is required (stamp the golden with the real Chrome version)")
	}
	if err := captureTlsClientHello(chromeImage, chromeVersion, goldenDir, timeout); err != nil {
		return err
	}
	if captureQuic {
		if err := captureQuicInitial(chromeImage, chromeVersion, goldenDir, timeout); err != nil {
			return err
		}
	}
	return nil
}

// writeSyntheticGolden reproduces the committed synthetic golden from the
// impl-independent uTLS generator.
func writeSyntheticGolden(goldenDir string) error {
	raw, err := fingerprint.GenerateChromeHello(fingerprint.ServerName)
	if err != nil {
		return err
	}
	path := filepath.Join(goldenDir, filepath.Base(fingerprint.GoldenChrome133Synthetic.Path))
	if err := fingerprint.WriteGoldenFile(path, raw); err != nil {
		return err
	}
	fmt.Printf("wrote synthetic golden %s (%d bytes, uTLS HelloChrome_133)\n", path, len(raw))
	return nil
}

// captureTlsClientHello runs headless Docker Chrome against the tcp endpoint,
// captures its ClientHello, writes the version-stamped golden, and diffs it
// against the committed synthetic golden.
func captureTlsClientHello(chromeImage string, chromeVersion string, goldenDir string, timeout time.Duration) error {
	// bind on all interfaces so the container reaches it; on Linux Docker with
	// --network host the container shares the host loopback.
	endpoint, err := fingerprint.NewEndpoint(fingerprint.EndpointOptions{BindHost: "0.0.0.0"})
	if err != nil {
		return err
	}
	defer endpoint.Close()
	url := fmt.Sprintf("https://%s:%d/", fingerprint.ServerName, endpoint.Port())

	fmt.Printf("endpoint listening on :%d, running Chrome from %s against %s\n", endpoint.Port(), chromeImage, url)
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	if err := runDockerChrome(ctx, chromeImage, endpoint.Port(), url, false); err != nil {
		return fmt.Errorf("run docker chrome: %w", err)
	}

	captures := endpoint.CapturedClientHellos()
	if len(captures) == 0 {
		return fmt.Errorf("Chrome connected but no client hello was captured")
	}
	got, err := fingerprint.ParseClientHello(captures[0].Message)
	if err != nil {
		return err
	}
	got.RecordCount = captures[0].RecordCount

	path := filepath.Join(goldenDir, fmt.Sprintf("chrome-%s-clienthello.bin", chromeVersion))
	if err := fingerprint.WriteGoldenFile(path, captures[0].Message); err != nil {
		return err
	}
	fmt.Printf("wrote real golden %s (%d bytes, Chrome %s)\n", path, len(captures[0].Message), chromeVersion)

	// diff the real capture against the committed synthetic golden, so the
	// owner sees exactly where the parrot trails real Chrome.
	golden, err := fingerprint.LoadGolden(fingerprint.GoldenChrome133Synthetic)
	if err != nil {
		return err
	}
	opts := fingerprint.DiffOptions{
		ExpectedServerName:        fingerprint.ServerName,
		ExpectedAlpnProtocols:     got.AlpnProtocols,
		ExpectApplicationSettings: len(got.AlpsProtocols) != 0,
	}
	drifts := fingerprint.Diff(golden.Fingerprint, got, opts)
	fmt.Println(fingerprint.FormatDrift(fingerprint.GoldenChrome133Synthetic, drifts))
	return nil
}

// captureQuicInitial runs headless Docker Chrome forced onto h3 against the quic
// endpoint, captures its first Initial, writes the golden, and prints the
// long-header version and padding.
func captureQuicInitial(chromeImage string, chromeVersion string, goldenDir string, timeout time.Duration) error {
	endpoint, err := fingerprint.NewQuicEndpoint(fingerprint.QuicEndpointOptions{BindHost: "0.0.0.0"})
	if err != nil {
		return err
	}
	defer endpoint.Close()
	udpAddr := endpoint.Addr().String()
	_, portText, _ := splitHostPort(udpAddr)
	url := fmt.Sprintf("https://%s:%s/", fingerprint.ServerName, portText)

	fmt.Printf("quic endpoint on %s, running Chrome (forced h3) against %s\n", udpAddr, url)
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	if err := runDockerChrome(ctx, chromeImage, portFromText(portText), url, true); err != nil {
		return fmt.Errorf("run docker chrome (quic): %w", err)
	}

	initials := endpoint.CapturedInitials()
	if len(initials) == 0 {
		return fmt.Errorf("Chrome connected but no quic initial was captured (did it fall back to tcp?)")
	}
	got, err := fingerprint.ParseQuicInitial(initials[0])
	if err != nil {
		return err
	}
	path := filepath.Join(goldenDir, fmt.Sprintf("chrome-%s-quic-initial.bin", chromeVersion))
	if err := fingerprint.WriteGoldenFile(path, initials[0]); err != nil {
		return err
	}
	fmt.Printf("wrote quic golden %s (%d bytes); version %08x, type %s, padded to %d\n",
		path, len(initials[0]), got.Version, got.PacketType, got.DatagramLength)
	return nil
}

// runDockerChrome runs headless Chrome from a pinned image against url, mapping
// the endpoint name to the host loopback. forceQuic adds the flags that make
// Chrome speak h3 to the endpoint. it trusts the endpoint with
// --ignore-certificate-errors, which does not alter the ClientHello or the
// Initial (it is a post-handshake policy).
func runDockerChrome(ctx context.Context, chromeImage string, port int, url string, forceQuic bool) error {
	args := []string{
		"run", "--rm", "--network", "host", chromeImage,
		"--headless=new", "--no-sandbox", "--disable-gpu",
		"--ignore-certificate-errors",
		fmt.Sprintf("--host-resolver-rules=MAP %s 127.0.0.1", fingerprint.ServerName),
	}
	if forceQuic {
		args = append(args,
			"--enable-quic",
			fmt.Sprintf("--origin-to-force-quic-on=%s:%d", fingerprint.ServerName, port),
		)
	}
	args = append(args, url)
	cmd := exec.CommandContext(ctx, "docker", args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	// Chrome exits on its own after loading; the capture happens at the
	// endpoint during the handshake regardless of the page result.
	_ = cmd.Run()
	// give the endpoint a moment to record the handshake it already read.
	timer := time.NewTimer(500 * time.Millisecond)
	defer timer.Stop()
	select {
	case <-ctx.Done():
	case <-timer.C:
	}
	return nil
}

// splitHostPort splits a host:port, tolerating ipv6 brackets.
func splitHostPort(address string) (host string, port string, ok bool) {
	for i := len(address) - 1; 0 <= i; i -= 1 {
		if address[i] == ':' {
			return address[:i], address[i+1:], true
		}
	}
	return address, "", false
}

// portFromText parses a decimal port, returning 0 on error (the caller has
// already used the text form for the url).
func portFromText(text string) int {
	port := 0
	for _, r := range text {
		if r < '0' || '9' < r {
			return port
		}
		port = port*10 + int(r-'0')
	}
	return port
}

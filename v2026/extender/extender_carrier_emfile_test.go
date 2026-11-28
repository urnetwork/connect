//go:build linux || darwin

// A real EMFILE, from the kernel, rather than one a test's listener answers: the one
// way to the state of 2026-11-01 that needs nothing from outside the host but
// connections, though no host's log says it was this (EXTENDER.md G2). The extender
// runs in a child process with its file descriptors limited, because the limit is the
// process's, and this test connects to it until its accept fails. The build tag leaves
// out the systems whose rlimit fields are another type.

package extender

import (
	"bufio"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"
)

const testEmfileChildEnv = "URNETWORK_TEST_EXTENDER_EMFILE_CHILD"

// How many descriptors the child may open beyond the ones it holds as it starts
// serving. Enough for its first connections, far short of what the parent opens.
const testEmfileSpareDescriptors = 16

// How many connections the parent holds open, which the child cannot all accept.
const testEmfileConnectionCount = 64

func TestExtenderSurvivesFileDescriptorExhaustion(t *testing.T) {
	if os.Getenv(testEmfileChildEnv) != "" {
		testEmfileChild(t)
		return
	}

	child := exec.Command(
		os.Args[0],
		"-test.run=^TestExtenderSurvivesFileDescriptorExhaustion$",
		"-logtostderr=true",
	)
	child.Env = append(os.Environ(), testEmfileChildEnv+"=1")
	stdin, err := child.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := child.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	stderr, err := child.StderrPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		stdin.Close()
		child.Process.Kill()
		child.Wait()
	})

	// the child's accept failing is the point of this test, so it is waited on
	// rather than assumed, in whichever words the build logs it
	sawEmfile := make(chan struct{})
	var sawEmfileOnce atomic.Bool
	go func() {
		scanner := bufio.NewScanner(stderr)
		for scanner.Scan() {
			if strings.Contains(scanner.Text(), "too many open files") && sawEmfileOnce.CompareAndSwap(false, true) {
				close(sawEmfile)
			}
		}
	}()

	port := 0
	scanner := bufio.NewScanner(stdout)
	for scanner.Scan() {
		if text, ok := strings.CutPrefix(scanner.Text(), "port="); ok {
			port, err = strconv.Atoi(text)
			if err != nil {
				t.Fatal(err)
			}
			break
		}
	}
	if port == 0 {
		t.Fatal("the child did not start serving")
	}
	go io.Copy(io.Discard, stdout)
	address := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))

	conns := []net.Conn{}
	closeConns := func() {
		for _, conn := range conns {
			conn.Close()
		}
		conns = nil
	}
	defer closeConns()
	// a dial succeeds from the backlog whether or not it is accepted, and stops
	// when the listener is gone; either way the child has met the limit by then
	for range testEmfileConnectionCount {
		conn, err := net.DialTimeout("tcp", address, 5*time.Second)
		if err != nil {
			break
		}
		conns = append(conns, conn)
	}
	select {
	case <-sawEmfile:
	case <-time.After(30 * time.Second):
		t.Fatal("the child's accept never failed, so this tested nothing")
	}

	// the connections go away, which is the host recovering
	closeConns()

	// and the extender answers again: a tls handshake, the part of serving that
	// needs the accept loop and nothing else
	deadline := time.Now().Add(30 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		lastErr = testEmfileHandshake(address)
		if lastErr == nil {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("the extender did not serve again after the host recovered: %v", lastErr)
}

// One tls handshake with the extender's carrier.
func testEmfileHandshake(address string) error {
	conn, err := net.DialTimeout("tcp", address, 2*time.Second)
	if err != nil {
		return err
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(2 * time.Second))
	tlsConn := tls.Client(conn, &tls.Config{
		InsecureSkipVerify: true,
		ServerName:         testServerName,
	})
	return tlsConn.Handshake()
}

// The child: an extender with only the tcp carrier and none of the limits that would
// turn the connections away before they cost a descriptor, then the limit.
func testEmfileChild(t *testing.T) {
	seed, err := connect.NewExtenderKeySeed()
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port

	settings := DefaultExtenderSettings()
	settings.IdentityKeySeed = seed
	settings.HeaderTimeout = 60 * time.Second
	settings.MaxConnectionCount = 0
	settings.MaxConnectionCountPerSource = 0
	settings.AdmissionSubnetsPerMinute = 0
	settings.AdmissionActionsPerSubnetPerMinute = 0
	settings.AdmissionRefusalsPerSubnetPerMinute = 0
	settings.Listen = func(network string, address string) (net.Listener, error) {
		return listener, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	server := NewExtenderServer(
		ctx,
		nil,
		[]string{"dest.example"},
		map[int][]connect.ExtenderConnectMode{port: {connect.ExtenderConnectModeTcpTls}},
		&net.Dialer{},
		settings,
	)
	go server.ListenAndServe()
	select {
	case <-server.Listening():
	case <-time.After(15 * time.Second):
		t.Fatal("the carrier did not bind")
	}

	// the highest descriptor in use is the newest one, near enough
	probe, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	limit := uint64(probe.Fd()) + testEmfileSpareDescriptors
	probe.Close()
	var rlimit syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &rlimit); err != nil {
		t.Fatal(err)
	}
	rlimit.Cur = limit
	if err := syscall.Setrlimit(syscall.RLIMIT_NOFILE, &rlimit); err != nil {
		t.Fatal(err)
	}
	fmt.Printf("port=%d\n", port)
	os.Stdout.Sync()

	// the parent ends this by closing stdin, or by killing it
	io.Copy(io.Discard, os.Stdin)
}

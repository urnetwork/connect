package connect

// transfer_encrypt.go's limits read from EncryptionSettings (QD-702): the
// fallbacks for a value that is not positive are the defaults, the defaults are
// the constants they replaced, and each limit is read where it is used.

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

// The defaults are the constants these settings replaced: the identity proof
// resends' window without a TlsTimeout, the identity key fetches' interval, a
// generated certificate's validity and the peer certificate read's watch.
func TestEncryptionSettingsDefaultsAreTheFormerConstants(t *testing.T) {
	defaults := DefaultEncryptionSettings()
	cases := []struct {
		name  string
		value time.Duration
		want  time.Duration
	}{
		{name: "IdentityProofResendWindow", value: defaults.IdentityProofResendWindow, want: 60 * time.Second},
		{name: "PeerClientPublicKeyFetchMinInterval", value: defaults.PeerClientPublicKeyFetchMinInterval, want: 1 * time.Second},
		{name: "GeneratedTlsCertificateBackdate", value: defaults.GeneratedTlsCertificateBackdate, want: 1 * time.Hour},
		{name: "GeneratedTlsCertificateLifetime", value: defaults.GeneratedTlsCertificateLifetime, want: 24 * time.Hour},
		{name: "PeerCertificateReadLogThreshold", value: defaults.PeerCertificateReadLogThreshold, want: 2 * time.Second},
	}
	for _, c := range cases {
		if c.value != c.want {
			t.Errorf("%s defaults to %s, want %s", c.name, c.value, c.want)
		}
	}
}

// A value that is not positive falls back to the default, read from
// DefaultEncryptionSettings, the one place it is set; a positive one is read as
// it is set. The nack restart cooldown fell back to 2 s where its default is 5 s.
func TestEncryptionSettingsFallBackToTheDefaults(t *testing.T) {
	defaults := DefaultEncryptionSettings()
	zero := &peerEncryptionSession{settings: &EncryptionSettings{}}
	negative := &peerEncryptionSession{settings: &EncryptionSettings{
		TlsTimeout:                          -1,
		IdentityProofResendWindow:           -1,
		PeerClientPublicKeyFetchMinInterval: -1,
		RequiredCipherPollInterval:          -1,
		UnknownWrapNackMinInterval:          -1,
		UnknownWrapNackRestartCooldown:      -1,
		EarlyClientHelloMinInterval:         -1,
		EarlyClientHelloBurst:               -1,
	}}
	set := &peerEncryptionSession{settings: &EncryptionSettings{
		IdentityProofResendWindow:           7 * time.Second,
		PeerClientPublicKeyFetchMinInterval: 11 * time.Second,
		RequiredCipherPollInterval:          13 * time.Millisecond,
		UnknownWrapNackMinInterval:          17 * time.Second,
		UnknownWrapNackRestartCooldown:      19 * time.Second,
		EarlyClientHelloMinInterval:         23 * time.Second,
		EarlyClientHelloBurst:               29,
	}}
	type reading struct {
		name string
		read func(*peerEncryptionSession) time.Duration
		def  time.Duration
		set  time.Duration
	}
	readings := []reading{
		{name: "identity proof resend window", read: (*peerEncryptionSession).identityProofResendWindow, def: defaults.IdentityProofResendWindow, set: 7 * time.Second},
		{name: "identity key fetch interval", read: (*peerEncryptionSession).peerClientPublicKeyFetchMinInterval, def: defaults.PeerClientPublicKeyFetchMinInterval, set: 11 * time.Second},
		{name: "required cipher poll interval", read: (*peerEncryptionSession).RequiredCipherPollInterval, def: defaults.RequiredCipherPollInterval, set: 13 * time.Millisecond},
		{name: "nack interval", read: (*peerEncryptionSession).unknownWrapNackMinInterval, def: defaults.UnknownWrapNackMinInterval, set: 17 * time.Second},
		{name: "nack restart cooldown", read: (*peerEncryptionSession).unknownWrapNackRestartCooldown, def: defaults.UnknownWrapNackRestartCooldown, set: 19 * time.Second},
		{name: "early ClientHello interval", read: (*peerEncryptionSession).earlyClientHelloMinInterval, def: defaults.EarlyClientHelloMinInterval, set: 23 * time.Second},
		{name: "early ClientHello burst", read: func(s *peerEncryptionSession) time.Duration { return time.Duration(s.earlyClientHelloBurst()) }, def: time.Duration(defaults.EarlyClientHelloBurst), set: 29},
	}
	for _, r := range readings {
		if got := r.read(zero); got != r.def {
			t.Errorf("zero %s reads %s, want the default %s", r.name, got, r.def)
		}
		if got := r.read(negative); got != r.def {
			t.Errorf("negative %s reads %s, want the default %s", r.name, got, r.def)
		}
		if got := r.read(set); got != r.set {
			t.Errorf("set %s reads %s, want %s", r.name, got, r.set)
		}
	}
	// a positive TlsTimeout is the identity proof resends' window
	withTimeout := &peerEncryptionSession{settings: &EncryptionSettings{TlsTimeout: 31 * time.Second, IdentityProofResendWindow: 7 * time.Second}}
	if got := withTimeout.identityProofResendWindow(); got != 31*time.Second {
		t.Errorf("with a TlsTimeout, the identity proof resend window is %s, want the TlsTimeout", got)
	}
}

// Whether got lies within [before+d, after+d] at the second precision a
// certificate's dates carry.
func validityWithin(got time.Time, before time.Time, after time.Time, d time.Duration) bool {
	return !got.Before(before.Add(d).Truncate(time.Second)) && !got.After(after.Add(d))
}

// A generated certificate is valid from the settings' backdate before its
// creation for their lifetime after it, the defaults' when they are not
// positive, and so is the one a manager generates for its own identity.
func TestGeneratedTlsCertificateTakesItsValidityFromSettings(t *testing.T) {
	cases := []struct {
		settings     *EncryptionSettings
		wantBackdate time.Duration
		wantLifetime time.Duration
	}{
		{settings: &EncryptionSettings{GeneratedTlsCertificateBackdate: 3 * time.Hour, GeneratedTlsCertificateLifetime: 5 * time.Hour}, wantBackdate: 3 * time.Hour, wantLifetime: 5 * time.Hour},
		{settings: &EncryptionSettings{}, wantBackdate: 1 * time.Hour, wantLifetime: 24 * time.Hour},
		{settings: nil, wantBackdate: 1 * time.Hour, wantLifetime: 24 * time.Hour},
	}
	for i, c := range cases {
		before := time.Now()
		cert, err := generateSequenceTlsCertificateForSettings(c.settings)
		after := time.Now()
		if err != nil {
			t.Fatalf("case %d: %v", i, err)
		}
		if !validityWithin(cert.Leaf.NotBefore, before, after, -c.wantBackdate) {
			t.Errorf("case %d: valid from %s, want %s before its creation", i, cert.Leaf.NotBefore, c.wantBackdate)
		}
		if !validityWithin(cert.Leaf.NotAfter, before, after, c.wantLifetime) {
			t.Errorf("case %d: valid until %s, want %s after its creation", i, cert.Leaf.NotAfter, c.wantLifetime)
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	settings := DefaultClientSettings()
	settings.EncryptionSettings.Mode = EncryptionModeOpportunistic
	settings.EncryptionSettings.GeneratedTlsCertificateLifetime = 7 * time.Hour
	before := time.Now()
	client := NewClient(ctx, NewId(), NewNoContractClientOob(), settings)
	after := time.Now()
	defer client.Cancel()
	serverTlsConfig := client.EncryptionSessionManager().ServerTlsConfig()
	if serverTlsConfig == nil || len(serverTlsConfig.Certificates) == 0 || serverTlsConfig.Certificates[0].Leaf == nil {
		t.Fatal("the manager generated no certificate")
	}
	if notAfter := serverTlsConfig.Certificates[0].Leaf.NotAfter; !validityWithin(notAfter, before, after, 7*time.Hour) {
		t.Fatalf("the manager's certificate is valid until %s, want 7h after its creation", notAfter)
	}
}

// The identity key fetch a session starts waits the settings' interval before
// the next, the default's when it is not positive.
func TestPeerClientPublicKeyFetchWaitsItsInterval(t *testing.T) {
	cases := []struct {
		interval time.Duration
		want     time.Duration
	}{
		{interval: time.Hour, want: time.Hour},
		{interval: 0, want: DefaultEncryptionSettings().PeerClientPublicKeyFetchMinInterval},
	}
	for _, c := range cases {
		func() {
			settings := DefaultClientSettings()
			settings.EncryptionSettings.Mode = EncryptionModeRequired
			settings.EncryptionSettings.PeerClientPublicKeyFetchMinInterval = c.interval
			sess, cleanup := newEarlyHelloSession(t, settings)
			defer cleanup()
			fetched := make(chan struct{}, 1)
			sess.peerClientPublicKeyFetcher = func(ctx context.Context) ([]byte, error) {
				fetched <- struct{}{}
				return nil, fmt.Errorf("no key published")
			}
			before := time.Now()
			sess.maybeFetchPeerClientPublicKeyForIdentity()
			after := time.Now()
			select {
			case <-fetched:
			case <-time.After(10 * time.Second):
				t.Fatal("no identity key fetch started")
			}
			sess.stateLock.Lock()
			next := sess.nextPeerKeyFetchTime
			sess.stateLock.Unlock()
			if next.Before(before.Add(c.want)) || next.After(after.Add(c.want)) {
				t.Errorf("interval %s: the next fetch may start %s after the first, want %s", c.interval, next.Sub(before), c.want)
			}
		}()
	}
}

// A net.Conn whose Read reports that the TLS handshake reading it has begun,
// and so holds the conn's handshake lock, and then waits until released.
type heldHandshakeConn struct {
	reading     chan struct{}
	readingOnce sync.Once
	release     chan struct{}
	releaseOnce sync.Once
}

func newHeldHandshakeConn() *heldHandshakeConn {
	return &heldHandshakeConn{reading: make(chan struct{}), release: make(chan struct{})}
}

// Ends the Read the handshake waits in, failing the handshake. Idempotent.
func (self *heldHandshakeConn) Release() {
	self.releaseOnce.Do(func() { close(self.release) })
}

func (self *heldHandshakeConn) Read(p []byte) (int, error) {
	self.readingOnce.Do(func() { close(self.reading) })
	<-self.release
	return 0, net.ErrClosed
}

func (self *heldHandshakeConn) Write(p []byte) (int, error)        { return len(p), nil }
func (self *heldHandshakeConn) Close() error                       { return nil }
func (self *heldHandshakeConn) LocalAddr() net.Addr                { return &net.TCPAddr{} }
func (self *heldHandshakeConn) RemoteAddr() net.Addr               { return &net.TCPAddr{} }
func (self *heldHandshakeConn) SetDeadline(t time.Time) error      { return nil }
func (self *heldHandshakeConn) SetReadDeadline(t time.Time) error  { return nil }
func (self *heldHandshakeConn) SetWriteDeadline(t time.Time) error { return nil }

// A Logger with V(2) on that reports the peer certificate watch's line.
type peerCertificateWatchLogger struct {
	noopLogger
	lines chan string
}

func (self *peerCertificateWatchLogger) Infof(format string, args ...any) {
	line := fmt.Sprintf(format, args...)
	if strings.Contains(line, "[peercert-block]") {
		select {
		case self.lines <- line:
		default:
		}
	}
}

func (self *peerCertificateWatchLogger) V(level int32) Verbose {
	return peerCertificateWatchVerbose{logger: self}
}

type peerCertificateWatchVerbose struct {
	logger *peerCertificateWatchLogger
}

func (self peerCertificateWatchVerbose) Enabled() bool                    { return true }
func (self peerCertificateWatchVerbose) Info(args ...any)                 {}
func (self peerCertificateWatchVerbose) Infof(format string, args ...any) {}

// With V(2) on, a read of an epoch's peer certificates that waits on a
// handshake in progress is logged once it has waited the settings' threshold.
func TestPeerCertificateReadWatchLogsAtItsThreshold(t *testing.T) {
	logger := &peerCertificateWatchLogger{lines: make(chan string, 1)}
	settings := DefaultClientSettings()
	settings.Log = logger
	settings.EncryptionSettings.Mode = EncryptionModeRequired
	settings.EncryptionSettings.PeerCertificateReadLogThreshold = time.Millisecond
	sess, cleanup := newEarlyHelloSession(t, settings)
	defer cleanup()

	// the handshake holds its conn's handshake lock while it waits in Read
	conn := newHeldHandshakeConn()
	tlsConn := tls.Server(conn, sess.tlsConfig)
	handshakeDone := make(chan struct{})
	go func() {
		defer close(handshakeDone)
		tlsConn.HandshakeContext(context.Background())
	}()
	readDone := make(chan struct{})
	defer func() {
		conn.Release()
		<-handshakeDone
		<-readDone
	}()
	select {
	case <-conn.reading:
	case <-time.After(10 * time.Second):
		close(readDone)
		t.Fatal("the handshake never read its conn")
	}
	go func() {
		defer close(readDone)
		sess.peerCertificatesOfEpoch(&tlsHandshakeEpoch{tlsConn: tlsConn})
	}()
	select {
	case line := <-logger.lines:
		if !strings.Contains(line, ">1ms") {
			t.Fatalf("the watch logged %q, want its threshold of 1ms", line)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the watch logged nothing while the read waited on the handshake")
	}
}

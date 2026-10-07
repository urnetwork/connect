package connect

// Root-cause tests for the client camouflage race and gating (EXTENDER.md P7,
// P8). Deterministic: the stagger clock and both dials are injected, so the
// camo-first-then-legacy ordering is forced with barriers rather than timing.

import (
	"context"
	"errors"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urnetwork/connect/protocol"
)

// A net.Conn stub a winning dial returns; only Close is exercised by the race.
type closeOnlyConn struct {
	net.Conn
	closed atomic.Bool
}

func (self *closeOnlyConn) Close() error {
	self.closed.Store(true)
	return nil
}

// A stagger that never fires, so the legacy attempt launches only when the
// camouflaged attempt fails.
func neverStagger() <-chan time.Time {
	return make(chan time.Time)
}

// The camouflaged attempt wins when it answers before the stagger, and the
// legacy attempt is never launched (P8).
func TestRaceExtenderTcpCamouflageCamoWinsWithoutLegacy(t *testing.T) {
	camoConn := &closeOnlyConn{}
	var legacyLaunched atomic.Bool
	camoDial := func(ctx context.Context, roundTrip *ExtenderRoundTrip) (net.Conn, *protocol.ExtenderResponse, error) {
		return camoConn, &protocol.ExtenderResponse{}, nil
	}
	legacyDial := func(ctx context.Context, roundTrip *ExtenderRoundTrip) (net.Conn, *protocol.ExtenderResponse, error) {
		legacyLaunched.Store(true)
		return &closeOnlyConn{}, &protocol.ExtenderResponse{}, nil
	}
	conn, _, err := raceExtenderTcpCamouflage(context.Background(), nil, neverStagger, camoDial, legacyDial)
	if err != nil {
		t.Fatal(err)
	}
	if conn != camoConn {
		t.Fatal("the camouflaged attempt did not win")
	}
	if legacyLaunched.Load() {
		t.Fatal("the legacy attempt launched though the camouflaged attempt answered first")
	}
}

// When the camouflaged attempt fails, the legacy attempt launches at once and
// its answer wins (P8): this is the skew fallback at the race layer.
func TestRaceExtenderTcpCamouflageFallsToLegacyOnCamoFailure(t *testing.T) {
	legacyConn := &closeOnlyConn{}
	camoDial := func(ctx context.Context, roundTrip *ExtenderRoundTrip) (net.Conn, *protocol.ExtenderResponse, error) {
		return nil, nil, errors.New("camouflaged attempt not authenticated")
	}
	legacyDial := func(ctx context.Context, roundTrip *ExtenderRoundTrip) (net.Conn, *protocol.ExtenderResponse, error) {
		return legacyConn, &protocol.ExtenderResponse{}, nil
	}
	conn, _, err := raceExtenderTcpCamouflage(context.Background(), nil, neverStagger, camoDial, legacyDial)
	if err != nil {
		t.Fatal(err)
	}
	if conn != legacyConn {
		t.Fatal("the race did not fall to the legacy attempt")
	}
}

// Both attempts failing joins their errors (P8): this is a skewed client in
// Phase B, where the legacy attempt is spliced and also fails.
func TestRaceExtenderTcpCamouflageBothFail(t *testing.T) {
	camoErr := errors.New("camo not authenticated")
	legacyErr := errors.New("legacy spliced, leaf check failed")
	camoDial := func(ctx context.Context, roundTrip *ExtenderRoundTrip) (net.Conn, *protocol.ExtenderResponse, error) {
		return nil, nil, camoErr
	}
	legacyDial := func(ctx context.Context, roundTrip *ExtenderRoundTrip) (net.Conn, *protocol.ExtenderResponse, error) {
		return nil, nil, legacyErr
	}
	_, _, err := raceExtenderTcpCamouflage(context.Background(), nil, neverStagger, camoDial, legacyDial)
	if err == nil {
		t.Fatal("the race returned no error though both attempts failed")
	}
	if !errors.Is(err, camoErr) || !errors.Is(err, legacyErr) {
		t.Fatalf("the race did not join both errors: %v", err)
	}
}

// A refusal or a limit from the camouflaged attempt ends the race with that
// answer and never launches the legacy attempt (P8): the extender gives the same
// answer to either attempt.
func TestRaceExtenderTcpCamouflageRefusalShortCircuits(t *testing.T) {
	var legacyLaunched atomic.Bool
	camoDial := func(ctx context.Context, roundTrip *ExtenderRoundTrip) (net.Conn, *protocol.ExtenderResponse, error) {
		return nil, nil, &ExtenderLimitedError{RetryAfter: time.Second}
	}
	legacyDial := func(ctx context.Context, roundTrip *ExtenderRoundTrip) (net.Conn, *protocol.ExtenderResponse, error) {
		legacyLaunched.Store(true)
		return &closeOnlyConn{}, &protocol.ExtenderResponse{}, nil
	}
	_, _, err := raceExtenderTcpCamouflage(context.Background(), nil, neverStagger, camoDial, legacyDial)
	var limitedErr *ExtenderLimitedError
	if !errors.As(err, &limitedErr) {
		t.Fatalf("the race did not return the limit: %v", err)
	}
	if legacyLaunched.Load() {
		t.Fatal("the legacy attempt launched though the camouflaged attempt was limited")
	}
}

// The stagger launches the legacy attempt while the camouflaged attempt is still
// pending (P8): the staggered launch, forced by a controllable stagger channel.
func TestRaceExtenderTcpCamouflageStaggerLaunchesLegacy(t *testing.T) {
	staggerC := make(chan time.Time, 1)
	camoReleased := make(chan struct{})
	legacyConn := &closeOnlyConn{}
	var legacyLaunched sync.WaitGroup
	legacyLaunched.Add(1)
	legacyLaunchedOnce := sync.Once{}

	camoDial := func(ctx context.Context, roundTrip *ExtenderRoundTrip) (net.Conn, *protocol.ExtenderResponse, error) {
		// stay pending until released, so the stagger must launch legacy
		select {
		case <-camoReleased:
			return nil, nil, errors.New("camo released late")
		case <-ctx.Done():
			return nil, nil, ctx.Err()
		}
	}
	legacyDial := func(ctx context.Context, roundTrip *ExtenderRoundTrip) (net.Conn, *protocol.ExtenderResponse, error) {
		legacyLaunchedOnce.Do(legacyLaunched.Done)
		return legacyConn, &protocol.ExtenderResponse{}, nil
	}

	done := make(chan struct{})
	var raceConn net.Conn
	var raceErr error
	go func() {
		defer close(done)
		raceConn, _, raceErr = raceExtenderTcpCamouflage(
			context.Background(),
			nil,
			func() <-chan time.Time { return staggerC },
			camoDial,
			legacyDial,
		)
	}()

	// fire the stagger: the legacy attempt must launch while camo is pending
	staggerC <- time.Now()
	legacyLaunched.Wait()
	// let the race complete on the legacy answer
	<-done
	close(camoReleased)
	if raceErr != nil {
		t.Fatal(raceErr)
	}
	if raceConn != legacyConn {
		t.Fatal("the staggered legacy attempt did not win while camo was pending")
	}
}

// The kill switch and the gating of the camouflaged attempt (P7, P8).
func TestExtenderCamouflageApplies(t *testing.T) {
	realityKey := make([]byte, extenderRealityX25519PublicKeyByteCount)
	identityKey := make([]byte, 32)
	base := func() *ExtenderConfig {
		return &ExtenderConfig{
			RealityPublicKey: append([]byte(nil), realityKey...),
			PublicKey:        append([]byte(nil), identityKey...),
		}
	}
	chromeSettings := &ConnectSettings{TlsClientHelloFingerprint: TlsClientHelloFingerprintChrome}
	if !extenderCamouflageApplies(chromeSettings, base()) {
		t.Fatal("camouflage should apply with both keys and the chrome fingerprint")
	}
	emptySettings := &ConnectSettings{}
	if !extenderCamouflageApplies(emptySettings, base()) {
		t.Fatal("camouflage should apply with the default (empty) fingerprint")
	}
	goSettings := &ConnectSettings{TlsClientHelloFingerprint: TlsClientHelloFingerprintGo}
	if extenderCamouflageApplies(goSettings, base()) {
		t.Fatal("the go kill switch did not drop the camouflaged attempt")
	}
	noReality := base()
	noReality.RealityPublicKey = nil
	if extenderCamouflageApplies(chromeSettings, noReality) {
		t.Fatal("camouflage applied without a reality key")
	}
	noIdentity := base()
	noIdentity.PublicKey = nil
	if extenderCamouflageApplies(chromeSettings, noIdentity) {
		t.Fatal("camouflage applied without the identity key the B3 check needs")
	}
}

// The front name is drawn from the bundled borrow list, and is empty when none
// is bundled, which drops the camouflaged attempt to legacy alone (P5, P8).
func TestExtenderCamouflageFrontName(t *testing.T) {
	restore := setBorrowDomainsForTest([]string{"front.example"})
	defer restore()
	if got := extenderCamouflageFrontName(&ExtenderConfig{}); got != "front.example" {
		t.Fatalf("front name = %q, expected front.example", got)
	}
	restoreEmpty := setBorrowDomainsForTest([]string{})
	defer restoreEmpty()
	if got := extenderCamouflageFrontName(&ExtenderConfig{}); got != "" {
		t.Fatalf("front name = %q, expected empty with no borrow list", got)
	}
}

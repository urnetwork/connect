package connect

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
	"testing"

	quic "github.com/quic-go/quic-go"
)

// The outer carrier must acquire against the same owner as the already-live
// H1 graph. Counting only the inner H1 reservation leaves QUIC receive windows
// invisible, and using a fresh/default budget per dial repeats that escape.
func TestExtenderQuicChargesH1OwnerBeforeOpeningSocket(t *testing.T) {
	settings := DefaultPlatformTransportSettingsWithMemoryTarget(mib(24))
	budget := settings.PlatformTransportBudget
	h1 := budget.register(platformTransportBudgetH1, settings.H1BudgetByteCount, true)
	if !h1.Acquire(context.Background()) {
		t.Fatal("H1 admission failed")
	}
	defer h1.Release()
	transport := &PlatformTransport{settings: settings}
	ctx := transport.dialContext(context.Background())
	marker := errors.New("packet factory failure")
	opened := 0
	connectSettings := DefaultConnectSettings()
	connectSettings.DialContextSettings = &DialContextSettings{
		PacketConnFactory: func(context.Context) (net.PacketConn, error) {
			opened++
			want := settings.H1BudgetByteCount + newExtenderQuicMemoryPolicy(ctx, connectSettings).byteCount
			if got := budget.Stats().UsedByteCount; got != want {
				t.Errorf("budget at socket open = %d, want H1 + outer QUIC = %d", got, want)
			}
			return nil, marker
		},
	}
	_, _, err := dialExtenderQuic(ctx, connectSettings, &ExtenderConfig{
		Ip: netip.MustParseAddr("192.0.2.1"),
	}, &tls.Config{}, nil)
	if !errors.Is(err, marker) || opened != 1 {
		t.Fatalf("dial = %v, socket opens = %d", err, opened)
	}
	if got := budget.Stats().UsedByteCount; got != settings.H1BudgetByteCount {
		t.Fatalf("failed outer dial retained %d bytes, want only H1 = %d", got, settings.H1BudgetByteCount)
	}
}

func TestExtenderQuicPolicyBoundsAllRetainedAreas(t *testing.T) {
	settings := DefaultPlatformTransportSettingsWithMemoryTarget(mib(24))
	ctx := (&PlatformTransport{settings: settings}).dialContext(context.Background())
	policy := newExtenderQuicMemoryPolicy(ctx, DefaultConnectSettings())
	config := policy.quicConfig
	if config.MaxStreamReceiveWindow != uint64(kib(256)) ||
		config.MaxConnectionReceiveWindow != uint64(kib(512)) ||
		config.MaxIncomingStreams != 8 || config.MaxIncomingUniStreams != 8 || config.EnableDatagrams {
		t.Fatalf("outer QUIC policy = %+v", config)
	}
	wantClaim := kib(512) + kib(512) + extenderQuicSendMemoryByteCount + 2*kib(64)
	if policy.byteCount != wantClaim {
		t.Fatalf("outer claim = %d, want runtime + both socket buffers = %d", policy.byteCount, wantClaim)
	}
	ptSettings := policy.packetTranslationSettings()
	if ptSettings.DnsMaxCombinedPacketByteCount != 2048 ||
		ptSettings.SequenceBufferSize != 4 || ptSettings.DnsMaxCombineBytes != kib(64) || ptSettings.DnsMaxCombineBytesPerAddress != kib(64) {
		t.Fatalf("DNS settings do not follow the owner: %+v", ptSettings)
	}
	wantClaim += kib(64) + ByteCount(4*(ptSettings.SequenceBufferSize+1))*kib(4)
	if policy.byteCount != wantClaim {
		t.Fatalf("DNS claim = %d, want all retained areas = %d", policy.byteCount, wantClaim)
	}
	for _, config := range []*quic.Config{{}, {
		InitialStreamReceiveWindow: uint64(mib(20)), MaxStreamReceiveWindow: uint64(mib(20)),
		InitialConnectionReceiveWindow: uint64(mib(20)), MaxConnectionReceiveWindow: uint64(mib(20)),
		MaxIncomingStreams: 100, MaxIncomingUniStreams: 100,
	}} {
		policy.boundReceiveConfig(config)
		// Timeouts, packet size and keepalive are deliberately left to the
		// API transport, so compare only memory-affecting fields.
		if config.InitialStreamReceiveWindow != policy.quicConfig.InitialStreamReceiveWindow ||
			config.MaxStreamReceiveWindow != policy.quicConfig.MaxStreamReceiveWindow ||
			config.InitialConnectionReceiveWindow != policy.quicConfig.InitialConnectionReceiveWindow ||
			config.MaxConnectionReceiveWindow != policy.quicConfig.MaxConnectionReceiveWindow ||
			config.MaxIncomingStreams != 8 || config.MaxIncomingUniStreams != 8 {
			t.Fatalf("API receive config escaped the claim: %+v", config)
		}
	}
}

func TestExtenderQuicPreservesPendingH1Claims(t *testing.T) {
	settings := DefaultPlatformTransportSettingsWithMemoryTarget(mib(24))
	ctx := (&PlatformTransport{settings: settings}).dialContext(context.Background())
	policy := newExtenderQuicMemoryPolicy(ctx, DefaultConnectSettings())
	policy.budget = NewPlatformTransportBudget(policy.byteCount+settings.H1BudgetByteCount-1, 16)
	h1 := policy.budget.register(platformTransportBudgetH1, settings.H1BudgetByteCount, true)
	if claim, err := policy.acquire(ctx); claim != nil || !errors.Is(err, errExtenderMemoryBudget) {
		if claim != nil {
			claim.Release()
		}
		t.Fatalf("outer carrier stole pending H1 bytes: claim=%v err=%v", claim, err)
	}
	h1.Release()
	claim, err := policy.acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	claim.Release()
}

func TestExtenderQuicConcurrentClaimsAndCleanupAreExact(t *testing.T) {
	settings := DefaultPlatformTransportSettingsWithMemoryTarget(mib(24))
	ctx := (&PlatformTransport{settings: settings}).dialContext(context.Background())
	policy := newExtenderQuicMemoryPolicy(ctx, DefaultConnectSettings())
	policy.budget = NewPlatformTransportBudget(2*policy.byteCount, 16)
	const attempts = 32
	claims := make(chan *platformTransportBudgetReservation, attempts)
	var wg sync.WaitGroup
	for range attempts {
		wg.Add(1)
		go func() {
			defer wg.Done()
			claim, err := policy.acquire(ctx)
			if err == nil {
				claims <- claim
			} else if !errors.Is(err, errExtenderMemoryBudget) {
				t.Errorf("admission: %v", err)
			}
		}()
	}
	wg.Wait()
	close(claims)
	if len(claims) != 2 || policy.budget.Stats().UsedByteCount != 2*policy.byteCount {
		t.Fatalf("simultaneous admission escaped capacity: claims=%d stats=%+v", len(claims), policy.budget.Stats())
	}
	for claim := range claims {
		claim.Release()
		claim.Release()
	}
	stats := policy.budget.Stats()
	if stats.UsedByteCount != 0 || stats.ReservedByteCount != stats.ReleasedByteCount {
		t.Fatalf("claim cleanup imbalance: %+v", stats)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if claim, err := policy.acquire(canceled); claim != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled admission = %v, %v", claim, err)
	}
}

type extenderMemoryPacketConn struct {
	packetBufferSpy
	closeCount atomic.Int32
	onClose    func()
}

func (self *extenderMemoryPacketConn) Close() error {
	self.closeCount.Add(1)
	if self.onClose != nil {
		self.onClose()
	}
	return nil
}

func TestAltQuicCapsSocketAndKeepsClaimThroughFailureCleanup(t *testing.T) {
	settings := DefaultPlatformTransportSettingsWithMemoryTarget(mib(24))
	ctx := (&PlatformTransport{settings: settings}).dialContext(context.Background())
	connectSettings := DefaultConnectSettings()
	policy := newExtenderQuicMemoryPolicy(ctx, connectSettings)
	socket := &extenderMemoryPacketConn{}
	socket.onClose = func() {
		if got := policy.budget.Stats().UsedByteCount; got != policy.byteCount {
			t.Errorf("socket still open with %d reserved bytes, want %d", got, policy.byteCount)
		}
	}
	connectSettings.DialContextSettings = &DialContextSettings{
		PacketConnFactory: func(context.Context) (net.PacketConn, error) { return socket, nil },
	}
	marker := errors.New("translation setup failed")
	wrap := func(_ context.Context, conn net.PacketConn) (net.PacketConn, error) {
		conn.(interface{ SetReadBuffer(int) error }).SetReadBuffer(7 * 1024 * 1024)
		conn.(interface{ SetWriteBuffer(int) error }).SetWriteBuffer(7 * 1024 * 1024)
		if socket.readBufferByteCount != int(kib(64)) || socket.writeBufferByteCount != int(kib(64)) {
			t.Fatalf("socket caps = %d, %d", socket.readBufferByteCount, socket.writeBufferByteCount)
		}
		return nil, marker
	}
	_, err := dialAltQuicAttempt(ctx, connectSettings, &net.UDPAddr{}, wrap, &tls.Config{}, &quic.Config{}, policy)
	if !errors.Is(err, marker) || socket.closeCount.Load() != 1 {
		t.Fatalf("failed dial = %v, socket closes = %d", err, socket.closeCount.Load())
	}
	if stats := policy.budget.Stats(); stats.UsedByteCount != 0 || stats.ReservedByteCount != stats.ReleasedByteCount {
		t.Fatalf("failed dial retained claim: %+v", stats)
	}
}

func TestUnownedMobileQuicUsesTheProcessRoot(t *testing.T) {
	oldBudget := MemoryBudget()
	defer SetMemoryBudget(oldBudget)
	SetMemoryBudget(mib(32))
	policy := newExtenderQuicMemoryPolicy(context.Background(), DefaultConnectSettings())
	claim, err := policy.acquire(context.Background())
	if err != nil || claim == nil || claim.budget != DefaultPlatformTransportBudget() {
		t.Fatalf("unowned mobile carrier did not acquire the process root: %v, %v", claim, err)
	}
	defer claim.Release()
	settings := DefaultPlatformTransportSettingsWithMemoryTarget(mib(24))
	ctx := (&PlatformTransport{settings: settings}).dialContext(context.Background())
	ownedPolicy := newExtenderQuicMemoryPolicy(ctx, DefaultConnectSettings())
	ownedClaim, err := ownedPolicy.acquire(ctx)
	if err != nil {
		t.Fatalf("device-owned mobile carrier was refused: %v", err)
	}
	ownedClaim.Release()
	dialer := &clientDialer{}
	dialer.Update(context.Background(), errExtenderMemoryBudget)
	if dialer.errorCount != 0 || !dialer.lastErrorTime.IsZero() {
		t.Fatal("local memory refusal poisoned the healthy extender")
	}
}

type extenderMemoryStreamConn struct {
	net.Conn
	closeCount atomic.Int32
	onClose    func()
}

func (self *extenderMemoryStreamConn) Close() error {
	self.closeCount.Add(1)
	self.onClose()
	return nil
}

func TestExtenderTcpSharesOwnerAndReleasesAfterSocket(t *testing.T) {
	settings := DefaultPlatformTransportSettingsWithMemoryTarget(mib(20))
	budget := settings.PlatformTransportBudget
	ctx := (&PlatformTransport{settings: settings}).dialContext(context.Background())
	h1 := budget.register(platformTransportBudgetH1, settings.H1BudgetByteCount, true)
	if !h1.Acquire(ctx) {
		t.Fatal("inner H1 admission failed")
	}
	defer h1.Release()
	claim, err := acquireExtenderTcpMemory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if budget.Stats().UsedByteCount != 2*settings.H1BudgetByteCount {
		t.Fatal("outer TLS/socket graph was not additive to the inner H1")
	}
	socket := &extenderMemoryStreamConn{onClose: func() {
		if budget.Stats().UsedByteCount != 2*settings.H1BudgetByteCount {
			t.Error("live socket lost its claim before close")
		}
	}}
	conn := &extenderBudgetConn{Conn: socket, reservation: claim}
	var wg sync.WaitGroup
	for range 16 {
		wg.Add(1)
		go func() { defer wg.Done(); conn.Close() }()
	}
	wg.Wait()
	if socket.closeCount.Load() != 1 || budget.Stats().UsedByteCount != settings.H1BudgetByteCount {
		t.Fatal("TCP lifetime close/release was not exact")
	}
	fullBudget := NewPlatformTransportBudget(settings.H1BudgetByteCount, 1)
	settings.PlatformTransportBudget = fullBudget
	fullH1 := fullBudget.register(platformTransportBudgetH1, settings.H1BudgetByteCount, true)
	if !fullH1.Acquire(ctx) {
		t.Fatal("single H1 admission failed")
	}
	defer fullH1.Release()
	if claim, err := acquireExtenderTcpMemory(ctx); claim != nil || !errors.Is(err, errExtenderMemoryBudget) {
		t.Fatalf("full H1 admitted an uncharged outer TLS graph: %v %v", claim, err)
	}
}

func TestExtenderTcpUnownedMobileUsesTheProcessRoot(t *testing.T) {
	oldBudget := MemoryBudget()
	defer SetMemoryBudget(oldBudget)
	SetMemoryBudget(mib(32))
	claim, err := acquireExtenderTcpMemory(context.Background())
	if err != nil || claim == nil || claim.budget != DefaultPlatformTransportBudget() {
		t.Fatalf("unowned mobile TCP did not acquire the process root: %v %v", claim, err)
	}
	claim.Release()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := acquireExtenderTcpMemory(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled TCP admission=%v", err)
	}
	SetMemoryBudget(0)
	if claim, err := acquireExtenderTcpMemory(context.Background()); claim != nil || err != nil {
		t.Fatalf("unbudgeted server TCP compatibility changed: %v %v", claim, err)
	}
}

func TestExtenderQuicRefusesFullH1OwnerWithoutOpeningSocket(t *testing.T) {
	settings := DefaultPlatformTransportSettingsWithMemoryTarget(mib(24))
	settings.PlatformTransportBudget = NewPlatformTransportBudget(settings.H1BudgetByteCount, 1)
	budget := settings.PlatformTransportBudget
	h1 := budget.register(platformTransportBudgetH1, settings.H1BudgetByteCount, true)
	if !h1.Acquire(context.Background()) {
		t.Fatal("H1 admission failed")
	}
	defer h1.Release()
	transport := &PlatformTransport{settings: settings}
	connectSettings := DefaultConnectSettings()
	opened := 0
	connectSettings.DialContextSettings = &DialContextSettings{
		PacketConnFactory: func(context.Context) (net.PacketConn, error) {
			opened++
			return nil, errors.New("unexpected socket open")
		},
	}
	// No cancellation timer releases this call: admission must be nonblocking
	// because the caller already holds the H1 bytes it would be waiting on.
	_, _, err := dialExtenderQuic(transport.dialContext(context.Background()), connectSettings, &ExtenderConfig{
		Ip: netip.MustParseAddr("192.0.2.1"),
	}, &tls.Config{}, nil)
	if err == nil || opened != 0 {
		t.Fatalf("full-budget dial = %v, socket opens = %d", err, opened)
	}
	if got := budget.Stats().UsedByteCount; got != settings.H1BudgetByteCount {
		t.Fatalf("rejected outer dial changed H1 reservation to %d", got)
	}
}

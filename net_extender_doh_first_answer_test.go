// Real bootstrap/manual consumers must begin safe feed work before a held
// DNS sibling finishes. All DNS transport is wire-format HTTP over net.Pipe.
package connect

import (
	"bufio"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/net/dns/dnsmessage"
)

// Owns the actual network client and synthetic DNS/socket boundaries. The
// held query is released only by the test after observing the consumer.
type extenderFirstAnswerFixture struct {
	ctx           context.Context
	cancel        context.CancelFunc
	client        *ExtenderNetworkClient
	strategy      *ClientStrategy
	directory     *ExtenderDirectory
	readyAddr     netip.Addr
	lateAddr      netip.Addr
	heldEntered   chan struct{}
	heldRelease   chan struct{}
	heldDone      chan struct{}
	feedEntered   chan struct{}
	releaseOnce   sync.Once
	feedOnce      sync.Once
	heldOnce      sync.Once
	heldDoneOnce  sync.Once
	serverWorkers sync.WaitGroup
	activeServers atomic.Int64
	queries       atomic.Int64
	feedDials     atomic.Int64
}

// No system DNS or external socket is available. Signed mode verifies a real
// synthetic root-signed TXT record; unsigned bootstrap remains nondialable.
func newExtenderFirstAnswerFixture(t *testing.T, readyIpv6 bool, mode string) *extenderFirstAnswerFixture {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	self := &extenderFirstAnswerFixture{
		ctx: ctx, cancel: cancel,
		readyAddr:   netip.MustParseAddr("192.0.2.121"),
		lateAddr:    netip.MustParseAddr("2001:db8::121"),
		heldEntered: make(chan struct{}),
		heldRelease: make(chan struct{}),
		heldDone:    make(chan struct{}),
		feedEntered: make(chan struct{}),
	}
	if readyIpv6 {
		self.readyAddr, self.lateAddr = self.lateAddr, self.readyAddr
	}
	clock := newTestClock()
	directorySettings := DefaultExtenderDirectorySettings()
	directorySettings.Now = clock.Now
	directorySettings.NetworkHosts = []string{testExtenderNetworkHost}
	self.directory = NewExtenderDirectory(ctx, directorySettings)
	rootPrivate, rootPublic := newTestRootKeyPair(t)
	self.directory.SetRootKeys(NewExtenderRootKeySet(rootPublic))

	dohSettings := DefaultDohSettings()
	dohSettings.Log = NewNoopLogger()
	dohSettings.RequestTimeout = time.Hour
	dohSettings.DnsResolverSettings = &DnsResolverSettings{
		EnableRemoteDoh:   true,
		RemoteDohUrlsIpv4: []string{"http://192.0.2.122/dns-query"},
		RemoteDohUrlsIpv6: []string{"http://[2001:db8::122]/dns-query"},
	}
	// The HTTP exchange outlives the connect callback's context, which the
	// production dial lifecycle cancels as soon as this endpoint is returned.
	dohSettings.DialContextSettings = &DialContextSettings{DialContext: func(_ context.Context, network string, address string) (net.Conn, error) {
		client, server := net.Pipe()
		self.serverWorkers.Add(1)
		self.activeServers.Add(1)
		go func() {
			defer self.serverWorkers.Done()
			defer self.activeServers.Add(-1)
			defer server.Close()
			stop := context.AfterFunc(ctx, func() { _ = server.Close() })
			defer stop()
			request, err := http.ReadRequest(bufio.NewReader(server))
			if err != nil {
				return
			}
			wire, err := base64.RawURLEncoding.DecodeString(request.URL.Query().Get("dns"))
			if err != nil {
				return
			}
			var parser dnsmessage.Parser
			if _, err := parser.Start(wire); err != nil {
				return
			}
			question, err := parser.Question()
			if err != nil {
				return
			}
			self.queries.Add(1)
			readyType := dnsmessage.TypeA
			if readyIpv6 {
				readyType = dnsmessage.TypeAAAA
			}
			addr := self.readyAddr
			if question.Type != readyType {
				self.heldOnce.Do(func() { close(self.heldEntered) })
				defer self.heldDoneOnce.Do(func() { close(self.heldDone) })
				select {
				case <-self.heldRelease:
				case <-ctx.Done():
					return
				}
				addr = self.lateAddr
			}
			response := httptest.NewRecorder()
			writeDohWire(response, request, []netip.Addr{addr}, 60, false)
			_ = response.Result().Write(server)
		}()
		return client, nil
	}}
	strategySettings := DefaultClientStrategySettings()
	strategySettings.ConnectSettings.Log = NewNoopLogger()
	strategySettings.ConnectSettings.DialContextSettings = &DialContextSettings{
		DialContext: func(dialCtx context.Context, network string, address string) (net.Conn, error) {
			self.feedDials.Add(1)
			self.feedOnce.Do(func() { close(self.feedEntered) })
			<-dialCtx.Done()
			return nil, dialCtx.Err()
		},
		PacketConnFactory: func(context.Context) (net.PacketConn, error) {
			return nil, errors.New("no packet socket in first-answer fixture")
		},
	}
	self.strategy = NewClientStrategy(ctx, strategySettings)
	settings := DefaultExtenderNetworkClientSettings()
	settings.Log = NewNoopLogger()
	settings.Now = clock.Now
	settings.ExtenderDnsName = "bootstrap-first.example"
	settings.ManualHosts = nil
	settings.DohSettings = dohSettings
	settings.HelloTimeout = time.Hour
	settings.DialTimeout = time.Hour
	settings.MinBackoff = time.Hour
	settings.MaxBackoff = time.Hour
	settings.RebootstrapTimeout = time.Hour
	settings.ProbeWindowCount = 0
	settings.IpVersionSupported = func(int) bool { return true }
	settings.Hello = func(context.Context) (*ExtenderHelloResult, error) { return nil, nil }
	settings.Hint = func(context.Context) (string, error) { return "", nil }
	settings.ResolveDnsTxt = func(context.Context, string) ([]string, error) { return nil, nil }
	if mode == "signed" {
		txt := testExtenderDnsRecordTxt(t, rootPrivate, clock, self.readyAddr.String())
		settings.ResolveDnsTxt = func(context.Context, string) ([]string, error) {
			return []string{txt}, nil
		}
	} else if mode == "manual" {
		settings.ExtenderDnsName = ""
		settings.ManualHosts = []string{"manual-first.example"}
	}
	self.client = NewExtenderNetworkClient(ctx, self.strategy, self.directory, settings)
	t.Cleanup(func() {
		cancel()
		self.client.Close()
		self.strategy.Close()
		self.directory.Close()
		self.serverWorkers.Wait()
	})
	return self
}

// A signed/manual feed can start safely while a DNS sibling is still blocked.
func (self *extenderFirstAnswerFixture) requireFeedBeforeHeldFamily(t *testing.T) {
	t.Helper()
	select {
	case <-self.heldEntered:
	case <-self.ctx.Done():
		t.Fatal("the held DNS family never reached its barrier")
	}
	select {
	case <-self.feedEntered:
	case <-self.ctx.Done():
		t.Fatalf("usable extender feed waited for the held DNS family: queries=%d", self.queries.Load())
	}
	if self.ctx.Err() != nil {
		t.Fatal("feed did not start before the dependency-cycle watchdog")
	}
	select {
	case <-self.heldDone:
		t.Fatal("held DNS completed before the real feed dial")
	default:
	}
}

// A monitor notification, not polling or a short sleep, observes late merge.
func (self *extenderFirstAnswerFixture) requireBothAddresses(t *testing.T, source string) {
	t.Helper()
	self.releaseOnce.Do(func() { close(self.heldRelease) })
	for {
		_, changed := self.directory.ChangeMonitor().Get()
		ready, late := false, false
		for _, entry := range self.directory.Snapshot().Entries {
			if entry.Ip == self.readyAddr {
				ready = true
			}
			if entry.Ip == self.lateAddr {
				late = true
				if entry.Source != source || len(entry.PublicKey) != 0 {
					t.Fatalf("late DNS address changed trust/source: source=%s key_bytes=%d", entry.Source, len(entry.PublicKey))
				}
			}
		}
		if ready && late {
			return
		}
		select {
		case <-changed:
		case <-self.ctx.Done():
			t.Fatal("late DNS family was dropped instead of merged")
		}
	}
}

// The verified TXT candidate may dial before the address refresh completes.
func TestExtenderDohFirstAnswerSignedFeedStartsBeforeHeldFamily(t *testing.T) {
	self := newExtenderFirstAnswerFixture(t, false, "signed")
	self.requireFeedBeforeHeldFamily(t)
	entry := testDirectoryEntry(t, self.directory, self.readyAddr)
	if len(entry.PublicKey) == 0 {
		t.Fatal("signed TXT lost its verified key")
	}
	self.requireBothAddresses(t, ExtenderSourceDns)
}

// Manual configuration is the existing explicit trust authority, not DNS.
func TestExtenderDohFirstAnswerManualIpv4StartsBeforeHeldIpv6(t *testing.T) {
	self := newExtenderFirstAnswerFixture(t, false, "manual")
	self.requireFeedBeforeHeldFamily(t)
	self.requireBothAddresses(t, ExtenderSourceManual)
}

// Ready AAAA must also bypass a held A; the old serial resolver never starts it.
func TestExtenderDohFirstAnswerManualIpv6StartsBeforeHeldIpv4(t *testing.T) {
	self := newExtenderFirstAnswerFixture(t, true, "manual")
	self.requireFeedBeforeHeldFamily(t)
	self.requireBothAddresses(t, ExtenderSourceManual)
}

// Unsigned DNS can release the startup attempt and fill inventory, but cannot
// turn its unverified address into permission to dial an extender.
func TestExtenderDohFirstAnswerUnsignedBootstrapRetainsTrust(t *testing.T) {
	self := newExtenderFirstAnswerFixture(t, false, "unsigned")
	for {
		status, changed := self.client.StatusMonitor().Get()
		if status.InitialAttemptDone {
			if self.ctx.Err() != nil {
				t.Fatal("initial attempt waited for the dependency-cycle watchdog")
			}
			break
		}
		select {
		case <-changed:
		case <-self.ctx.Done():
			t.Fatal("unsigned first publication did not release the initial attempt before sibling completion")
		}
	}
	if self.feedDials.Load() != 0 || self.directory.AddressUsable(self.readyAddr) {
		t.Fatal("unsigned DNS acquired feed authority")
	}
	if entry := testDirectoryEntry(t, self.directory, self.readyAddr); len(entry.PublicKey) != 0 || entry.Source != ExtenderSourceDns {
		t.Fatal("unsigned first DNS address changed trust")
	}
	self.requireBothAddresses(t, ExtenderSourceDns)
}

// Closing during the held sibling must join all one-shot queries and their
// synthetic HTTP handlers, not leave a late publisher behind the closed client.
func TestExtenderDohFirstAnswerCloseJoinsHeldFamily(t *testing.T) {
	self := newExtenderFirstAnswerFixture(t, false, "manual")
	self.requireFeedBeforeHeldFamily(t)
	self.cancel()
	self.client.Close()
	self.serverWorkers.Wait()
	if active := self.activeServers.Load(); active != 0 {
		t.Fatalf("Close left %d DNS handlers alive", active)
	}
	entries := self.directory.Snapshot().Entries
	for _, entry := range entries {
		if entry.Ip == self.lateAddr {
			t.Fatal("canceled late query published an address")
		}
	}
}

// A fully answered custom resolver is an unchanged control, including its
// explicit manual trust source and configured destination.
func TestExtenderDohFirstAnswerCustomResolverControl(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	settings := DefaultExtenderNetworkClientSettings()
	settings.Log = NewNoopLogger()
	settings.ExtenderDnsName = ""
	settings.ManualHosts = []string{"custom-first.example"}
	settings.ProbeWindowCount = 0
	settings.HelloTimeout = 5 * time.Second
	settings.MinBackoff = time.Hour
	settings.MaxBackoff = time.Hour
	settings.IpVersionSupported = func(int) bool { return true }
	settings.ResolveDns = func(ctx context.Context, name string) ([]netip.Addr, error) {
		if name != "custom-first.example" {
			return nil, fmt.Errorf("wrong synthetic name")
		}
		return []netip.Addr{netip.MustParseAddr("192.0.2.123")}, nil
	}
	directory := NewExtenderDirectory(ctx, DefaultExtenderDirectorySettings())
	defer directory.Close()
	strategy := newTestBlockingDialStrategy(t, ctx)
	client := NewExtenderNetworkClient(ctx, strategy, directory, settings)
	defer client.Close()
	for {
		status, changed := client.StatusMonitor().Get()
		if status.Connecting {
			break
		}
		select {
		case <-changed:
		case <-time.After(5 * time.Second):
			t.Fatal("healthy custom resolver did not reach feed")
		}
	}
	if entry := testDirectoryEntry(t, directory, netip.MustParseAddr("192.0.2.123")); entry.Source != ExtenderSourceManual {
		t.Fatal("custom resolver policy changed")
	}
}

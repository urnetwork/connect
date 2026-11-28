// The DNS-family barrier must not prevent a usable tunnel TCP family from starting.
package connect

import (
	"context"
	"encoding/base64"
	"errors"
	"net"
	"net/http"
	"net/netip"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/net/dns/dnsmessage"
)

// Owns two real tunnel stacks and a wire-format DoH server reached through them.
// The held query can finish only after the application TCP connection starts.
type dohProgressTunFixture struct {
	ctx                   context.Context
	left                  *Tun
	dial                  DialContextFunction
	right                 *Tun
	host                  string
	port                  string
	readyVersion          int
	heldEntered           chan struct{}
	applicationAccepted   chan struct{}
	applicationAcceptOnce sync.Once
	hold                  atomic.Bool
	readyQueries          atomic.Int64
	heldQueries           atomic.Int64
	heldFinished          chan struct{}
	heldFinishedOnce      sync.Once
	serverError           chan error
}

// Constructs the real remote-only resolver path without contacting host DNS or the internet.
func newDohProgressTunFixture(t *testing.T, readyVersion int) *dohProgressTunFixture {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	self := &dohProgressTunFixture{
		ctx:                 ctx,
		host:                "lookup-barrier.example",
		readyVersion:        readyVersion,
		heldEntered:         make(chan struct{}),
		applicationAccepted: make(chan struct{}),
		heldFinished:        make(chan struct{}),
		serverError:         make(chan error, 8),
	}
	t.Cleanup(cancel)
	settings := tunTestSettings(6)
	settings.Log = NewNoopLogger()
	var err error
	self.right, err = CreateTunWithResolver(ctx, settings, &DnsResolverSettings{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = self.right.Close() })
	readyAddr := tunTestLocalAddress(t, self.right, readyVersion)
	applicationListener, err := self.right.ListenTCP(&net.TCPAddr{IP: net.IP(readyAddr.AsSlice())})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = applicationListener.Close() })
	self.port = itoa(applicationListener.Addr().(*net.TCPAddr).Port)
	applicationDone := make(chan struct{})
	go func() {
		defer close(applicationDone)
		for {
			conn, err := applicationListener.Accept()
			if err != nil {
				return
			}
			self.applicationAcceptOnce.Do(func() { close(self.applicationAccepted) })
			_ = conn.Close()
		}
	}()
	t.Cleanup(func() {
		_ = applicationListener.Close()
		<-applicationDone
	})

	resolverListener, err := self.right.ListenTCP(&net.TCPAddr{IP: net.IP(tunTestLocalAddress(t, self.right, 4).AsSlice())})
	if err != nil {
		t.Fatal(err)
	}
	var heldEnteredOnce sync.Once
	resolverServer := &http.Server{
		Handler: http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			wire, err := base64.RawURLEncoding.DecodeString(request.URL.Query().Get("dns"))
			if err != nil {
				self.serverError <- err
				writer.WriteHeader(http.StatusBadRequest)
				return
			}
			var parser dnsmessage.Parser
			if _, err := parser.Start(wire); err != nil {
				self.serverError <- err
				writer.WriteHeader(http.StatusBadRequest)
				return
			}
			question, err := parser.Question()
			if err != nil {
				self.serverError <- err
				writer.WriteHeader(http.StatusBadRequest)
				return
			}
			ready := question.Type == dnsmessage.TypeA
			if self.readyVersion == 6 {
				ready = question.Type == dnsmessage.TypeAAAA
			}
			if ready {
				self.readyQueries.Add(1)
				if self.hold.Load() {
					select {
					case <-self.heldEntered:
					case <-request.Context().Done():
						return
					}
				}
				writeDohWire(writer, request, []netip.Addr{readyAddr}, 60, false)
				return
			}
			self.heldQueries.Add(1)
			if self.hold.Load() {
				heldEnteredOnce.Do(func() { close(self.heldEntered) })
				defer self.heldFinishedOnce.Do(func() { close(self.heldFinished) })
				select {
				case <-self.applicationAccepted:
				case <-request.Context().Done():
					return
				}
			}
			writeDohWire(writer, request, nil, 60, false)
		}),
	}
	serverDone := make(chan struct{})
	go func() { defer close(serverDone); _ = resolverServer.Serve(resolverListener) }()
	t.Cleanup(func() {
		_ = resolverServer.Close()
		<-serverDone
	})
	resolver := &DnsResolverSettings{
		EnableRemoteDoh:   true,
		RemoteDohUrlsIpv4: []string{"http://" + resolverListener.Addr().String() + "/dns-query"},
	}
	self.left, err = CreateTunWithResolver(ctx, settings, resolver)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = self.left.Close() })
	self.dial = self.left.dialContext

	var bridgeWorkers sync.WaitGroup
	bridge := func(destination *Tun, source *Tun) {
		bridgeWorkers.Add(1)
		go func() {
			defer bridgeWorkers.Done()
			for {
				packet, err := source.Read()
				if err != nil {
					return
				}
				_, _ = destination.Write(packet)
				MessagePoolReturn(packet)
			}
		}()
	}
	bridge(self.left, self.right)
	bridge(self.right, self.left)
	t.Cleanup(func() {
		cancel()
		_ = self.left.Close()
		_ = self.right.Close()
		bridgeWorkers.Wait()
	})
	return self
}

// Proves the dependency cycle using a real query, not a short negative wait.
func (self *dohProgressTunFixture) dialHeldFamily(t *testing.T, readyCached bool) {
	t.Helper()
	if readyCached {
		recordType := "A"
		if self.readyVersion == 6 {
			recordType = "AAAA"
		}
		addrs, authoritative := self.left.DohCache().QueryResult(self.ctx, recordType, self.host)
		if !authoritative || len(addrs) != 1 {
			t.Fatal("failed to populate ready-family cache")
		}
	}
	self.hold.Store(true)
	if readyCached {
		// Establish the missing family's flight before a cached IPv6 winner
		// can cancel it prior to launch. The dial must join, not duplicate it.
		recordType := "AAAA"
		if self.readyVersion == 6 {
			recordType = "A"
		}
		heldQueryDone := make(chan struct{})
		go func() {
			defer close(heldQueryDone)
			self.left.DohCache().QueryResult(self.ctx, recordType, self.host)
		}()
		defer func() { <-heldQueryDone }()
		select {
		case <-self.heldEntered:
		case <-self.ctx.Done():
			t.Fatal("uncached family did not enter its explicit barrier")
		}
	}
	conn, err := self.dial(self.ctx, "tcp", net.JoinHostPort(self.host, self.port))
	if err != nil {
		readyCached := func() bool {
			cache := self.left.DohCache()
			cache.stateLock.Lock()
			defer cache.stateLock.Unlock()
			recordType := "A"
			if self.readyVersion == 6 {
				recordType = "AAAA"
			}
			answer := cache.queryResultExpiration[NewDohKey(recordType, self.host)]
			return answer != nil && len(answer.Addrs()) == 1
		}()
		t.Fatalf("usable family stranded behind held DNS: ready_answer_cached=%t ready_queries=%d held_queries=%d dial_error=%v",
			readyCached, self.readyQueries.Load(), self.heldQueries.Load(), err)
	}
	_ = conn.Close()
	select {
	case <-self.applicationAccepted:
	case <-self.ctx.Done():
		t.Fatal("dial returned without application accept")
	}
	if self.readyQueries.Load() != 1 || self.heldQueries.Load() != 1 {
		t.Fatalf("custom resolver query counts ready=%d held=%d, want one each", self.readyQueries.Load(), self.heldQueries.Load())
	}
	select {
	case <-self.heldFinished:
	case <-self.ctx.Done():
		t.Fatal("held query did not finish after application progress or cancellation")
	}
	select {
	case err := <-self.serverError:
		t.Fatal(err)
	default:
	}
}

// A cold usable A answer must make progress even when AAAA waits for that progress.
func TestTunDohProgressReadyIpv4HeldIpv6(t *testing.T) {
	newDohProgressTunFixture(t, 4).dialHeldFamily(t, false)
}

// A cold usable AAAA answer must make progress even when A waits for that progress.
func TestTunDohProgressReadyIpv6HeldIpv4(t *testing.T) {
	newDohProgressTunFixture(t, 6).dialHeldFamily(t, false)
}

// Warming only A cannot make an unrelated pending AAAA a prerequisite.
func TestTunDohProgressCachedIpv4HeldIpv6(t *testing.T) {
	newDohProgressTunFixture(t, 4).dialHeldFamily(t, true)
}

// Warming only AAAA cannot make an unrelated pending A a prerequisite.
func TestTunDohProgressCachedIpv6HeldIpv4(t *testing.T) {
	newDohProgressTunFixture(t, 6).dialHeldFamily(t, true)
}

// Protected control names use the same progress rule, while their underlying
// dialer still sees only the caller-configured resolver's literal answers.
func TestInternalDohProgressReadyFamilyBypassesHeldFamily(t *testing.T) {
	for _, readyVersion := range []int{4, 6} {
		self := newDohProgressTunFixture(t, readyVersion)
		resolver := &internalDohResolver{cache: self.left.DohCache(), domains: []string{self.host}}
		self.dial = resolver.wrapDialContext(self.left.dialContext)
		self.dialHeldFamily(t, false)
	}
}

// A fully cached positive/negative pair is the adjacent healthy control.
func TestTunDohProgressBothFamiliesCachedControl(t *testing.T) {
	self := newDohProgressTunFixture(t, 4)
	for _, recordType := range []string{"A", "AAAA"} {
		_, authoritative := self.left.DohCache().QueryResult(self.ctx, recordType, self.host)
		if !authoritative {
			t.Fatal("failed to populate paired cache entries")
		}
	}
	self.hold.Store(true)
	conn, err := self.left.dialContext(self.ctx, "tcp", net.JoinHostPort(self.host, self.port))
	if err != nil {
		t.Fatal(err)
	}
	_ = conn.Close()
	if self.readyQueries.Load() != 1 || self.heldQueries.Load() != 1 {
		t.Fatal("cached dial re-queried the custom resolver")
	}
	if errors.Is(self.ctx.Err(), context.DeadlineExceeded) {
		t.Fatal("healthy cache control exhausted its watchdog")
	}
}

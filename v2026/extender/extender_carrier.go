package extender

import (
	"errors"
	"fmt"
	"log"
	mathrand "math/rand/v2"
	"net"
	"slices"
	"syscall"
	"time"

	"github.com/urnetwork/connect/v2026"
)

// Keeping the carriers up once they have bound (EXTENDER.md G2).
//
// A carrier that fails must not take the server down with it. One accept error
// or one udp carrier's exit used to close every carrier for the life of the
// process, while the provider stayed connected and the activation loop kept
// posting for a listener that was gone. That is probably what one operator's
// extenders showed on 2026-11-01, tcp 443 and udp 53 refused with the process up,
// though no host's log confirms it and which failure it was is not known.
//
// So each bound port is supervised on its own. An accept error that is a passing
// condition of the host is waited out on the same listener, as net/http does.
// Anything else ends that one port, which is bound again on a backoff with
// jitter while the others keep serving. Carriers says what is serving at every
// moment, so nothing is offered, and no activation is posted, for a listener that
// is gone. The server itself ends only with Close or its context.

// The least time between two log lines of an accept error that keeps being
// retried. It fails the same way every retry, so one line an interval, with the
// count since, says as much as one line a retry.
const extenderAcceptErrorLogInterval = 1 * time.Minute

var errExtenderServerClosing = errors.New("the extender server is closing")

// One port of one carrier. Its `serving` is guarded by the server's state lock.
type extenderCarrierPort struct {
	carrier string
	port    int
	serving bool
}

// Binds one tcp carrier port and adds the listener to what Close interrupts.
func (self *ExtenderServer) bindTcpCarrier(port int) (net.Listener, *extenderOwnedListener, error) {
	log.Printf("[extender] listen tcp %d", port)
	listener, err := self.listenTcp(fmt.Sprintf(":%d", port))
	if err != nil {
		// Ownership transfers for every non-nil callback result, even when
		// the callback rejects that result with an error.
		if listener != nil {
			listener.Close()
		}
		return nil, nil, err
	}
	if listener == nil {
		return nil, nil, fmt.Errorf("extender listener factory returned nil")
	}
	ownedListener, ok := self.addListener(listener)
	if !ok {
		return nil, nil, errExtenderServerClosing
	}
	return listener, ownedListener, nil
}

// Binds one udp carrier port and adds the endpoint to what Close interrupts.
func (self *ExtenderServer) bindUdpCarrier(
	port int,
	connectMode connect.ExtenderConnectMode,
) (net.PacketConn, *extenderOwnedCloser, error) {
	log.Printf("[extender] listen udp %d (%s)", port, connectMode)
	listenPacket := net.ListenPacket
	if self.settings.ListenPacket != nil {
		listenPacket = self.settings.ListenPacket
	}
	packetConn, err := listenPacket("udp", fmt.Sprintf(":%d", port))
	if err != nil {
		if packetConn != nil {
			packetConn.Close()
		}
		return nil, nil, err
	}
	if packetConn == nil {
		return nil, nil, fmt.Errorf("extender packet listener factory returned nil")
	}
	ownedCloser, ok := self.addCloser(func() { packetConn.Close() })
	if !ok {
		return nil, nil, errExtenderServerClosing
	}
	return packetConn, ownedCloser, nil
}

// Serves one tcp carrier port for the life of the server, from the listener
// ListenAndServe bound.
func (self *ExtenderServer) superviseTcpCarrier(
	port int,
	listener net.Listener,
	ownedListener *extenderOwnedListener,
) {
	carrierPort := &extenderCarrierPort{
		carrier: connect.ExtenderCarrierTcp,
		port:    port,
		serving: true,
	}
	serveOn := func(listener net.Listener, ownedListener *extenderOwnedListener) func() error {
		return func() error {
			defer func() {
				listener.Close()
				self.removeListener(ownedListener)
			}()
			return self.serveTcpCarrier(carrierPort, listener)
		}
	}
	self.superviseCarrier(carrierPort, serveOn(listener, ownedListener), func() (func() error, error) {
		listener, ownedListener, err := self.bindTcpCarrier(port)
		if err != nil {
			return nil, err
		}
		return serveOn(listener, ownedListener), nil
	})
}

// Serves one udp carrier port for the life of the server, from the endpoint
// ListenAndServe bound.
func (self *ExtenderServer) superviseUdpCarrier(
	port int,
	connectMode connect.ExtenderConnectMode,
	packetConn net.PacketConn,
	ownedCloser *extenderOwnedCloser,
) {
	carrierPort := &extenderCarrierPort{
		carrier: connect.ExtenderCarrierForConnectMode(connectMode),
		port:    port,
		serving: true,
	}
	serveOn := func(packetConn net.PacketConn, ownedCloser *extenderOwnedCloser) func() error {
		return func() error {
			defer func() {
				ownedCloser.close()
				self.removeCloser(ownedCloser)
			}()
			return self.serveQuicCarrier(connectMode, packetConn)
		}
	}
	self.superviseCarrier(carrierPort, serveOn(packetConn, ownedCloser), func() (func() error, error) {
		packetConn, ownedCloser, err := self.bindUdpCarrier(port, connectMode)
		if err != nil {
			return nil, err
		}
		return serveOn(packetConn, ownedCloser), nil
	})
}

// Runs `serve`, and each time it ends under a live server reports the port lost,
// binds it again through `rebind` on a backoff with jitter, and reports it back.
// `serve` releases its socket when it returns, so the port is free to bind again.
func (self *ExtenderServer) superviseCarrier(
	carrierPort *extenderCarrierPort,
	serve func() error,
	rebind func() (func() error, error),
) {
	settings := self.settings
	minBackoff := positiveDurationOr(settings.CarrierRebuildMinBackoff, 1*time.Second)
	maxBackoff := max(minBackoff, positiveDurationOr(settings.CarrierRebuildMaxBackoff, 5*time.Minute))
	stableTimeout := positiveDurationOr(settings.CarrierRebuildStableTimeout, 5*time.Minute)
	backoff := minBackoff

	for {
		serveTime := time.Now()
		err := runCarrierStep(serve)
		if self.ctx.Err() != nil {
			return
		}
		if err == nil {
			err = errors.New("the carrier stopped serving")
		}
		connect.DefaultLogger().Errorf(
			"[extender] the %s carrier on port %d ended and will be rebuilt: %s\n",
			carrierPort.carrier,
			carrierPort.port,
			err,
		)
		self.carrierPortLost(carrierPort, err)
		if stableTimeout <= time.Since(serveTime) {
			backoff = minBackoff
		}

		bindError := ""
		for {
			select {
			case <-self.ctx.Done():
				return
			case <-time.After(extenderCarrierRebuildWait(backoff)):
			}
			backoff = min(2*backoff, maxBackoff)

			bindErr := runCarrierStep(func() error {
				var err error
				serve, err = rebind()
				return err
			})
			if errors.Is(bindErr, errExtenderServerClosing) || self.ctx.Err() != nil {
				return
			}
			if bindErr == nil {
				break
			}
			// each new reason once, not every retry
			if bindError != bindErr.Error() {
				bindError = bindErr.Error()
				connect.DefaultLogger().Errorf(
					"[extender] the %s carrier on port %d did not bind again: %s\n",
					carrierPort.carrier,
					carrierPort.port,
					bindErr,
				)
				self.carrierPortBindFailed(carrierPort, bindErr)
			}
		}
		connect.DefaultLogger().Infof(
			"[extender] the %s carrier on port %d is serving again\n",
			carrierPort.carrier,
			carrierPort.port,
		)
		self.carrierPortServing(carrierPort)
	}
}

// Runs one step of a carrier's supervision, reading a panic as what ended it, so
// that nothing a carrier does reaches the other carriers. A panic is logged with
// its stack by HandleError.
func runCarrierStep(step func() error) (err error) {
	if r := connect.HandleError(func() {
		err = step()
	}); r != nil {
		return fmt.Errorf("the carrier panicked: %v", r)
	}
	return err
}

// A wait of about `backoff`, uniform over half to one and a half of it.
func extenderCarrierRebuildWait(backoff time.Duration) time.Duration {
	if backoff <= 0 {
		return 0
	}
	return backoff/2 + time.Duration(mathrand.Int64N(int64(backoff)))
}

func positiveDurationOr(value time.Duration, otherwise time.Duration) time.Duration {
	if 0 < value {
		return value
	}
	return otherwise
}

// Accepts on one tcp carrier port until the listener ends. An error that is a
// passing condition of the host is waited out on this listener: closing it would
// drop what is in its backlog, and could not help a process out of file
// descriptors, which could not bind another. While such errors go on for
// AcceptDownTimeout the port is reported lost, since it serves nothing however
// bound it is. It is back with its next accepted connection, or once an accept
// has blocked for longer than the retries wait, since an accept that blocks has
// nothing failing it. It must not need the connection: a revoked extender gets no
// uptime probes, and it posts its re-activation only once the port is back.
//
// The caller closes the listener. Nil means the server is ending.
func (self *ExtenderServer) serveTcpCarrier(
	carrierPort *extenderCarrierPort,
	listener net.Listener,
) error {
	settings := self.settings
	minBackoff := positiveDurationOr(settings.AcceptRetryMinBackoff, 5*time.Millisecond)
	maxBackoff := max(minBackoff, positiveDurationOr(settings.AcceptRetryMaxBackoff, 1*time.Second))
	downTimeout := positiveDurationOr(settings.AcceptDownTimeout, 30*time.Second)

	healedTimeout := 2 * maxBackoff

	var backoff time.Duration
	var failingSince time.Time
	var logTime time.Time
	suppressedCount := 0
	reportedLost := false
	// armed around an accept while the port is reported lost, and stopped however
	// that accept ends, a panic included
	var healedTimer *time.Timer
	defer func() {
		if healedTimer != nil {
			healedTimer.Stop()
		}
	}()
	for {
		select {
		case <-self.ctx.Done():
			return nil
		default:
		}

		var healed chan struct{}
		if reportedLost {
			healed = make(chan struct{})
			healedTimer = time.AfterFunc(healedTimeout, func() {
				defer close(healed)
				self.carrierPortServing(carrierPort)
			})
		}
		acceptTime := time.Now()
		conn, err := listener.Accept()
		if healedTimer != nil {
			if !healedTimer.Stop() {
				// what it reported is ordered before what this accept's answer does
				<-healed
			}
			healedTimer = nil
		}
		if err != nil {
			if self.ctx.Err() != nil {
				return nil
			}
			if !extenderAcceptErrorIsTransient(err) {
				return err
			}
			now := time.Now()
			if failingSince.IsZero() || healedTimeout <= now.Sub(acceptTime) {
				failingSince = now
			}
			if extenderAcceptErrorLogInterval <= now.Sub(logTime) {
				connect.DefaultLogger().Errorf(
					"[extender] accept on tcp port %d failed, %d more since the last line, retrying: %s\n",
					carrierPort.port,
					suppressedCount,
					err,
				)
				logTime = now
				suppressedCount = 0
			} else {
				suppressedCount += 1
			}
			if downTimeout <= now.Sub(failingSince) {
				self.carrierPortLost(carrierPort, err)
				reportedLost = true
			}
			backoff = min(max(2*backoff, minBackoff), maxBackoff)
			select {
			case <-self.ctx.Done():
				return nil
			case <-time.After(backoff):
			}
			continue
		}
		backoff = 0
		failingSince = time.Time{}
		if reportedLost {
			reportedLost = false
			self.carrierPortServing(carrierPort)
		}
		self.startConnection(conn)
	}
}

// The errors accept(2) leaves on the listener for a moment that are not in what
// Go reads as temporary: a connection that went away while it queued, which the
// net package reads as temporary only unwrapped; memory; and the network errors
// already pending on the new socket, which the Linux man page says to treat like
// EAGAIN.
var extenderAcceptTransientErrors = []error{
	syscall.ECONNABORTED,
	syscall.ECONNRESET,
	syscall.ENOBUFS,
	syscall.ENOMEM,
	syscall.EPROTO,
	syscall.EPERM,
	syscall.ENETDOWN,
	syscall.ENETUNREACH,
	syscall.EHOSTDOWN,
	syscall.EHOSTUNREACH,
	syscall.ENOPROTOOPT,
	syscall.EOPNOTSUPP,
	// the same conditions as winsock numbers them. On windows the names above are
	// values of syscall's own that no socket returns, and on any other system
	// nothing returns these
	syscall.Errno(10004), // WSAEINTR
	syscall.Errno(10024), // WSAEMFILE
	syscall.Errno(10050), // WSAENETDOWN
	syscall.Errno(10051), // WSAENETUNREACH
	syscall.Errno(10052), // WSAENETRESET
	syscall.Errno(10053), // WSAECONNABORTED
	syscall.Errno(10054), // WSAECONNRESET
	syscall.Errno(10055), // WSAENOBUFS
	syscall.Errno(10060), // WSAETIMEDOUT
	syscall.Errno(10064), // WSAEHOSTDOWN
	syscall.Errno(10065), // WSAEHOSTUNREACH
}

// Whether an accept error is a passing condition of the host rather than the end
// of the listener: EMFILE, ENFILE, ECONNABORTED and the rest of what net/http
// retries, and the errors above. Anything else, a closed listener first of all,
// ends the port, which is then bound again.
func extenderAcceptErrorIsTransient(err error) bool {
	if errors.Is(err, net.ErrClosed) {
		return false
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Temporary() {
		return true
	}
	return slices.ContainsFunc(extenderAcceptTransientErrors, func(transient error) bool {
		return errors.Is(err, transient)
	})
}

// Takes one port out of what the server serves, with the error that ended it,
// and tells the owner. Does nothing when it is already out or the server is
// closing, which is what Close already said.
func (self *ExtenderServer) carrierPortLost(carrierPort *extenderCarrierPort, err error) {
	changed := func() bool {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if self.closing || !carrierPort.serving {
			return false
		}
		carrierPort.serving = false
		self.carrierPortCounts[carrierPort.carrier] -= 1
		if self.carrierPortCounts[carrierPort.carrier] <= 0 {
			delete(self.carrierPortCounts, carrierPort.carrier)
			self.carriers = slices.DeleteFunc(self.carriers, func(carrier string) bool {
				return carrier == carrierPort.carrier
			})
			self.carrierListenErrs[carrierPort.carrier] = err
		}
		if carrierPort.carrier == connect.ExtenderCarrierDns {
			self.dnsPorts = slices.DeleteFunc(self.dnsPorts, func(port int) bool {
				return port == carrierPort.port
			})
		}
		return true
	}()
	if !changed {
		return
	}
	self.notifyListenError(carrierPort, err)
	self.notifyCarriersChange()
}

// Records why a lost port could not be bound again, which replaces what ended it
// in ListenErrors while the carrier has no other port serving.
func (self *ExtenderServer) carrierPortBindFailed(carrierPort *extenderCarrierPort, err error) {
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if self.carrierPortCounts[carrierPort.carrier] <= 0 {
			self.carrierListenErrs[carrierPort.carrier] = err
		}
	}()
	self.notifyListenError(carrierPort, err)
}

// Puts a lost port back in what the server serves, and tells the owner.
func (self *ExtenderServer) carrierPortServing(carrierPort *extenderCarrierPort) {
	changed := func() bool {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if self.closing || carrierPort.serving {
			return false
		}
		carrierPort.serving = true
		self.addCarrierWithLock(carrierPort.carrier)
		if carrierPort.carrier == connect.ExtenderCarrierDns {
			self.addDnsPortWithLock(carrierPort.port)
		}
		return true
	}()
	if changed {
		self.notifyCarriersChange()
	}
}

// Tells the owner why a carrier is down. Whatever its handler does ends here: a
// panic in it is logged by HandleError and costs the owner the news, not this port
// its supervision, which is on this goroutine.
func (self *ExtenderServer) notifyListenError(carrierPort *extenderCarrierPort, err error) {
	if self.settings.ListenErrorHandler != nil {
		connect.HandleError(func() {
			self.settings.ListenErrorHandler(carrierPort.carrier, err)
		})
	}
}

// Tells the owner the carriers changed, as notifyListenError does.
func (self *ExtenderServer) notifyCarriersChange() {
	if self.settings.CarriersChangeHandler != nil {
		connect.HandleError(self.settings.CarriersChangeHandler)
	}
}

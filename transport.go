package connect

import (
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"math"
	mathrand "math/rand"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	// "runtime/debug"

	"maps"

	"github.com/gorilla/websocket"
	quic "github.com/quic-go/quic-go"

	"github.com/urnetwork/connect/protocol"
)

// note that it is possible to have multiple transports for the same client destination
// e.g. platform, p2p, and a bunch of extenders

// extenders are identified and credited with the platform by ip address
// they forward to a special port, 8443, that whitelists their ip without rate limiting
// when an extender gets an http message from a client, it always connects tcp to connect.bringyour.com:8443
// appends the proxy protocol headers, and then forwards the bytes from the client
// https://docs.nginx.com/nginx/admin-guide/load-balancer/using-proxy-protocol/
// rate limit using $proxy_protocol_addr https://www.nginx.com/blog/rate-limiting-nginx/
// add the source ip as the X-Extender header

// the transport attempts to upgrade from http1 to http3
// versus the h1 transport, h3 is:
// - more cpu efficient.
//   The quic stream does not need to mask/unmask each byte before TLS.
// - better throughput on poor networks.
//   quic optimizes congestion control to better handle poor network conditions.
// However, h3 is not available in all locations due to dpi/filtering.
// When available, it takes precedence over the default transport.

// packet translation mode gives options for how udp packets are formed on the wire
// We include options here that are known to help with availability

// When packet translation is set, the upgrade mode must be h3 only

// 1: initial version
// 2: latency and speed test support
const TransportVersion = 2

// turn this on to be extra careful about returning all messages
// note we don't run this because it's most efficient to let the gc handle some infrequent orphaned messages
const DebugCloseSend = false

type TransportControl = byte

const (
	TransportControlSpeedStart TransportControl = 1
	TransportControlSpeedStop  TransportControl = 2
)

type TransportMode string

// in order of increasing preference
const (
	// start all modes in skewed parallel and choose the best one
	TransportModeAuto      TransportMode = "auto"
	TransportModeH3DnsPump TransportMode = "h3dnspump"
	TransportModeH3Dns     TransportMode = "h3dns"
	TransportModeH1        TransportMode = "h1"
	TransportModeH3        TransportMode = "h3"
	TransportModeNone      TransportMode = ""
)

type ClientAuth struct {
	ByJwt string
	// ClientId Id
	InstanceId Id
	AppVersion string
}

func (self *ClientAuth) ClientId() (Id, error) {
	byJwt, err := ParseByJwtUnverified(self.ByJwt)
	if err != nil {
		return Id{}, err
	}
	return byJwt.ClientId, nil
}

// (ctx, network, address)
// type DialContextFunc func(ctx context.Context, network string, address string) (net.Conn, error)

type PlatformTransportSettings struct {
	// Log, when set, is used by the platform transport and its framer
	// (propagated to `FramerSettings.Log` when nil).
	// nil resolves to `DefaultLogger()`.
	Log Logger

	HttpConnectTimeout   time.Duration
	WsHandshakeTimeout   time.Duration
	QuicConnectTimeout   time.Duration
	QuicHandshakeTimeout time.Duration
	QuicTlsConfig        *tls.Config
	AuthTimeout          time.Duration
	ReconnectTimeout     time.Duration
	PingTimeout          time.Duration
	WriteTimeout         time.Duration
	ReadTimeout          time.Duration
	TransportGenerator   func() (sendTransport Transport, receiveTransport Transport)
	TransportBufferSize  int
	InactiveDrainTimeout time.Duration
	// it smoothes out the h3 transition to not start/stop h1 if h3 connects in this time
	ModeInitialDelay time.Duration

	// MinConnectDelay time.Duration
	// MaxConnectDelay time.Duration

	ProtocolVersion int

	H3Port  int
	DnsPort int

	// FIXME
	DnsTlds        [][]byte
	V2H1Auth       bool
	FramerSettings *FramerSettings

	PtDnsSlowMultiple int
}

func DefaultPlatformTransportSettings() *PlatformTransportSettings {
	tlsConfig, err := DefaultTlsConfig()
	if err != nil {
		panic(err)
	}
	return &PlatformTransportSettings{
		HttpConnectTimeout:   15 * time.Second,
		WsHandshakeTimeout:   15 * time.Second,
		QuicConnectTimeout:   15 * time.Second,
		QuicHandshakeTimeout: 15 * time.Second,
		QuicTlsConfig:        tlsConfig,
		AuthTimeout:          5 * time.Second,
		ReconnectTimeout:     5 * time.Second,
		PingTimeout:          5 * time.Second,
		WriteTimeout:         10 * time.Second,
		ReadTimeout:          30 * time.Second,
		TransportBufferSize:  32,
		InactiveDrainTimeout: 30 * time.Second,
		ModeInitialDelay:     2 * time.Second,
		// MinConnectDelay:      0,
		// MaxConnectDelay:      1 * time.Second,
		ProtocolVersion: DefaultProtocolVersion,
		H3Port:          443,
		DnsPort:         53,
		// FIXME
		DnsTlds: [][]byte{[]byte("ur.xyz.")},
		// servers are migrated on 2025-06-12. We can remove this and always use true.
		V2H1Auth: true,
		// the platform transport must carry the per-peer encryption handshake,
		// so its framer max is the connect runtime minimum message length
		FramerSettings:    DefaultFramerSettings(int(DefaultClientSettings().MinimumMessageLenLimit())),
		PtDnsSlowMultiple: 4,
	}
}

type PlatformTransport struct {
	ctx    context.Context
	cancel context.CancelFunc
	log    Logger

	clientStrategy *ClientStrategy
	routeManager   *RouteManager

	platformUrl string
	auth        *ClientAuth

	settings *PlatformTransportSettings

	stateLock sync.Mutex
	// notified when availableModes changes. availableModes is a map, so it
	// cannot be a MonitorValue; the notify is issued inside the same locked
	// scope as the mutation (see setModeAvailable)
	availableModeMonitor *Monitor
	availableModes       map[TransportMode]bool
	targetMode           TransportMode
	// the elected active mode, watched by every transport's mode gate and
	// inactive-drain watchdog. a MonitorValue so the mutation cannot be
	// separated from its notification, and so re-electing the same mode does
	// not wake the election loop's own watchers
	mode *MonitorValue[TransportMode]

	// the number of connections with routes currently registered on the route
	// manager. 0 < count means the transport is carrying (or able to carry)
	// traffic. Used by make-before-break migration to wait for a replacement
	// transport to come up before closing the old one (CONNECTDRAIN2.md §3.3)
	registeredCount  atomic.Int64
	connectedMonitor *Monitor
}

// IsConnected reports whether the transport has a connection with routes
// registered on the route manager
func (self *PlatformTransport) IsConnected() bool {
	return 0 < self.registeredCount.Load()
}

// ConnectedNotify returns a channel that closes on the next connect state
// change. Capture the channel before checking `IsConnected`.
func (self *PlatformTransport) ConnectedNotify() <-chan struct{} {
	return self.connectedMonitor.NotifyChannel()
}

func (self *PlatformTransport) setRegistered(registered bool) {
	if registered {
		self.registeredCount.Add(1)
	} else {
		self.registeredCount.Add(-1)
	}
	self.connectedMonitor.NotifyAll()
}

func NewPlatformTransportWithDefaults(
	ctx context.Context,
	clientStrategy *ClientStrategy,
	routeManager *RouteManager,
	platformUrl string,
	auth *ClientAuth,
) *PlatformTransport {
	return NewPlatformTransport(
		ctx,
		clientStrategy,
		routeManager,
		platformUrl,
		auth,
		DefaultPlatformTransportSettings(),
	)
}

func NewPlatformTransport(
	ctx context.Context,
	clientStrategy *ClientStrategy,
	routeManager *RouteManager,
	platformUrl string,
	auth *ClientAuth,
	settings *PlatformTransportSettings,
) *PlatformTransport {
	return NewPlatformTransportWithTargetMode(
		ctx,
		clientStrategy,
		routeManager,
		platformUrl,
		auth,
		TransportModeAuto,
		settings,
	)
}

func NewPlatformTransportWithTargetMode(
	ctx context.Context,
	clientStrategy *ClientStrategy,
	routeManager *RouteManager,
	platformUrl string,
	auth *ClientAuth,
	targetMode TransportMode,
	settings *PlatformTransportSettings,
) *PlatformTransport {
	cancelCtx, cancel := context.WithCancel(ctx)
	log := loggerOrDefault(settings.Log)
	// propagate so a transport-level logger covers the framer
	if settings.FramerSettings != nil && settings.FramerSettings.Log == nil {
		settings.FramerSettings.Log = log
	}
	transport := &PlatformTransport{
		ctx:    cancelCtx,
		cancel: cancel,
		log:    log,
		// cancel: func() {
		// 	select {
		// 	case <- ctx.Done():
		// 	default:
		// 		debug.PrintStack()
		// 		cancel()
		// 	}
		// },
		clientStrategy:       clientStrategy,
		routeManager:         routeManager,
		platformUrl:          platformUrl,
		auth:                 auth,
		settings:             settings,
		availableModeMonitor: NewMonitor(),
		availableModes:       map[TransportMode]bool{},
		targetMode:           targetMode,
		mode:                 NewMonitorValue(TransportModeNone),
		connectedMonitor:     NewMonitor(),
	}
	go HandleError(transport.run, cancel)
	return transport
}

// the auth is used on future connections
func (self *PlatformTransport) SetAuth(auth *ClientAuth) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	self.auth = auth
}

// setModeAvailable records whether a mode has a live connection, waking the
// election loop (`run`) when that changes. The notify is issued in the same
// locked scope as the mutation, and only on an actual change: an unconditional
// notify would wake the loop on every reconnect churn for no new decision.
func (self *PlatformTransport) setModeAvailable(mode TransportMode, available bool) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	if self.availableModes[mode] == available {
		return
	}
	self.availableModes[mode] = available
	self.availableModeMonitor.NotifyAll()
}

func (self *PlatformTransport) modesAvailable() (map[TransportMode]bool, chan struct{}) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	return maps.Clone(self.availableModes), self.availableModeMonitor.NotifyChannel()
}

// setActiveMode publishes the elected mode. It notifies the mode gates and the
// inactive-drain watchdogs — but deliberately not the election loop, which is
// the caller: watching what it writes would wake it in a cycle.
func (self *PlatformTransport) setActiveMode(mode TransportMode) {
	self.mode.Set(mode)
}

func (self *PlatformTransport) activeMode() (TransportMode, chan struct{}) {
	return self.mode.Get()
}

// transportModePreferences ranks the real transport modes. LOWER IS BETTER (see
// isBetterMode). TransportModeNone is deliberately NOT a key: modePreference
// ranks it, and any unknown mode, worse than every real mode. Leaving it out of
// the table and reading the map directly scored it 0 — better than everything —
// which is why no mode gate ever parked and the election could not distinguish
// "no transport" from "the best transport".
//
// Two tiers, with a tie inside each:
//   - the direct modes (h3, h1) are equally preferred. whichever connects first
//     becomes active and the other does not preempt it; the election is sticky
//     among equals (see run).
//   - the packet translation modes (h3dns, h3dnspump) tunnel over dns to stay
//     reachable where the direct modes are filtered. they are an availability
//     fallback, so they rank below the direct modes and are equally preferred
//     among themselves.
//
// This table previously had the tiers inverted — it made h3dnspump the most
// preferred mode — contradicting the mode constants, which are declared "in
// order of increasing preference". Nothing enforced the ordering then (the mode
// was never elected at all), so the inversion was inert; the gates enforce it now.
var transportModePreferences = map[TransportMode]int{
	TransportModeH3: 1,
	TransportModeH1: 1,

	TransportModeH3Dns:     2,
	TransportModeH3DnsPump: 2,
}

// modePreferenceNone ranks TransportModeNone — the absence of a transport — and
// any mode missing from the table as worse than every real mode.
const modePreferenceNone = math.MaxInt

func modePreference(mode TransportMode) int {
	if preference, ok := transportModePreferences[mode]; ok {
		return preference
	}
	return modePreferenceNone
}

func (self *PlatformTransport) run() {
	defer self.cancel()

	// TODO udp protocols need proxy protocol support in the load balancer
	// see https://github.com/nginx/nginx/issues/1061
	switch self.targetMode {
	case TransportModeAuto:
		go HandleError(func() {
			self.runH1(0)
		}, self.cancel)
		// go HandleError(func() {
		// 	self.runH3(TransportModeH3, 0, 1)
		// }, self.cancel)
		// go HandleError(func() {
		// 	self.runH3(TransportModeH3Dns, self.settings.ModeInitialDelay, self.settings.PtDnsSlowMultiple)
		// }, self.cancel)
		// go HandleError(func() {
		// 	self.runH3(TransportModeH3DnsPump, self.settings.ModeInitialDelay*2, self.settings.PtDnsSlowMultiple)
		// }, self.cancel)
	case TransportModeH3:
		go HandleError(func() {
			self.runH3(TransportModeH3, 0, 1)
		}, self.cancel)
	case TransportModeH1:
		go HandleError(func() {
			self.runH1(0)
		}, self.cancel)
	case TransportModeH3Dns:
		go HandleError(func() {
			self.runH3(TransportModeH3Dns, 0, self.settings.PtDnsSlowMultiple)
		}, self.cancel)
	case TransportModeH3DnsPump:
		go HandleError(func() {
			self.runH3(TransportModeH3DnsPump, 0, self.settings.PtDnsSlowMultiple)
		}, self.cancel)
	}

	for {
		available, notify := self.modesAvailable()

		// descending preference. the comparator must be consistent: the previous
		// one returned 1 for both (a, b) and (b, a) when the preferences tied
		// (h3 and h1 do), and `maps.Keys` is randomly ordered, so the election
		// picked an arbitrary winner among tied modes on every pass — flipping
		// the active mode and thrashing the gates. break ties on the mode name
		orderedModes := slices.Collect(maps.Keys(transportModePreferences))
		slices.SortFunc(orderedModes, func(a TransportMode, b TransportMode) int {
			preferenceA := modePreference(a)
			preferenceB := modePreference(b)
			if preferenceA < preferenceB {
				return -1
			} else if preferenceB < preferenceA {
				return 1
			}
			return strings.Compare(string(a), string(b))
		})
		bestMode := TransportModeNone
		for _, mode := range orderedModes {
			if available[mode] {
				bestMode = mode
				break
			}
		}

		// equally preferred modes do not preempt each other: whichever connected
		// first stays active (h3 and h1 tie, as do h3dns and h3dnspump). the
		// active mode changes only when something strictly better becomes
		// available, or when it is no longer available itself — in which case it
		// falls back to the best that remains, and to TransportModeNone when
		// nothing does. that fallback previously lived in an `else` on
		// `0 < len(orderedModes)`, which is unreachable (orderedModes is the key
		// set of a constant map), so a mode that dropped left the active mode
		// pinned to its stale value
		activeMode := self.mode.Value()
		if !available[activeMode] || isBetterMode(bestMode, activeMode) {
			activeMode = bestMode
		}
		self.setActiveMode(activeMode)

		select {
		case <-notify:
		case <-self.ctx.Done():
			return
		}
	}
}

// returns true is other is better than current
// isBetterMode reports whether mode is strictly preferred over other. Lower
// preference values are better; TransportModeNone is worse than everything.
func isBetterMode(mode TransportMode, other TransportMode) bool {
	return modePreference(mode) < modePreference(other)
}

// standDown reports whether a transport running mode should stand down because a
// strictly better mode is currently active, along with the channel that closes
// when the active mode changes. A transport runs when it is the active mode, or
// when nothing better than it is active — including at startup, where the active
// mode is TransportModeNone (worse than every real mode) precisely so that the
// first transport is admitted and can make itself available.
//
// The gates previously asked isBetterMode(myMode, activeMode) — standing down
// when the transport was BETTER than what was active, exactly backwards. It was
// masked because TransportModeNone scored best, so the predicate was always
// false and no transport ever stood down.
func (self *PlatformTransport) standDown(mode TransportMode) (bool, chan struct{}) {
	activeMode, notify := self.activeMode()
	return isBetterMode(activeMode, mode), notify
}

func (self *PlatformTransport) runH1(initialTimeout time.Duration) {
	// connect and update route manager for this transport
	defer self.cancel()

	clientId, _ := self.auth.ClientId()

	if 0 < initialTimeout {
		select {
		case <-self.ctx.Done():
			return
		case <-time.After(initialTimeout):
		}
	}

	for {
		// stand down while a strictly better mode is active
		func() {
			for {
				standDown, notify := self.standDown(TransportModeH1)
				if !standDown {
					return
				}
				select {
				case <-self.ctx.Done():
					return
				case <-notify:
				}
			}
		}()

		reconnect := NewReconnect(self.settings.ReconnectTimeout)
		connect := func() (*websocket.Conn, error) {
			header := http.Header{}
			if self.settings.V2H1Auth {
				header.Add("Authorization", fmt.Sprintf("Bearer %s", self.auth.ByJwt))
				header.Add("X-UR-AppVersion", self.auth.AppVersion)
				header.Add("X-UR-InstanceId", self.auth.InstanceId.String())
				header.Add("X-UR-TransportVersion", fmt.Sprintf("%d", TransportVersion))
			}

			ws, _, err := self.clientStrategy.WsDialContext(self.ctx, self.platformUrl, header)
			if err != nil {
				return nil, err
			}

			success := false
			defer func() {
				if !success {
					ws.Close()
				}
			}()

			if !self.settings.V2H1Auth {
				authBytes, err := EncodeFrame(&protocol.Auth{
					ByJwt:      self.auth.ByJwt,
					AppVersion: self.auth.AppVersion,
					InstanceId: self.auth.InstanceId.Bytes(),
				}, self.settings.ProtocolVersion)
				if err != nil {
					return nil, err
				}
				defer MessagePoolReturn(authBytes)

				ws.SetWriteDeadline(time.Now().Add(self.settings.AuthTimeout))
				if err := ws.WriteMessage(websocket.BinaryMessage, authBytes); err != nil {
					return nil, err
				}
				ws.SetReadDeadline(time.Now().Add(self.settings.AuthTimeout))
				if messageType, message, err := ws.ReadMessage(); err != nil {
					return nil, err
				} else {
					// verify the auth echo
					switch messageType {
					case websocket.BinaryMessage:
						if !bytes.Equal(authBytes, message) {
							return nil, fmt.Errorf("Auth response error: bad bytes.")
						}
					default:
						return nil, fmt.Errorf("Auth response error.")
					}
				}
			}

			success = true
			return ws, nil
		}

		if connectDelay := self.clientStrategy.NextConnectTime().Sub(time.Now()); 0 < connectDelay {
			select {
			case <-self.ctx.Done():
				return
			case <-time.After(connectDelay):
			}
		}

		var ws *websocket.Conn
		var err error
		if self.log.V(2).Enabled() {
			ws, err = TraceWithReturnError(fmt.Sprintf("[t]connect %s", clientId), connect)
		} else {
			ws, err = connect()
		}
		if err != nil {
			self.log.Infof("[t]auth error %s = %s\n", clientId, err)
			select {
			case <-self.ctx.Done():
				return
			case <-reconnect.After():
				continue
			}
		}

		c := func() {
			defer ws.Close()

			self.setModeAvailable(TransportModeH1, true)
			defer self.setModeAvailable(TransportModeH1, false)

			handleCtx, handleCancel := context.WithCancel(self.ctx)
			defer handleCancel()

			var readCounter atomic.Uint64
			var writeCounter atomic.Uint64

			send := make(chan []byte, self.settings.TransportBufferSize)
			receive := make(chan []byte, self.settings.TransportBufferSize)
			controlSend := make(chan []byte, self.settings.TransportBufferSize)

			drain := func(c chan []byte) {
				for {
					select {
					case message, ok := <-c:
						if !ok {
							return
						}
						MessagePoolReturn(message)
					default:
						return
					}
				}
			}

			var exportedSend chan []byte
			// note: this should be false in production
			//       it seems better to potentially leak messages than to
			//       have an extra inefficiency on the packet path
			if DebugCloseSend {
				// use zero buffer here so that the transport can stop accepting and not drop messages
				exportedSend = make(chan []byte)
				go HandleError(func() {
					defer func() {
						handleCancel()
						close(send)
						drain(send)
					}()
					for {
						select {
						case <-handleCtx.Done():
							return
						case message, ok := <-exportedSend:
							if !ok {
								return
							}
							select {
							case <-handleCtx.Done():
								MessagePoolReturn(message)
								return
							case send <- message:
							}
						}
					}
				}, func() {
					handleCancel()
					close(send)
					drain(send)
				})
			} else {
				exportedSend = send
			}

			// the platform can route any destination,
			// since every client has a platform transport
			var sendTransport Transport
			var receiveTransport Transport
			if self.settings.TransportGenerator != nil {
				sendTransport, receiveTransport = self.settings.TransportGenerator()
			} else {
				sendTransport = NewSendGatewayTransport()
				receiveTransport = NewReceiveGatewayTransport()
			}

			self.routeManager.UpdateTransport(sendTransport, []Route{exportedSend})
			self.routeManager.UpdateTransport(receiveTransport, []Route{receive})
			self.setRegistered(true)

			// scoped to the writer goroutine; canceled when it exits so the
			// outer defer can drain `send` without racing the writer.
			writerCtx, writerCancel := context.WithCancel(context.Background())
			defer func() {
				self.setRegistered(false)
				self.routeManager.RemoveTransport(sendTransport)
				self.routeManager.RemoveTransport(receiveTransport)
				handleCancel()
				// once the writer has exited and no new writes can be routed,
				// drain any pooled messages still sitting in send. a stale
				// reflect.Select in MultiRouteSelector that captured our
				// route snapshot may still resolve after RemoveTransport;
				// drain again briefly to catch any final messages.
				<-writerCtx.Done()
				drain(send)
				time.Sleep(time.Millisecond)
				drain(send)
			}()

			go HandleError(func() {
				defer handleCancel()

				for {
					mode, notify := self.activeMode()
					if mode != TransportModeH1 {
						startReadCount := readCounter.Load()
						startWriteCount := writeCounter.Load()
						select {
						case <-handleCtx.Done():
							return
						case <-time.After(self.settings.InactiveDrainTimeout):
							// no activity after cool down, shut down this transport
							if readCounter.Load() == startReadCount && writeCounter.Load() == startWriteCount {
								handleCancel()
							}
						case <-notify:
						}
					} else {
						select {
						case <-handleCtx.Done():
							return
						case <-notify:
						}
					}
				}
			}, handleCancel)

			go HandleError(func() {
				defer writerCancel()
				defer handleCancel()

				speedTest := false

				write := func(message []byte) error {
					ws.SetWriteDeadline(time.Now().Add(self.settings.WriteTimeout))
					err := ws.WriteMessage(websocket.BinaryMessage, message)
					MessagePoolReturn(message)
					if err != nil {
						// note that for websocket a dealine timeout cannot be recovered
						self.log.Infof("[ts]%s-> error = %s\n", clientId, err)
						return err
					}
					if self.log.V(2).Enabled() {
						self.log.Infof("[ts]%s->\n", clientId)
					}

					writeCounter.Add(1)
					return nil
				}

				for {
					if speedTest {
						// during speed test, continue draining user traffic
						// so the route manager does not back up. mixing user
						// traffic with the speed-test echo slightly reduces
						// measurement accuracy but avoids stalling the client.
						select {
						case <-handleCtx.Done():
							return
						case <-WakeupAfter(self.settings.PingTimeout, self.settings.PingTimeout):
							ws.SetWriteDeadline(time.Now().Add(self.settings.WriteTimeout))
							if err := ws.WriteMessage(websocket.BinaryMessage, make([]byte, 0)); err != nil {
								// note that for websocket a dealine timeout cannot be recovered
								return
							}
						case message, ok := <-controlSend:
							if !ok {
								return
							}
							if len(message) == 5 {
								switch message[0] {
								case TransportControlSpeedStop:
									speedTest = false
								}
							}
							if write(message) != nil {
								return
							}
						case message, ok := <-send:
							if !ok {
								return
							}
							if len(message) <= 16 {
								self.log.Infof("[ts]send message must be >16 bytes (%d)\n", len(message))
								MessagePoolReturn(message)
							} else if write(message) != nil {
								return
							}
						}
					} else {
						select {
						case <-handleCtx.Done():
							return
						case message, ok := <-send:
							if !ok {
								return
							}
							// if !MessagePoolCheckShared(message) {
							// 	panic("[t]shared should be set")
							// }

							if len(message) <= 16 {
								self.log.Infof("[ts]send message must be >16 bytes (%d)\n", len(message))
								MessagePoolReturn(message)
							} else if write(message) != nil {
								return
							}
						case <-WakeupAfter(self.settings.PingTimeout, self.settings.PingTimeout):
							ws.SetWriteDeadline(time.Now().Add(self.settings.WriteTimeout))
							if err := ws.WriteMessage(websocket.BinaryMessage, make([]byte, 0)); err != nil {
								// note that for websocket a dealine timeout cannot be recovered
								return
							}
						case message, ok := <-controlSend:
							if !ok {
								return
							}
							if len(message) == 5 {
								switch message[0] {
								case TransportControlSpeedStart:
									speedTest = true
								}
							}
							if write(message) != nil {
								return
							}
						}
					}
				}
			}, handleCancel)

			go HandleError(func() {
				defer func() {
					handleCancel()
					close(receive)
					close(controlSend)

					drain(receive)
					drain(controlSend)
				}()

				speedTest := false

				for {
					select {
					case <-handleCtx.Done():
						return
					default:
					}

					ws.SetReadDeadline(time.Now().Add(self.settings.ReadTimeout))
					messageType, r, err := ws.NextReader()
					if err != nil {
						if self.log.V(2).Enabled() {
							self.log.Infof("[tr]%s<- error = %s\n", clientId, err)
						}
						return
					}

					switch messageType {
					case websocket.BinaryMessage:

						message, err := MessagePoolReadAll(r)
						if err != nil {
							if self.log.V(2).Enabled() {
								self.log.Infof("[tr]%s<- error = %s\n", clientId, err)
							}
							return
						}

						readCounter.Add(1)

						if len(message) <= 16 {
							if len(message) == 0 {
								// ping
								if self.log.V(2).Enabled() {
									self.log.Infof("[tr]ping %s<-\n", clientId)
								}
								MessagePoolReturn(message)
							} else if len(message) == 5 {
								switch message[0] {
								case TransportControlSpeedStart:
									speedTest = true
									// echo
									select {
									case <-handleCtx.Done():
										MessagePoolReturn(message)
										return
									case controlSend <- message:
									}
								case TransportControlSpeedStop:
									speedTest = false
									// echo
									select {
									case <-handleCtx.Done():
										MessagePoolReturn(message)
										return
									case controlSend <- message:
									}
								default:
									MessagePoolReturn(message)
								}
							} else if len(message) == 16 {
								// latency test echo
								select {
								case <-handleCtx.Done():
									MessagePoolReturn(message)
									return
								case controlSend <- message:
								}
							} else {
								MessagePoolReturn(message)
							}
							continue
						}
						if speedTest {
							// speed test echo
							select {
							case <-handleCtx.Done():
								MessagePoolReturn(message)
								return
							case controlSend <- message:
							}
							continue
						}

						select {
						case <-handleCtx.Done():
							MessagePoolReturn(message)
							return
						case receive <- message:
							if self.log.V(2).Enabled() {
								self.log.Infof("[tr]%s<-\n", clientId)
							}
						case <-time.After(self.settings.ReadTimeout):
							self.log.Infof("[tr]drop %s<-\n", clientId)
							MessagePoolReturn(message)
						}
					default:
						if self.log.V(2).Enabled() {
							self.log.Infof("[tr]other=%s %s<-\n", messageType, clientId)
						}
					}

					// messageType, message, err := ws.ReadMessage()
					// if err != nil {
					// 	self.log.Infof("[tr]%s<- error = %s\n", clientId, err)
					// 	return
					// }

				}
			}, func() {
				handleCancel()
				close(receive)
				close(controlSend)

				drain(receive)
				drain(controlSend)
			})

			select {
			case <-handleCtx.Done():
			}
		}

		reconnect = NewReconnect(self.settings.ReconnectTimeout)
		if self.log.V(2).Enabled() {
			Trace(fmt.Sprintf("[t]connect run %s", clientId), c)
		} else {
			c()
		}

		select {
		case <-self.ctx.Done():
			return
		case <-reconnect.After():
		}
	}
}

func (self *PlatformTransport) runH3(ptMode TransportMode, initialTimeout time.Duration, slowMultiple int) {
	// connect and update route manager for this transport
	defer self.cancel()

	if slowMultiple < 1 {
		panic(fmt.Errorf("Bad slow multiple: %d", slowMultiple))
	}

	clientId, _ := self.auth.ClientId()

	authBytes, err := EncodeFrame(&protocol.Auth{
		ByJwt:      self.auth.ByJwt,
		AppVersion: self.auth.AppVersion,
		InstanceId: self.auth.InstanceId.Bytes(),
	}, self.settings.ProtocolVersion)
	if err != nil {
		return
	}
	defer MessagePoolReturn(authBytes)

	if 0 < initialTimeout {
		select {
		case <-self.ctx.Done():
			return
		case <-time.After(initialTimeout):
		}
	}

	for {
		// wait until we are back in the specific pt mode or auto mode
		// stand down while a strictly better mode is active
		func() {
			for {
				standDown, notify := self.standDown(ptMode)
				if !standDown {
					return
				}
				select {
				case <-self.ctx.Done():
					return
				case <-notify:
				}
			}
		}()

		reconnect := NewReconnect(self.settings.ReconnectTimeout)

		type ConnStream struct {
			conn   *quic.Conn
			stream *quic.Stream
		}

		connect := func() (*ConnStream, error) {
			// quicConfig := &quic.Config{
			// 	HandshakeIdleTimeout: self.settings.QuicConnectTimeout + self.settings.QuicHandshakeTimeout,
			// }

			success := false

			quicConfig := &quic.Config{
				HandshakeIdleTimeout:    time.Duration(slowMultiple) * (self.settings.QuicConnectTimeout + self.settings.QuicHandshakeTimeout),
				MaxIdleTimeout:          self.settings.PingTimeout * 4,
				KeepAlivePeriod:         0,
				Allow0RTT:               true,
				DisablePathMTUDiscovery: true,
				InitialPacketSize:       1400,
				// pin the receive windows and stream counts. the library
				// defaults allow ~15mib per connection plus ~6mib per stream;
				// the max windows are per connection, so scaled by the memory
				// budget. the platform transport uses one bidirectional
				// stream, so the stream counts only bound abuse.
				InitialStreamReceiveWindow:     uint64(kib(256)),
				MaxStreamReceiveWindow:         uint64(MemoryScaledByteCount(mib(3), kib(384))),
				InitialConnectionReceiveWindow: uint64(kib(512)),
				MaxConnectionReceiveWindow:     uint64(MemoryScaledByteCount(mib(4), kib(512))),
				MaxIncomingStreams:             8,
				MaxIncomingUniStreams:          8,
			}
			var tlsConfig *tls.Config
			if self.settings.QuicTlsConfig != nil {
				// copy
				tlsConfig = self.settings.QuicTlsConfig.Clone()
			} else {
				tlsConfig = &tls.Config{}
			}

			var packetConn net.PacketConn

			udpConn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4zero, Port: 0})
			if err != nil {
				return nil, err
			}
			// bind to the physical egress interface so the platform QUIC
			// connection never loops into the tunnel this process provides
			// (R1); a no-op off Windows and when no egress index is set.
			_ = applyEgress(udpConn)
			// single close path: once packetConn is bound (either directly
			// to udpConn or wrapping it via packetTranslation), it owns the
			// close. before that, we close udpConn directly. avoids the
			// double-close on udpConn when packetConn == udpConn or when
			// packetTranslation.Close closes its inner udpConn.
			defer func() {
				if success {
					return
				}
				if packetConn != nil {
					packetConn.Close()
				} else {
					udpConn.Close()
				}
			}()

			serverName, err := connectHost(self.platformUrl)
			if err != nil {
				return nil, err
			}
			var udpAddr *net.UDPAddr
			switch ptMode {
			case TransportModeH3Dns:
				tld := self.settings.DnsTlds[mathrand.Intn(len(self.settings.DnsTlds))]
				udpAddr, err = net.ResolveUDPAddr("udp", fmt.Sprintf("%s:%d", serverName, self.settings.DnsPort))
				if err != nil {
					return nil, err
				}
				ptSettings := DefaultPacketTranslationSettings()
				ptSettings.DnsTlds = [][]byte{tld}
				packetConn, err = NewPacketTranslation(self.ctx, PacketTranslationModeDns, udpConn, ptSettings)
				if err != nil {
					return nil, err
				}
			case TransportModeH3DnsPump:
				tld := self.settings.DnsTlds[mathrand.Intn(len(self.settings.DnsTlds))]
				pumpServerName, err := pumpHost(self.platformUrl, tld)
				if err != nil {
					return nil, err
				}
				udpAddr, err = net.ResolveUDPAddr("udp", fmt.Sprintf("%s:%d", pumpServerName, self.settings.DnsPort))
				if err != nil {
					return nil, err
				}
				ptSettings := DefaultPacketTranslationSettings()
				ptSettings.DnsTlds = [][]byte{tld}
				packetConn, err = NewPacketTranslation(self.ctx, PacketTranslationModeDnsPump, udpConn, ptSettings)
				if err != nil {
					return nil, err
				}
			default:
				udpAddr, err = net.ResolveUDPAddr("udp", fmt.Sprintf("%s:%d", serverName, self.settings.H3Port))
				if err != nil {
					return nil, err
				}
				packetConn = udpConn
			}

			self.log.Infof("[c]h3 connect to %v (%s)\n", udpAddr, serverName)

			tlsConfig.ServerName = serverName
			quicTransport := &quic.Transport{
				Conn: packetConn,
				// createdConn: true,
				// isSingleUse: true,
			}
			conn, err := quicTransport.DialEarly(self.ctx, udpAddr, tlsConfig, quicConfig)

			// conn, err := quic.Dial(self.ctx, packetConn, packetConn.ConnectedAddr(), self.settings.QuicTlsConfig, quicConfig)
			if err != nil {
				self.log.Infof("[c]h3 connect err = %s\n", err)
				return nil, err
			}
			defer func() {
				if !success {
					conn.CloseWithError(0, "")
				}
			}()

			stream, err := conn.OpenStreamSync(self.ctx)
			if err != nil {
				self.log.Infof("[c]h3 open stream err = %s\n", err)
				return nil, err
			}

			framer := NewFramer(self.settings.FramerSettings)

			stream.SetWriteDeadline(time.Now().Add(time.Duration(slowMultiple) * self.settings.AuthTimeout))
			if err := framer.Write(stream, authBytes); err != nil {
				return nil, err
			}
			stream.SetReadDeadline(time.Now().Add(time.Duration(slowMultiple) * self.settings.AuthTimeout))
			if message, err := framer.Read(stream); err != nil {
				return nil, err
			} else {
				// verify the auth echo
				equal := bytes.Equal(authBytes, message)
				MessagePoolReturn(message)
				if !equal {
					return nil, fmt.Errorf("Auth response error: bad bytes.")
				}
			}

			success = true
			return &ConnStream{
				conn:   conn,
				stream: stream,
			}, nil
		}

		if connectDelay := self.clientStrategy.NextConnectTime().Sub(time.Now()); 0 < connectDelay {
			select {
			case <-self.ctx.Done():
				return
			case <-time.After(connectDelay):
			}
		}

		var connStream *ConnStream
		var err error
		if self.log.V(2).Enabled() {
			connStream, err = TraceWithReturnError(fmt.Sprintf("[t]connect %s", clientId), connect)
		} else {
			connStream, err = connect()
		}
		if err != nil {
			self.log.Infof("[t]auth error %s = %s\n", clientId, err)
			select {
			case <-self.ctx.Done():
				return
			case <-reconnect.After():
				continue
			}
		}
		conn := connStream.conn
		stream := connStream.stream

		c := func() {
			defer conn.CloseWithError(0, "")

			self.setModeAvailable(ptMode, true)
			defer self.setModeAvailable(ptMode, false)

			handleCtx, handleCancel := context.WithCancel(self.ctx)
			defer handleCancel()

			framer := NewFramer(self.settings.FramerSettings)

			var readCounter atomic.Uint64
			var writeCounter atomic.Uint64

			send := make(chan []byte, self.settings.TransportBufferSize)
			receive := make(chan []byte, self.settings.TransportBufferSize)

			drain := func(c chan []byte) {
				for {
					select {
					case message, ok := <-c:
						if !ok {
							return
						}
						MessagePoolReturn(message)
					default:
						return
					}
				}
			}

			// the platform can route any destination,
			// since every client has a platform transport
			var sendTransport Transport
			var receiveTransport Transport
			if self.settings.TransportGenerator != nil {
				sendTransport, receiveTransport = self.settings.TransportGenerator()
			} else {
				sendTransport = NewSendGatewayTransport()
				receiveTransport = NewReceiveGatewayTransport()
			}

			self.routeManager.UpdateTransport(sendTransport, []Route{send})
			self.routeManager.UpdateTransport(receiveTransport, []Route{receive})
			self.setRegistered(true)

			// scoped to the writer goroutine; canceled when it exits so the
			// outer defer can drain `send` without racing the writer.
			h3WriterCtx, h3WriterCancel := context.WithCancel(context.Background())
			defer func() {
				self.setRegistered(false)
				self.routeManager.RemoveTransport(sendTransport)
				self.routeManager.RemoveTransport(receiveTransport)
				handleCancel()
				// note `send` is not closed. drain any pooled bytes still
				// queued after the writer exits and RemoveTransport has
				// stopped new route writes. a stale reflect.Select may
				// still resolve after RemoveTransport; drain again briefly
				// to catch any final messages.
				<-h3WriterCtx.Done()
				drain(send)
				time.Sleep(time.Millisecond)
				drain(send)
			}()

			go HandleError(func() {
				defer handleCancel()

				for {
					mode, notify := self.activeMode()
					if mode != ptMode {
						startReadCount := readCounter.Load()
						startWriteCount := writeCounter.Load()
						select {
						case <-handleCtx.Done():
							return
						case <-time.After(time.Duration(slowMultiple) * self.settings.InactiveDrainTimeout):
							// no activity after cool down, shut down this transport
							if readCounter.Load() == startReadCount && writeCounter.Load() == startWriteCount {
								handleCancel()
							}
						case <-notify:
						}
					} else {
						select {
						case <-handleCtx.Done():
							return
						case <-notify:
						}
					}
				}
			}, handleCancel)

			go HandleError(func() {
				defer h3WriterCancel()
				defer handleCancel()

				for {
					select {
					case <-handleCtx.Done():
						return
					case message, ok := <-send:
						if !ok {
							return
						}
						// if !MessagePoolCheckShared(message) {
						// 	panic("[t]shared should be set")
						// }
						stream.SetWriteDeadline(time.Now().Add(time.Duration(slowMultiple) * self.settings.WriteTimeout))
						err := framer.Write(stream, message)
						MessagePoolReturn(message)
						if err != nil {
							// note that for websocket a dealine timeout cannot be recovered
							self.log.Infof("[ts]%s-> error = %s\n", clientId, err)
							return
						}
						if self.log.V(2).Enabled() {
							self.log.Infof("[ts]%s->\n", clientId)
						}
					case <-WakeupAfter(self.settings.PingTimeout, self.settings.PingTimeout):
						stream.SetWriteDeadline(time.Now().Add(time.Duration(slowMultiple) * self.settings.WriteTimeout))
						if err := framer.Write(stream, make([]byte, 0)); err != nil {
							// note that for websocket a dealine timeout cannot be recovered
							return
						}
					}
				}
			}, handleCancel)

			go HandleError(func() {
				defer func() {
					handleCancel()
					close(receive)
				}()

				for {
					select {
					case <-handleCtx.Done():
						return
					default:
					}

					stream.SetReadDeadline(time.Now().Add(time.Duration(slowMultiple) * self.settings.ReadTimeout))
					message, err := framer.Read(stream)
					if err != nil {
						self.log.Infof("[tr]%s<- error = %s\n", clientId, err)
						return
					}

					if 0 == len(message) {
						// ping
						if self.log.V(2).Enabled() {
							self.log.Infof("[tr]ping %s<-\n", clientId)
						}
						MessagePoolReturn(message)
						continue
					}

					select {
					case <-handleCtx.Done():
						MessagePoolReturn(message)
						return
					case receive <- message:
						if self.log.V(2).Enabled() {
							self.log.Infof("[tr]%s<-\n", clientId)
						}
					case <-time.After(time.Duration(slowMultiple) * self.settings.ReadTimeout):
						self.log.Infof("[tr]drop %s<-\n", clientId)
						MessagePoolReturn(message)
					}
				}
			}, func() {
				handleCancel()
				close(receive)
			})

			select {
			case <-handleCtx.Done():
			}
		}
		reconnect = NewReconnect(self.settings.ReconnectTimeout)
		if self.log.V(2).Enabled() {
			Trace(fmt.Sprintf("[t]connect run %s", clientId), c)
		} else {
			c()
		}

		select {
		case <-self.ctx.Done():
			return
		case <-reconnect.After():
		}
	}
}

func (self *PlatformTransport) Close() {
	self.cancel()
}

func connectHost(platformUrl string) (string, error) {
	u, err := url.Parse(platformUrl)
	if err != nil {
		return "", err
	}
	return u.Hostname(), nil
}

// this host should resolve in dns to the root zone ips for the tld
func pumpHost(platformUrl string, tld []byte) (string, error) {
	u, err := url.Parse(platformUrl)
	if err != nil {
		return "", err
	}
	host := u.Hostname()
	if net.ParseIP(host) != nil {
		return host, nil
	}
	// tld replace . with -
	// zone-<tld>.<base>
	baseHost := strings.SplitN(host, ".", 2)[1]
	pumpHost := fmt.Sprintf("zone-%s.%s", strings.ReplaceAll(string(tld), ".", "-"), baseHost)
	return pumpHost, nil
}

package connect

import (
	"context"
	"encoding/json"
	"math"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/protocol"
)

// A single FIFO scheduler separates serialization from propagation. There is
// no goroutine per message and no accidental reordering on timer wakeups.
// The finite relay queue counts only bytes/messages awaiting serialization;
// already propagating messages are separately bounded by the flight limit.
type windowPathLink struct {
	rate       ByteCount
	delay      time.Duration
	queueCount int
	queueBytes ByteCount
	dropOnFull bool
	dropped    atomic.Int64
	maxQueued  atomic.Int64
}

type windowPathFrame struct {
	bytes  []byte
	depart time.Time
	arrive time.Time
}

func (self *windowPathLink) run(ctx context.Context, from, to Route) {
	var queue []windowPathFrame
	head, serviced := 0, 0
	queuedBytes := ByteCount(0)
	departure := time.Now()
	timer := time.NewTimer(0)
	defer timer.Stop()
	defer func() {
		for _, frame := range queue[head:] {
			MessagePoolReturn(frame.bytes)
		}
	}()
	for {
		now := time.Now()
		for serviced < len(queue) && !now.Before(queue[serviced].depart) {
			queuedBytes -= ByteCount(len(queue[serviced].bytes))
			serviced++
		}
		queuedCount := len(queue) - serviced
		if old := self.maxQueued.Load(); old < int64(queuedCount) {
			self.maxQueued.Store(int64(queuedCount))
		}
		var output Route
		var ready []byte
		var timeout <-chan time.Time
		if head < len(queue) {
			if remaining := time.Until(queue[head].arrive); remaining <= 0 {
				output, ready = to, queue[head].bytes
			} else {
				timer.Reset(remaining)
				timeout = timer.C
			}
		}
		input := from
		if (!self.dropOnFull && (queuedCount >= self.queueCount || queuedBytes >= self.queueBytes)) || len(queue)-head >= 65536 {
			input = nil
		}
		select {
		case <-ctx.Done():
			return
		case <-timeout:
		case output <- ready:
			queue[head] = windowPathFrame{}
			head++
			if head >= 1024 && head*2 >= len(queue) {
				count := copy(queue, queue[head:])
				clear(queue[count:])
				queue = queue[:count]
				serviced -= head
				head = 0
			}
		case frame := <-input:
			now = time.Now()
			for serviced < len(queue) && !now.Before(queue[serviced].depart) {
				queuedBytes -= ByteCount(len(queue[serviced].bytes))
				serviced++
			}
			queuedCount = len(queue) - serviced
			if self.dropOnFull && (queuedCount >= self.queueCount || queuedBytes+ByteCount(len(frame)) > self.queueBytes) {
				self.dropped.Add(1)
				MessagePoolReturn(frame)
				continue
			}
			if departure.Before(now) {
				departure = now
			}
			if self.rate > 0 {
				departure = departure.Add(time.Duration(int64(len(frame)) * int64(time.Second) / self.rate))
			}
			queue = append(queue, windowPathFrame{bytes: frame, depart: departure, arrive: departure.Add(self.delay)})
			queuedBytes += ByteCount(len(frame))
		}
	}
}

// The instrument itself must not charge propagation as queue occupancy or
// turn a finite FIFO into one RTT of service per packet.
func TestWindowPathFifoSeparatesQueueAndPropagation(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		from, to := make(Route, 8), make(Route, 8)
		link := windowPathLink{rate: 1000, delay: 10 * time.Second, queueCount: 2, queueBytes: 2000, dropOnFull: true}
		done := make(chan struct{})
		go func() { defer close(done); link.run(ctx, from, to) }()
		t.Cleanup(func() {
			cancel()
			<-done
			for len(to) != 0 {
				MessagePoolReturn(<-to)
			}
		})
		for i := range 3 {
			b := MessagePoolGet(1000)
			b[0] = byte(i)
			from <- b
		}
		synctest.Wait()
		if link.dropped.Load() != 1 {
			t.Fatal("finite serialization queue did not drop exactly its third message")
		}
		time.Sleep(2 * time.Second)
		b := MessagePoolGet(1000)
		b[0] = 3
		from <- b
		synctest.Wait()
		if link.dropped.Load() != 1 {
			t.Fatal("propagating messages consumed relay queue capacity")
		}
		for _, want := range []byte{0, 1, 3} {
			b := <-to
			got := b[0]
			MessagePoolReturn(b)
			if got != want {
				t.Fatalf("FIFO order: got %d want %d", got, want)
			}
		}
	})
}

// Virtual-time goodput is a deterministic model outcome, not host throughput.
// The control changes only the RTT multiplier; budgets, sampler, receiver,
// framing and offered bytes stay identical in both arms.
func TestWindowCompressionResidenceRestoresShortPathCapacity(t *testing.T) {
	assertMessagePoolOwnership(t)
	var old, fixed float64
	for _, arm := range []string{"path-rtt-only", "delivery"} {
		synctest.Test(t, func(t *testing.T) {
			reading := measureWindowPathCell(t, windowPathCell{
				Arm: arm, RoundTrip: 300 * time.Microsecond, Compression: 10 * time.Millisecond,
				Flows: 1, Payload: 1280, Budget: mib(48), Rate: 125000000,
			}, 100*time.Millisecond)
			if arm == "path-rtt-only" {
				old = reading.Mbps
			} else {
				fixed = reading.Mbps
			}
			t.Logf("%s: %.1f model Mb/s, window=%d reason=%s", arm, reading.Mbps, reading.Window.Window, reading.Window.Reason)
		})
	}
	if old >= 400 || fixed < 850 {
		t.Fatalf("short-path residence control: old=%.1f fixed=%.1f model Mb/s", old, fixed)
	}
}

// Every reported/design-point RTT is exercised with one/eight offered flows
// and immediate/compressed ACKs. A constant window with the same memory and
// receiver bounds calibrates the fixture's attainable payload rate per cell.
func TestWindowPathDeterministicPerformanceMatrix(t *testing.T) {
	assertMessagePoolOwnership(t)
	for _, rtt := range envDurations(t, "CONNECT_WINDOW_MODEL_RTT_US", time.Microsecond, []time.Duration{300 * time.Microsecond, time.Millisecond, 2 * time.Millisecond, 5 * time.Millisecond, 10 * time.Millisecond, 25 * time.Millisecond, 100 * time.Millisecond, 200 * time.Millisecond, 400 * time.Millisecond}) {
		for _, flows := range []int{1, 8} {
			for _, compression := range []time.Duration{0, 10 * time.Millisecond} {
				var ceiling, fixed windowPathReading
				for _, arm := range []string{"ceiling", "delivery"} {
					synctest.Test(t, func(t *testing.T) {
						reading := measureWindowPathCell(t, windowPathCell{Arm: arm, RoundTrip: rtt, Compression: compression, Flows: flows, Payload: 1280, Budget: mib(48), Rate: 125000000}, max(100*time.Millisecond, 2*rtt))
						if arm == "ceiling" {
							ceiling = reading
						} else {
							fixed = reading
						}
					})
				}
				t.Logf("rtt=%s flows=%d compression=%s ceiling=%.1f fixed=%.1f min-flow=%.1f model Mb/s", rtt, flows, compression, ceiling.Mbps, fixed.Mbps, fixed.MinFlowMbps)
				if ceiling.Mbps < 500 || fixed.Mbps < .85*ceiling.Mbps || fixed.MinFlowMbps == 0 || fixed.RelayDrops != 0 {
					t.Errorf("uncalibrated or underperforming model cell: rtt=%s flows=%d compression=%s ceiling=%.1f fixed=%.1f min-flow=%.1f drops=%d", rtt, flows, compression, ceiling.Mbps, fixed.Mbps, fixed.MinFlowMbps, fixed.RelayDrops)
					encoded, _ := json.Marshal(fixed)
					t.Log(string(encoded))
				}
			}
		}
	}
}

// A relay may accept a reliable-carrier message and then refuse it at its
// finite forwarding queue. Pin recovery and sustained delivery at that exact
// boundary, where carrier reliability cannot recover the discarded Pack.
func TestWindowPathRecoversFiniteRelayOverflow(t *testing.T) {
	assertMessagePoolOwnership(t)
	for _, flows := range []int{1, 8} {
		synctest.Test(t, func(t *testing.T) {
			reading := measureWindowPathCell(t, windowPathCell{Arm: "delivery", RoundTrip: 100 * time.Millisecond, Compression: 10 * time.Millisecond, Flows: flows, Payload: 1280, Budget: mib(48), Rate: 125000000, Drop: true}, time.Second)
			t.Logf("flows=%d goodput=%.1f min-flow=%.1f model Mb/s relay-drops=%d gap-repairs=%d", flows, reading.Mbps, reading.MinFlowMbps, reading.RelayDrops, reading.Recovery.SelectiveGapWriteCount)
			if reading.RelayDrops == 0 || reading.Recovery.SelectiveGapWriteCount == 0 {
				t.Fatal("finite relay overflow/recovery stimulus did not run")
			}
			if reading.Mbps < 800 || reading.MinFlowMbps == 0 {
				t.Fatalf("relay loss left an underfilled or stalled window: %.1f model Mb/s", reading.Mbps)
			}
		})
	}
}

type windowPathCell struct {
	Tcp         bool
	Upload      bool
	Arm         string
	RoundTrip   time.Duration
	Compression time.Duration
	Flows       int
	Lanes       int
	Payload     int
	Budget      ByteCount
	Drop        bool
	Rate        ByteCount
}

type windowPathReading struct {
	Cell           windowPathCell
	WarmupSeconds  float64
	Seconds        float64
	Bytes          int64
	Mbps           float64
	MinFlowMbps    float64
	RelayDrops     int64
	MaxRelayQueued int64
	Recovery       ClientSendRecoveryStatsSnapshot
	Receiver       ClientReceiveStatsSnapshot
	SenderReceive  ClientReceiveStatsSnapshot
	Window         SendWindowEstimate
}

// Measures receiver bytes over one common interval. Construction, warmup and
// draining are outside the interval. This is a Transfer/FIFO instrument, not
// a measurement of H1 sockets, a native kernel TUN, or provider TCP.
func measureWindowPathCell(t *testing.T, cell windowPathCell, duration time.Duration) windowPathReading {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	settings := func() *ClientSettings {
		s := DefaultClientSettings()
		s.Log = NewNoopLogger()
		s.EncryptionSettings.Mode = EncryptionModeOff
		s.beforeClientKeyPublishForTest = func() { <-ctx.Done() }
		s.SendBufferSettings.WindowSizing = WindowSizingConstant
		if cell.Arm == "delivery" || cell.Arm == "path-rtt-only" {
			s.SendBufferSettings.WindowSizing = WindowSizingFromDelivery
		}
		s.SendBufferSettings.ApplyWindowSizing()
		if cell.Arm == "path-rtt-only" {
			zero := time.Duration(0)
			s.SendBufferSettings.ackCompressionResidenceOverrideForTest = &zero
		}
		s.SendBufferSettings.ResendQueueMaxByteCount = mib(2)
		s.SendBufferSettings.ResendQueueBudget = NewTransferMemoryBudget(cell.Budget)
		s.SendBufferSettings.LogicalDataLaneCount = cell.Lanes
		s.ReceiveBufferSettings.ReceiveQueueBudget = NewTransferMemoryBudget(cell.Budget)
		s.ReceiveBufferSettings.ReceiveQueueMaxByteCount = cell.Budget
		s.ReceiveBufferSettings.AdvertiseReceiveWindow = true
		s.ReceiveBufferSettings.AckCompressTimeout = cell.Compression
		if cell.Arm == "constant" {
			s.SendBufferSettings.ResendQueueBudget = nil
			s.ReceiveBufferSettings.ReceiveQueueBudget = nil
			s.ReceiveBufferSettings.ReceiveQueueMaxByteCount = mib(2) + kib(512)
			s.ReceiveBufferSettings.AdvertiseReceiveWindow = false
		}
		if cell.Arm == "ceiling" {
			s.SendBufferSettings.ResendQueueMaxByteCount = cell.Budget
		}
		return s
	}
	sender := NewClient(ctx, NewId(), NewNoContractClientOob(), settings())
	receiver := NewClient(ctx, NewId(), NewNoContractClientOob(), settings())
	sender.ContractManager().AddNoContractPeer(receiver.ClientId())
	receiver.ContractManager().AddNoContractPeer(sender.ClientId())
	sendOut, sendIn, receiveOut, receiveIn := make(Route, 128), make(Route, 128), make(Route, 128), make(Route, 128)
	sender.RouteManager().UpdateTransport(NewSendGatewayTransportWithType(TransportTypeH1), []Route{sendOut})
	sender.RouteManager().UpdateTransportWithProperties(NewReceiveGatewayTransportWithType(TransportTypeH1), []Route{sendIn}, TransferCarrierProperties{ReceiveReliability: CarrierReliabilityReliable})
	receiver.RouteManager().UpdateTransport(NewSendGatewayTransportWithType(TransportTypeH1), []Route{receiveOut})
	receiver.RouteManager().UpdateTransportWithProperties(NewReceiveGatewayTransportWithType(TransportTypeH1), []Route{receiveIn}, TransferCarrierProperties{ReceiveReliability: CarrierReliabilityReliable})
	var workers sync.WaitGroup
	dataLink := windowPathLink{rate: cell.Rate, delay: cell.RoundTrip / 2, queueCount: 4096, queueBytes: mib(8), dropOnFull: cell.Drop}
	ackLink := windowPathLink{delay: cell.RoundTrip / 2, queueCount: 4096, queueBytes: mib(8)}
	if cell.Upload {
		dataLink.rate = 0
		dataLink.dropOnFull = false
		ackLink.rate = cell.Rate
		ackLink.dropOnFull = cell.Drop
	}
	workers.Go(func() { dataLink.run(ctx, sendOut, receiveIn) })
	workers.Go(func() { ackLink.run(ctx, receiveOut, sendIn) })
	counts := make([]atomic.Int64, cell.Flows)
	cleanupWorkload := func() {}
	if cell.Tcp {
		cleanupWorkload = startWindowTcpWorkload(t, ctx, sender, receiver, counts, cell.Upload)
	} else {
		receiver.AddReceiveCallback(func(_ TransferPath, frames []*protocol.Frame, _ Peer) {
			for _, frame := range frames {
				if len(frame.MessageBytes) != 0 {
					flow := int(frame.MessageBytes[0])
					if flow < len(counts) {
						counts[flow].Add(int64(len(frame.MessageBytes)))
					}
				}
			}
		})
		for flow := range cell.Flows {
			workers.Go(func() {
				for ctx.Err() == nil {
					payload := MessagePoolGet(cell.Payload)
					clear(payload)
					payload[0] = byte(flow)
					frame := &protocol.Frame{MessageType: protocol.MessageType_IpIpPacketFromProvider, MessageBytes: payload, Raw: true}
					key := TransferKey{}
					if cell.Lanes > 0 {
						key.LogicalLane = uint32(flow%cell.Lanes + 1)
					}
					if ok, err := sender.SendWithTimeoutDetailed(frame, receiver.ClientId(), nil, -1, key); !ok {
						MessagePoolReturn(payload)
						if ctx.Err() == nil {
							t.Errorf("performance producer: %v", err)
						}
						return
					}
				}
			})
		}
	}
	defer func() {
		cancel()
		cleanupWorkload()
		workers.Wait()
		closeCtx, closeCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer closeCancel()
		for _, client := range []*Client{sender, receiver} {
			if err := client.CloseAndWait(closeCtx); err != nil {
				t.Errorf("performance client cleanup: %v", err)
			}
		}
		for _, route := range []Route{sendOut, sendIn, receiveOut, receiveIn} {
			for len(route) != 0 {
				MessagePoolReturn(<-route)
			}
		}
	}()
	// Allow the blind RTT, the peer-capacity step and two complete delivery
	// horizons to settle. A 2-RTT warmup included startup in 400-ms cells.
	warmup := 300*time.Millisecond + 5*cell.RoundTrip
	time.Sleep(warmup)
	before := make([]int64, len(counts))
	for i := range counts {
		before[i] = counts[i].Load()
	}
	start := time.Now()
	time.Sleep(duration)
	elapsed := time.Since(start)
	reading := windowPathReading{Cell: cell, WarmupSeconds: warmup.Seconds(), Seconds: elapsed.Seconds(), MinFlowMbps: 1e20}
	for i := range counts {
		delivered := counts[i].Load() - before[i]
		reading.Bytes += delivered
		reading.MinFlowMbps = min(reading.MinFlowMbps, float64(delivered)*8/elapsed.Seconds()/1e6)
	}
	reading.Mbps = float64(reading.Bytes) * 8 / elapsed.Seconds() / 1e6
	reading.RelayDrops = dataLink.dropped.Load()
	reading.MaxRelayQueued = dataLink.maxQueued.Load()
	statsSource, statsDestination := sender, receiver.ClientId()
	if cell.Upload {
		statsSource, statsDestination = receiver, sender.ClientId()
		reading.RelayDrops = ackLink.dropped.Load()
		reading.MaxRelayQueued = ackLink.maxQueued.Load()
	}
	reading.Recovery = statsSource.SendRecoveryStats()
	reading.Receiver = receiver.ReceiveStats()
	reading.SenderReceive = sender.ReceiveStats()
	reading.Window = statsSource.DestinationSendStats(statsDestination).SendWindow
	return reading
}

// Opt-in paired sweep for the cells absent from the original 200/400 ms
// instrument. Each repetition includes A/A and a large-window calibration.
// The JSON ledger includes every repetition; no best-run selection or claim
// of a native-path optimum is made from this transfer-only fixture.
func TestWindowPathPerformanceMatrix(t *testing.T) {
	if os.Getenv("CONNECT_WINDOW_PATH_MEASURE") == "" {
		t.Skip("set CONNECT_WINDOW_PATH_MEASURE=1 for the paired path matrix")
	}
	testWindowPathPerformanceMatrix(t, false, false)
}

func testWindowPathPerformanceMatrix(t *testing.T, tcp bool, upload bool) {
	assertMessagePoolOwnership(t)
	rtts := envDurations(t, "CONNECT_WINDOW_PATH_RTT_US", time.Microsecond, []time.Duration{300 * time.Microsecond, time.Millisecond, 2 * time.Millisecond, 5 * time.Millisecond, 10 * time.Millisecond, 25 * time.Millisecond, 100 * time.Millisecond, 200 * time.Millisecond, 400 * time.Millisecond})
	flows := envInts(t, "CONNECT_WINDOW_PATH_FLOWS", []int{1, 8})
	compressions := envDurations(t, "CONNECT_WINDOW_PATH_ACK_MS", time.Millisecond, []time.Duration{0, 10 * time.Millisecond})
	repetitions := envInt(t, "CONNECT_WINDOW_PATH_REPETITIONS", 3)
	duration := envSeconds(t, "CONNECT_WINDOW_PATH_SECONDS", 3)
	if repetitions < 1 || duration <= 0 {
		t.Fatal("performance repetitions and duration must be positive")
	}
	for _, rtt := range rtts {
		for _, count := range flows {
			for _, compression := range compressions {
				if rtt < 0 || count < 1 || count > 256 || compression < 0 {
					t.Fatal("invalid performance RTT, flow count, or compression")
				}
				for repetition := range repetitions {
					var readings []windowPathReading
					arms := []string{"ceiling", "constant", "matched", "path-rtt-only", "delivery", "matched"}
					if repetition%2 != 0 {
						arms = []string{"matched", "delivery", "path-rtt-only", "matched", "constant", "ceiling"}
					}
					if configured := os.Getenv("CONNECT_WINDOW_PATH_ARMS"); configured != "" {
						arms = strings.Split(configured, ",")
					}
					for _, arm := range arms {
						switch arm {
						case "ceiling", "constant", "matched", "path-rtt-only", "delivery":
						default:
							t.Fatalf("unknown performance arm %q", arm)
						}
						cell := windowPathCell{Tcp: tcp, Upload: upload, Arm: arm, RoundTrip: rtt, Compression: compression, Flows: count, Lanes: envInt(t, "CONNECT_WINDOW_PATH_LANES", 0), Payload: envInt(t, "CONNECT_WINDOW_PATH_PAYLOAD", 1280), Budget: mib(48), Rate: 125000000, Drop: os.Getenv("CONNECT_WINDOW_PATH_DROP") != ""}
						if cell.Payload < 1 || cell.Lanes < 0 {
							t.Fatal("invalid payload size or lane count")
						}
						reading := measureWindowPathCell(t, cell, duration)
						readings = append(readings, reading)
						encoded, err := json.Marshal(reading)
						if err != nil {
							t.Fatal(err)
						}
						t.Logf("repetition=%d %s", repetition, encoded)
						if reading.Bytes == 0 || reading.MinFlowMbps == 0 {
							t.Errorf("stalled performance cell: %+v", cell)
						}
					}
					comparison := compareWindowPathReadings(readings)
					encoded, err := json.Marshal(comparison)
					if err != nil {
						t.Fatal(err)
					}
					t.Logf("comparison repetition=%d %s", repetition, encoded)
				}
			}
		}
	}
}

type windowPathComparison struct {
	Cell                windowPathCell
	MatchedFirstMbps    float64
	MatchedLastMbps     float64
	MatchedDriftPercent float64
	CeilingMbps         float64
	DeliveryMbps        float64
	DeliveryOfCeiling   float64
	CensoredReasons     []string
}

// A/A drift and an unattained calibration ceiling prevent treating host load
// or a capped fixture as evidence for a window-policy change. All raw runs
// remain in the ledger, including censored and stalled observations.
func compareWindowPathReadings(readings []windowPathReading) windowPathComparison {
	comparison := windowPathComparison{Cell: readings[0].Cell}
	comparison.Cell.Arm = "comparison"
	matched := 0
	for _, reading := range readings {
		switch reading.Cell.Arm {
		case "matched":
			if matched == 0 {
				comparison.MatchedFirstMbps = reading.Mbps
			}
			comparison.MatchedLastMbps = reading.Mbps
			matched++
		case "ceiling":
			comparison.CeilingMbps = reading.Mbps
		case "delivery":
			comparison.DeliveryMbps = reading.Mbps
		}
		if reading.Bytes == 0 || reading.MinFlowMbps == 0 {
			comparison.CensoredReasons = append(comparison.CensoredReasons, "stalled flow")
		}
	}
	if matched < 2 || min(comparison.MatchedFirstMbps, comparison.MatchedLastMbps) <= 0 {
		comparison.CensoredReasons = append(comparison.CensoredReasons, "missing A/A repeat")
	} else {
		comparison.MatchedDriftPercent = 100 * math.Abs(comparison.MatchedFirstMbps-comparison.MatchedLastMbps) / max(comparison.MatchedFirstMbps, comparison.MatchedLastMbps)
		if comparison.MatchedDriftPercent > 10 {
			comparison.CensoredReasons = append(comparison.CensoredReasons, "A/A drift exceeds 10 percent")
		}
	}
	if comparison.CeilingMbps > 0 {
		comparison.DeliveryOfCeiling = comparison.DeliveryMbps / comparison.CeilingMbps
	}
	if comparison.CeilingMbps < .9*float64(comparison.Cell.Rate)*8/1e6 {
		comparison.CensoredReasons = append(comparison.CensoredReasons, "calibration below 90 percent of link rate")
	}
	return comparison
}

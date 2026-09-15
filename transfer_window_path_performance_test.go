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
	rate            ByteCount
	rateAfter       ByteCount
	rateChangeAfter time.Duration
	delay           time.Duration
	queueCount      int
	queueBytes      ByteCount
	dropOnFull      bool
	dropped         atomic.Int64
	maxQueued       atomic.Int64
	maxQueuedBytes  atomic.Int64
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
	changeAt := departure.Add(self.rateChangeAfter)
	rate := self.rate
	changed := self.rateAfter <= 0
	timer := time.NewTimer(0)
	defer timer.Stop()
	defer func() {
		for _, frame := range queue[head:] {
			MessagePoolReturn(frame.bytes)
		}
	}()
	for {
		now := time.Now()
		if !changed && !now.Before(changeAt) {
			// Finish the frame already in the serializer at its original
			// rate. Reschedule only frames still waiting at the change.
			for serviced < len(queue) && !changeAt.Before(queue[serviced].depart) {
				queuedBytes -= ByteCount(len(queue[serviced].bytes))
				serviced++
			}
			index := serviced
			departure = changeAt
			if index < len(queue) && rate > 0 {
				serialization := time.Duration(int64(len(queue[index].bytes)) * int64(time.Second) / int64(rate))
				if queue[index].depart.Add(-serialization).Before(changeAt) {
					departure = queue[index].depart
					index++
				}
			}
			rate = self.rateAfter
			for ; index < len(queue); index++ {
				departure = departure.Add(time.Duration(int64(len(queue[index].bytes)) * int64(time.Second) / int64(rate)))
				queue[index].depart = departure
				queue[index].arrive = departure.Add(self.delay)
			}
			changed = true
		}
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
		var deadline time.Time
		if head < len(queue) {
			if remaining := time.Until(queue[head].arrive); remaining <= 0 {
				output, ready = to, queue[head].bytes
			} else {
				deadline = queue[head].arrive
			}
		}
		if !changed && (deadline.IsZero() || changeAt.Before(deadline)) {
			deadline = changeAt
		}
		if !deadline.IsZero() {
			timer.Reset(time.Until(deadline))
			timeout = timer.C
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
			if rate > 0 {
				departure = departure.Add(time.Duration(int64(len(frame)) * int64(time.Second) / int64(rate)))
			}
			queue = append(queue, windowPathFrame{bytes: frame, depart: departure, arrive: departure.Add(self.delay)})
			queuedBytes += ByteCount(len(frame))
			self.maxQueued.Store(max(self.maxQueued.Load(), int64(len(queue)-serviced)))
			self.maxQueuedBytes.Store(max(self.maxQueuedBytes.Load(), int64(queuedBytes)))
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
func TestWindowPathBoundsBurstsAtFiniteRelay(t *testing.T) {
	assertMessagePoolOwnership(t)
	for _, flows := range []int{1, 8} {
		for _, arm := range []string{"unpaced", "delivery"} {
			synctest.Test(t, func(t *testing.T) {
				reading := measureWindowPathCell(t, windowPathCell{Arm: arm, RoundTrip: 100 * time.Millisecond, Compression: 10 * time.Millisecond, Flows: flows, Payload: 1280, Budget: mib(48), Rate: 125000000, Drop: true}, time.Second)
				t.Logf("%s flows=%d goodput=%.1f min-flow=%.1f model Mb/s relay-drops=%d gap-repairs=%d", arm, flows, reading.Mbps, reading.MinFlowMbps, reading.RelayDrops, reading.Recovery.SelectiveGapWriteCount)
				if arm == "unpaced" {
					if reading.RelayDrops == 0 || reading.Recovery.SelectiveGapWriteCount == 0 {
						t.Fatal("unpaced relay overflow/recovery stimulus did not run")
					}
				} else if reading.RelayDrops != 0 || reading.Mbps < 900 || reading.MinFlowMbps == 0 {
					t.Fatalf("paced relay is lossy, underfilled or stalled: %.1f model Mb/s, %d drops", reading.Mbps, reading.RelayDrops)
				}
			})
		}
	}
}

// The configured target is 1 Gb/s, but a slower physical path must still
// converge to its own capacity. Its startup may overflow the finite relay;
// retain those drops in the ledger rather than erasing the ramp.
func TestWindowPathSlowLinkKeepsCapacity(t *testing.T) {
	assertMessagePoolOwnership(t)
	for _, rtt := range []time.Duration{300 * time.Microsecond, 100 * time.Millisecond} {
		for _, flows := range []int{1, 8} {
			var ceiling, fixed windowPathReading
			for _, arm := range []string{"ceiling", "delivery"} {
				synctest.Test(t, func(t *testing.T) {
					reading := measureWindowPathCell(t, windowPathCell{Arm: arm, RoundTrip: rtt, Compression: 10 * time.Millisecond, Flows: flows, Payload: 1280, Budget: mib(48), Rate: 12500000, Drop: arm == "delivery"}, time.Second)
					if arm == "ceiling" {
						ceiling = reading
					} else {
						fixed = reading
					}
				})
			}
			t.Logf("slow rtt=%s flows=%d ceiling=%.1f fixed=%.1f min-flow=%.1f model Mb/s startup-and-measurement-drops=%d", rtt, flows, ceiling.Mbps, fixed.Mbps, fixed.MinFlowMbps, fixed.RelayDrops)
			if ceiling.Mbps < 90 || fixed.Mbps < .85*ceiling.Mbps || fixed.MinFlowMbps == 0 {
				t.Fatalf("slow link lost capacity: ceiling=%.1f fixed=%.1f min-flow=%.1f", ceiling.Mbps, fixed.Mbps, fixed.MinFlowMbps)
			}
		}
	}
}

type windowPathCell struct {
	CalibrationWindow  ByteCount
	SendWindow         ByteCount
	ReceiveWindow      ByteCount
	ReceiveWindowAfter ByteCount
	WindowChangeAfter  time.Duration
	Tcp                bool
	Upload             bool
	TcpBufferMax       ByteCount
	Arm                string
	RoundTrip          time.Duration
	Compression        time.Duration
	Flows              int
	RoundRobinOffer    bool
	Lanes              int
	Payload            int
	Budget             ByteCount
	Drop               bool
	Rate               ByteCount
	RateAfter          ByteCount
	RateChangeAfter    time.Duration
	Warmup             time.Duration
}

type windowPathReading struct {
	Cell                  windowPathCell
	WarmupSeconds         float64
	Seconds               float64
	Bytes                 int64
	Mbps                  float64
	MinFlowMbps           float64
	IntervalMbps          []float64
	RelayDrops            int64
	MeasurementRelayDrops int64
	NatRefused            int64
	MaxRelayQueued        int64
	MaxRelayQueuedBytes   int64
	Recovery              ClientSendRecoveryStatsSnapshot
	Receiver              ClientReceiveStatsSnapshot
	SenderReceive         ClientReceiveStatsSnapshot
	Window                SendWindowEstimate
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
		if cell.Arm == "delivery" || cell.Arm == "path-rtt-only" || cell.Arm == "unpaced" {
			s.SendBufferSettings.WindowSizing = WindowSizingFromDelivery
		}
		s.SendBufferSettings.ApplyWindowSizing()
		s.SendBufferSettings.disableWindowPacingForTest = cell.Arm == "unpaced"
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
			if cell.CalibrationWindow > 0 {
				s.SendBufferSettings.ResendQueueMaxByteCount = cell.CalibrationWindow
			}
		}
		return s
	}
	senderSettings, receiverSettings := settings(), settings()
	dataSenderSettings, dataReceiverSettings := senderSettings, receiverSettings
	if cell.Upload {
		dataSenderSettings, dataReceiverSettings = receiverSettings, senderSettings
	}
	if cell.SendWindow > 0 {
		dataSenderSettings.SendBufferSettings.ResendQueueBudget = NewTransferMemoryBudget(cell.SendWindow)
		dataSenderSettings.SendBufferSettings.DeliverySizedWindowCeilingByteCount = cell.SendWindow
		dataSenderSettings.SendBufferSettings.ResendQueueMaxByteCount = min(dataSenderSettings.SendBufferSettings.ResendQueueMaxByteCount, cell.SendWindow)
	}
	if cell.ReceiveWindow > 0 {
		dataReceiverSettings.ReceiveBufferSettings.ReceiveQueueBudget = NewTransferMemoryBudget(cell.ReceiveWindow)
		dataReceiverSettings.ReceiveBufferSettings.ReceiveQueueMaxByteCount = max(cell.ReceiveWindow, cell.ReceiveWindowAfter)
	}
	sender := NewClient(ctx, NewId(), NewNoContractClientOob(), senderSettings)
	receiver := NewClient(ctx, NewId(), NewNoContractClientOob(), receiverSettings)
	sender.ContractManager().AddNoContractPeer(receiver.ClientId())
	receiver.ContractManager().AddNoContractPeer(sender.ClientId())
	sendOut, sendIn, receiveOut, receiveIn := make(Route, 128), make(Route, 128), make(Route, 128), make(Route, 128)
	sender.RouteManager().UpdateTransport(NewSendGatewayTransportWithType(TransportTypeH1), []Route{sendOut})
	sender.RouteManager().UpdateTransportWithProperties(NewReceiveGatewayTransportWithType(TransportTypeH1), []Route{sendIn}, TransferCarrierProperties{ReceiveReliability: CarrierReliabilityReliable})
	receiver.RouteManager().UpdateTransport(NewSendGatewayTransportWithType(TransportTypeH1), []Route{receiveOut})
	receiver.RouteManager().UpdateTransportWithProperties(NewReceiveGatewayTransportWithType(TransportTypeH1), []Route{receiveIn}, TransferCarrierProperties{ReceiveReliability: CarrierReliabilityReliable})
	var workers sync.WaitGroup
	if cell.ReceiveWindowAfter > 0 {
		workers.Go(func() {
			select {
			case <-ctx.Done():
			case <-time.After(cell.WindowChangeAfter):
				dataReceiverSettings.ReceiveBufferSettings.ReceiveQueueBudget.SetTotalByteCount(cell.ReceiveWindowAfter)
			}
		})
	}
	dataLink := windowPathLink{rate: cell.Rate, rateAfter: cell.RateAfter, rateChangeAfter: cell.RateChangeAfter, delay: cell.RoundTrip / 2, queueCount: 4096, queueBytes: mib(8), dropOnFull: cell.Drop}
	ackLink := windowPathLink{delay: cell.RoundTrip / 2, queueCount: 4096, queueBytes: mib(8)}
	if cell.Upload {
		dataLink.rate = 0
		dataLink.rateAfter = 0
		dataLink.dropOnFull = false
		ackLink.rate = cell.Rate
		ackLink.rateAfter = cell.RateAfter
		ackLink.rateChangeAfter = cell.RateChangeAfter
		ackLink.dropOnFull = cell.Drop
	}
	workers.Go(func() { dataLink.run(ctx, sendOut, receiveIn) })
	workers.Go(func() { ackLink.run(ctx, receiveOut, sendIn) })
	counts := make([]atomic.Int64, cell.Flows)
	var natRefused atomic.Int64
	cleanupWorkload := func() {}
	if cell.Tcp {
		cleanupWorkload = startWindowTcpWorkload(t, ctx, sender, receiver, counts, cell.Upload, &natRefused, cell.TcpBufferMax)
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
		producers := cell.Flows
		if cell.RoundRobinOffer {
			producers = max(1, min(cell.Lanes, cell.Flows))
		}
		for producer := range producers {
			workers.Go(func() {
				flow := producer
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
					if cell.RoundRobinOffer {
						flow += producers
						if flow >= cell.Flows {
							flow = producer
						}
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
	if cell.Tcp {
		// Inner TCP startup is visible well after the Transfer window settles.
		// Keep that ramp outside steady-state comparisons; the ledger retains
		// the warmup duration and per-second readings to expose later ramps.
		warmup += 2 * time.Second
	}
	if cell.Warmup > 0 {
		warmup = cell.Warmup
	}
	if os.Getenv("CONNECT_WINDOW_PACING_TRACE") != "" {
		traceStart := time.Now()
		for time.Since(traceStart) < warmup {
			time.Sleep(min(10*time.Millisecond, warmup-time.Since(traceStart)))
			bytes := int64(0)
			for i := range counts {
				bytes += counts[i].Load()
			}
			estimate := sender.DestinationSendStats(receiver.ClientId()).SendWindow
			t.Logf("pacing-trace at=%s bytes=%d window=%d rate=%d service=%d backlog=%t rtt=%s sampled=%t", time.Since(traceStart), bytes, estimate.Window, estimate.PacingByteRate, estimate.ServiceByteRate, estimate.ServiceBacklogged, estimate.RoundTrip, estimate.Sized)
		}
	} else {
		time.Sleep(warmup)
	}
	dropsBefore := dataLink.dropped.Load()
	if cell.Upload {
		dropsBefore = ackLink.dropped.Load()
	}
	before := make([]int64, len(counts))
	for i := range counts {
		before[i] = counts[i].Load()
	}
	start := time.Now()
	previousTime := start
	previousBytes := int64(0)
	var intervalMbps []float64
	for remaining := duration; remaining > 0; remaining = time.Until(start.Add(duration)) {
		time.Sleep(min(time.Second, remaining))
		now := time.Now()
		delivered := int64(0)
		for i := range counts {
			delivered += counts[i].Load() - before[i]
		}
		intervalMbps = append(intervalMbps, float64(delivered-previousBytes)*8/now.Sub(previousTime).Seconds()/1e6)
		previousTime, previousBytes = now, delivered
	}
	elapsed := time.Since(start)
	reading := windowPathReading{Cell: cell, WarmupSeconds: warmup.Seconds(), Seconds: elapsed.Seconds(), MinFlowMbps: 1e20, IntervalMbps: intervalMbps}
	for i := range counts {
		delivered := counts[i].Load() - before[i]
		reading.Bytes += delivered
		reading.MinFlowMbps = min(reading.MinFlowMbps, float64(delivered)*8/elapsed.Seconds()/1e6)
	}
	reading.Mbps = float64(reading.Bytes) * 8 / elapsed.Seconds() / 1e6
	reading.NatRefused = natRefused.Load()
	reading.RelayDrops = dataLink.dropped.Load()
	reading.MaxRelayQueued = dataLink.maxQueued.Load()
	reading.MaxRelayQueuedBytes = dataLink.maxQueuedBytes.Load()
	statsSource, statsDestination := sender, receiver.ClientId()
	if cell.Upload {
		statsSource, statsDestination = receiver, sender.ClientId()
		reading.RelayDrops = ackLink.dropped.Load()
		reading.MaxRelayQueued = ackLink.maxQueued.Load()
		reading.MaxRelayQueuedBytes = ackLink.maxQueuedBytes.Load()
	}
	reading.Recovery = statsSource.SendRecoveryStats()
	reading.Receiver = receiver.ReceiveStats()
	reading.SenderReceive = sender.ReceiveStats()
	reading.Window = statsSource.DestinationSendStats(statsDestination).SendWindow
	reading.MeasurementRelayDrops = reading.RelayDrops - dropsBefore
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
						case "ceiling", "constant", "matched", "path-rtt-only", "unpaced", "delivery":
						default:
							t.Fatalf("unknown performance arm %q", arm)
						}
						cell := windowPathCell{Tcp: tcp, Upload: upload, Arm: arm, RoundTrip: rtt, Compression: compression, Flows: count, Lanes: envInt(t, "CONNECT_WINDOW_PATH_LANES", 0), Payload: envInt(t, "CONNECT_WINDOW_PATH_PAYLOAD", 1280), Budget: mib(48), Rate: 125000000, Drop: os.Getenv("CONNECT_WINDOW_PATH_DROP") != ""}
						bufferMib := envInt(t, "CONNECT_WINDOW_TCP_BUFFER_MAX_MIB", 0)
						if bufferMib < 0 || bufferMib > 512 {
							t.Fatal("TCP buffer maximum must be 0 (default) or 1–512 MiB")
						}
						cell.TcpBufferMax = ByteCount(bufferMib) * mib(1)
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

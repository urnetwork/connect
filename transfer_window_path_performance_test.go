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
	rate             ByteCount
	rateAfter        ByteCount
	rateChangeAfter  time.Duration
	delay            time.Duration
	delayAfter       time.Duration
	delayChangeAfter time.Duration
	queueCount       int
	queueBytes       ByteCount
	dropOnFull       bool
	dropped          atomic.Int64
	maxQueued        atomic.Int64
	maxQueuedBytes   atomic.Int64
}

type windowPathFrame struct {
	bytes  []byte
	depart time.Time
	arrive time.Time
	delay  time.Duration
}

func (self *windowPathLink) run(ctx context.Context, from, to Route) {
	var queue []windowPathFrame
	head, serviced := 0, 0
	queuedBytes := ByteCount(0)
	departure := time.Now()
	changeAt := departure.Add(self.rateChangeAfter)
	delayChangeAt := departure.Add(self.delayChangeAfter)
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
				queue[index].arrive = departure.Add(queue[index].delay)
				if head < index && queue[index].arrive.Before(queue[index-1].arrive) {
					queue[index].arrive = queue[index-1].arrive
				}
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
			delay := self.delay
			if self.delayAfter > 0 && !now.Before(delayChangeAt) {
				delay = self.delayAfter
			}
			arrival := departure.Add(delay)
			// A changed propagation path remains FIFO: frames already in
			// flight retain their deadline, including across a shorter path.
			if head < len(queue) && arrival.Before(queue[len(queue)-1].arrive) {
				arrival = queue[len(queue)-1].arrive
			}
			queue = append(queue, windowPathFrame{bytes: frame, depart: departure, arrive: arrival, delay: delay})
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
// Both arms use the FIFO's known physical RTT for sizing, so service residence
// learned from compressed replies cannot silently restore the omitted term.
// The control changes only that window term; service pacing still observes
// the real receiver compression in both arms. A 256 KiB opening requires
// measured growth to cover compression; a retained 2 MiB opening already fits.
func TestWindowCompressionResidenceGrowsSmallOpening(t *testing.T) {
	assertMessagePoolOwnership(t)
	var old, fixed float64
	for _, arm := range []string{"path-rtt-only", "delivery"} {
		synctest.Test(t, func(t *testing.T) {
			reading := measureWindowPathCell(t, windowPathCell{
				Arm: arm, RoundTrip: 300 * time.Microsecond, Compression: 10 * time.Millisecond,
				Flows: 1, Payload: 1280, Budget: mib(48), Rate: 125000000,
				KnownPathRoundTrip: true, HoldFullAckCompression: true,
				BootstrapWindow: 256 * 1024,
			}, 100*time.Millisecond)
			logWindowServiceReading(t, reading)
			wantResidence := 10300 * time.Microsecond
			if arm == "path-rtt-only" {
				old = reading.Mbps
				wantResidence = 300 * time.Microsecond
			} else {
				fixed = reading.Mbps
			}
			if reading.Window.Initial != 256*1024 || reading.Window.RoundTrip != 300*time.Microsecond || reading.Window.WindowRoundTrip != wantResidence {
				t.Fatalf("%s did not isolate the compression window term: %+v", arm, reading.Window)
			}
			t.Logf("%s: %.1f model Mb/s, window=%d reason=%s", arm, reading.Mbps, reading.Window.Window, reading.Window.Reason)
		})
	}
	if old <= 0 || old >= 400 || fixed < 850 {
		t.Fatalf("short-path residence control: old=%.1f fixed=%.1f model Mb/s", old, fixed)
	}
}

// Every reported/design-point RTT is exercised with one/eight offered flows
// and immediate/compressed ACKs. A constant window with the same memory and
// receiver bounds calibrates the fixture's attainable payload rate per cell.
func TestWindowPathDeterministicPerformanceMatrix(t *testing.T) {
	assertMessagePoolOwnership(t)
	roundTrips := envDurations(t, "CONNECT_WINDOW_MODEL_RTT_US", time.Microsecond, windowDeterministicRoundTrips())
	for _, cell := range windowDeterministicPerformanceCells(roundTrips, os.Getenv("CONNECT_WINDOW_MODEL_RTT_US") != "") {
		var ceiling, fixed windowPathReading
		for _, arm := range []string{"ceiling", "delivery"} {
			synctest.Test(t, func(t *testing.T) {
				trial := cell
				trial.Arm = arm
				reading := measureWindowPathCell(t, trial, max(100*time.Millisecond, 2*cell.RoundTrip))
				if arm == "ceiling" {
					ceiling = reading
				} else {
					fixed = reading
				}
			})
		}
		t.Logf("rtt=%s flows=%d compression=%s ceiling=%.1f fixed=%.1f min-flow=%.1f model Mb/s", cell.RoundTrip, cell.Flows, cell.Compression, ceiling.Mbps, fixed.Mbps, fixed.MinFlowMbps)
		if ceiling.Mbps < 500 || fixed.Mbps < .85*ceiling.Mbps || fixed.MinFlowMbps == 0 || fixed.RelayDrops != 0 {
			t.Errorf("uncalibrated or underperforming model cell: rtt=%s flows=%d compression=%s ceiling=%.1f fixed=%.1f min-flow=%.1f drops=%d", cell.RoundTrip, cell.Flows, cell.Compression, ceiling.Mbps, fixed.Mbps, fixed.MinFlowMbps, fixed.RelayDrops)
			encoded, _ := json.Marshal(fixed)
			t.Log(string(encoded))
		}
	}
}

// A large configured opening creates pressure at the finite forwarding queue.
// Both arms use that same opening and startup allowance; only pacing differs.
// Carrier reliability cannot recover a Pack discarded after its first hop.
func TestWindowPathBoundsBurstsAtFiniteRelay(t *testing.T) {
	assertMessagePoolOwnership(t)
	for _, flows := range []int{1, 8} {
		for _, arm := range []string{"unpaced", "delivery"} {
			synctest.Test(t, func(t *testing.T) {
				reading := measureWindowPathCell(t, windowPathCell{Arm: arm, RoundTrip: 100 * time.Millisecond, Compression: 10 * time.Millisecond, Flows: flows, Payload: 1280, Budget: mib(48), BootstrapWindow: mib(48), Rate: 125000000, Drop: true}, time.Second)
				if reading.Window.Initial != mib(48) {
					t.Fatal("finite relay control did not use the same large opening")
				}
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
	// Optional explicit opening for a controlled comparison. It does not
	// change byte permissions, the serializer, or measurement duration.
	BootstrapWindow          ByteCount                  `json:",omitempty"`
	InitialLogicalAckRelease time.Duration              `json:",omitempty"`
	HoldFullAckCompression   bool                       `json:",omitempty"`
	SenderProfile            *windowPathEndpointProfile `json:",omitempty"`
	ReceiverProfile          *windowPathEndpointProfile `json:",omitempty"`
	ProfileFixtureSha256     string                     `json:",omitempty"`
	Bidirectional            bool                       `json:",omitempty"`
	CalibrationWindow        ByteCount
	SendWindow               ByteCount
	ReceiveWindow            ByteCount
	ReceiveWindowAfter       ByteCount
	WindowChangeAfter        time.Duration
	Tcp                      bool
	Upload                   bool
	TcpBufferMax             ByteCount
	Arm                      string
	RoundTrip                time.Duration
	RoundTripAfter           time.Duration
	RoundTripChangeAfter     time.Duration
	QualityChanged           bool
	Compression              time.Duration
	Flows                    int
	RoundRobinOffer          bool
	Lanes                    int
	Payload                  int
	Budget                   ByteCount
	Drop                     bool
	Rate                     ByteCount
	RateAfter                ByteCount
	RateChangeAfter          time.Duration
	Warmup                   time.Duration
	PacingWakeDelay          time.Duration
	KnownPathRoundTrip       bool
}

type windowPathReading struct {
	Cell                  windowPathCell
	WarmupSeconds         float64
	Seconds               float64
	Bytes                 int64
	Mbps                  float64
	MinFlowMbps           float64
	IntervalMbps          []float64
	DirectionMbps         []float64 `json:",omitempty"`
	RelayDrops            int64
	MeasurementRelayDrops int64
	NatRefused            int64
	MaxRelayQueued        int64
	MaxRelayQueuedBytes   int64
	Recovery              ClientSendRecoveryStatsSnapshot
	Receiver              ClientReceiveStatsSnapshot
	SenderReceive         ClientReceiveStatsSnapshot
	Window                SendWindowEstimate
	ReverseWindow         SendWindowEstimate `json:",omitzero"`
}

// Isolates the window's compression-residence term by making both control
// arms use the receiver's full advertised delay. Early H1 heads otherwise
// change the counterfactual's feedback cadence as well as its window term.
// Only this explicitly selected fixture holds a dedicated ACK worker.
func holdWindowPathAckCompression(ctx context.Context, t *testing.T, settings *ReceiveBufferSettings) {
	t.Helper()
	compression := settings.AckCompressTimeout
	if compression <= 0 {
		t.Fatal("full-compression control requires a positive advertised interval")
	}
	var stateLock sync.Mutex
	deadlineTimes := map[receiveSequenceId]time.Time{}
	settings.afterAckWriteForTest = func(id receiveSequenceId) {
		now := time.Now()
		stateLock.Lock()
		previous := deadlineTimes[id]
		deadlineTimes[id] = now.Add(compression)
		stateLock.Unlock()
		if !previous.IsZero() && now.Before(previous) && ctx.Err() == nil {
			t.Errorf("full-compression control wrote after %s, before its %s interval", now.Sub(previous.Add(-compression)), compression)
		}
	}
	settings.beforeAckCompressWaitForTest = func(id receiveSequenceId) {
		stateLock.Lock()
		deadline := deadlineTimes[id]
		stateLock.Unlock()
		if deadline.IsZero() || !time.Now().Before(deadline) {
			return
		}
		timer := time.NewTimer(time.Until(deadline))
		defer timer.Stop()
		select {
		case <-ctx.Done():
		case <-timer.C:
		}
	}
}

// Measures receiver bytes over one common interval. Construction, warmup and
// draining are outside the interval. This is a Transfer/FIFO instrument, not
// a measurement of H1 sockets, a native kernel TUN, or provider TCP.
func measureWindowPathCell(t *testing.T, cell windowPathCell, duration time.Duration) windowPathReading {
	t.Helper()
	if cell.Bidirectional && cell.Tcp {
		t.Fatal("bidirectional fixture currently requires the raw Transfer workload")
	}
	if (cell.SenderProfile == nil) != (cell.ReceiverProfile == nil) {
		t.Fatal("both endpoint profiles are required")
	}
	ctx, cancel := context.WithCancel(context.Background())
	fixtureStart := time.Now()
	var firstAckLanes atomic.Uint32
	settings := func(profile *windowPathEndpointProfile) *ClientSettings {
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
		if cell.KnownPathRoundTrip {
			s.SendBufferSettings.windowRoundTripOverrideForTest = &cell.RoundTrip
		}
		if cell.PacingWakeDelay > 0 {
			s.SendBufferSettings.afterWindowPacingWaitForTest = func() { time.Sleep(cell.PacingWakeDelay) }
		}
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
		if profile != nil {
			profile.apply(s)
			if s.MinimumMessageLenLimit() != profile.MinimumMessageLimit {
				t.Fatal("profile message limit differs from the constructor capture")
			}
		}
		if cell.BootstrapWindow > 0 {
			if profile != nil {
				t.Fatal("explicit opening cannot overwrite a captured SDK profile")
			}
			s.SendBufferSettings.ResendQueueMaxByteCount = cell.BootstrapWindow
		}
		if cell.HoldFullAckCompression {
			if cell.InitialLogicalAckRelease > 0 {
				t.Fatal("full-compression control cannot also force an initial ACK release")
			}
			holdWindowPathAckCompression(ctx, t, s.ReceiveBufferSettings)
		}
		if cell.InitialLogicalAckRelease > 0 && profile != nil && profile.LogicalDataLanes == 0 {
			release := fixtureStart.Add(cell.InitialLogicalAckRelease)
			s.ReceiveBufferSettings.afterAckWriterOpenForTest = func(id receiveSequenceId, _ MultiRouteWriter) {
				if id.LogicalLane == 0 {
					return
				}
				if !time.Now().Before(release) {
					t.Errorf("lane %d missed initial ACK barrier at %s", id.LogicalLane, time.Since(fixtureStart))
					return
				}
				select {
				case <-ctx.Done():
				case <-time.After(time.Until(release)):
				}
			}
			s.ReceiveBufferSettings.afterAckWriteForTest = func(id receiveSequenceId) {
				if id.LogicalLane == 0 || id.LogicalLane > 8 {
					return
				}
				bit := uint32(1) << (id.LogicalLane - 1)
				if firstAckLanes.Or(bit)&bit != 0 {
					return
				}
				if time.Now() != release {
					t.Errorf("lane %d first ACK at %s, want %s", id.LogicalLane, time.Since(fixtureStart), cell.InitialLogicalAckRelease)
				}
			}
		}
		return s
	}
	senderSettings, receiverSettings := settings(cell.SenderProfile), settings(cell.ReceiverProfile)
	if cell.SenderProfile != nil && cell.Arm == "ceiling" {
		senderSettings.SendBufferSettings.ResendQueueMaxByteCount = cell.SenderProfile.windowLimit(cell.ReceiverProfile)
		receiverSettings.SendBufferSettings.ResendQueueMaxByteCount = cell.ReceiverProfile.windowLimit(cell.SenderProfile)
	}
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
	for _, link := range []*windowPathLink{&dataLink, &ackLink} {
		link.delayAfter = cell.RoundTripAfter / 2
		link.delayChangeAfter = cell.RoundTripChangeAfter
	}
	if cell.Upload && !cell.Bidirectional {
		dataLink.rate = 0
		dataLink.rateAfter = 0
		dataLink.dropOnFull = false
		ackLink.rate = cell.Rate
		ackLink.rateAfter = cell.RateAfter
		ackLink.rateChangeAfter = cell.RateChangeAfter
		ackLink.dropOnFull = cell.Drop
	}
	if cell.Bidirectional {
		ackLink.rate, ackLink.rateAfter, ackLink.rateChangeAfter = cell.Rate, cell.RateAfter, cell.RateChangeAfter
		ackLink.dropOnFull = cell.Drop
	}
	workers.Go(func() { dataLink.run(ctx, sendOut, receiveIn) })
	workers.Go(func() { ackLink.run(ctx, receiveOut, sendIn) })
	if cell.QualityChanged {
		workers.Go(func() {
			changeAfter := cell.RoundTripChangeAfter
			if cell.RateChangeAfter > 0 && (changeAfter <= 0 || cell.RateChangeAfter < changeAfter) {
				changeAfter = cell.RateChangeAfter
			}
			select {
			case <-ctx.Done():
			case <-time.After(changeAfter):
				// Both app endpoints observe this modeled interface change.
				// Congestion-only cells deliberately receive no notification.
				at := time.Now()
				sender.sendBuffer.networkQualityChanged(Id{}, at)
				receiver.sendBuffer.networkQualityChanged(Id{}, at)
			}
		})
	}
	directions := 1
	if cell.Bidirectional {
		directions = 2
	}
	counts := make([]atomic.Int64, directions*cell.Flows)
	var natRefused atomic.Int64
	cleanupWorkload := func() {}
	if cell.Tcp {
		cleanupWorkload = startWindowTcpWorkload(t, ctx, sender, receiver, counts, cell.Upload, &natRefused, cell.TcpBufferMax)
	} else {
		for direction := range directions {
			source, destination, lanes := sender, receiver, senderSettings.SendBufferSettings.LogicalDataLaneCount
			if cell.Upload != (direction == 1) {
				source, destination, lanes = receiver, sender, receiverSettings.SendBufferSettings.LogicalDataLaneCount
			}
			destination.AddReceiveCallback(func(_ TransferPath, frames []*protocol.Frame, _ Peer) {
				for _, frame := range frames {
					if len(frame.MessageBytes) != 0 {
						flow := int(frame.MessageBytes[0])
						if flow < cell.Flows {
							counts[direction*cell.Flows+flow].Add(int64(len(frame.MessageBytes)))
						}
					}
				}
			})
			producers := cell.Flows
			if cell.RoundRobinOffer {
				producers = max(1, min(lanes, cell.Flows))
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
						if lanes > 0 {
							key.LogicalLane = uint32(flow%lanes + 1)
						}
						if ok, err := source.SendWithTimeoutDetailed(frame, destination.ClientId(), nil, -1, key); !ok {
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
		for _, settings := range []*ClientSettings{senderSettings, receiverSettings} {
			for _, budget := range []*TransferMemoryBudget{settings.SendBufferSettings.ResendQueueBudget,
				settings.ReceiveBufferSettings.ReceiveQueueBudget, settings.ReceiveBufferSettings.PackQueueBudget} {
				if budget != nil && budget.UsedByteCount() != 0 {
					t.Errorf("performance endpoint retained %d budget bytes after close", budget.UsedByteCount())
				}
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
		// Trace both data and inner-TCP feedback through the measured interval.
		// Copy statistics before reading their mean so tracing cannot advance
		// the production ring or hold a service lock while writing test output.
		workers.Go(func() {
			for {
				select {
				case <-ctx.Done():
					return
				case <-time.After(10 * time.Millisecond):
				}
				now := time.Now()
				bytes := int64(0)
				for i := range counts {
					bytes += counts[i].Load()
				}
				phase := "warmup"
				if now.Sub(traceStart) >= warmup {
					phase = "measurement"
				}
				for _, direction := range []struct {
					name        string
					client      *Client
					destination Id
				}{
					{name: "forward", client: sender, destination: receiver.ClientId()},
					{name: "reverse", client: receiver, destination: sender.ClientId()},
				} {
					estimate := direction.client.DestinationSendStats(direction.destination).SendWindow
					t.Logf("pacing-trace arm=%s configured-rtt=%s phase=%s direction=%s at=%s bytes=%d window=%d learned=%d candidate=%d initial=%d floor=%d ceiling=%d obtainable=%d rate=%d service=%d delivery=%d backlog=%t rtt=%s residence=%s sampled=%t service-sized=%t established=%t probe-rate=%d probe-bytes=%d reason=%q", cell.Arm, cell.RoundTrip, phase, direction.name, now.Sub(traceStart), bytes, estimate.Window, estimate.LearnedWindow, estimate.CandidateWindow, estimate.Initial, estimate.Floor, estimate.Ceiling, estimate.Obtainable, estimate.PacingByteRate, estimate.ServiceByteRate, estimate.DeliveryByteRate, estimate.ServiceBacklogged, estimate.RoundTrip, estimate.WindowRoundTrip, estimate.Sized, estimate.ServiceSized, estimate.ServiceEstablished, estimate.PacingProbeByteRate, estimate.PacingProbeByteCount, estimate.Reason)
					var services []*windowPacingService
					func() {
						direction.client.sendBuffer.mutex.Lock()
						defer direction.client.sendBuffer.mutex.Unlock()
						for _, service := range direction.client.sendBuffer.windowPacingServices {
							services = append(services, service)
						}
					}()
					for _, service := range services {
						var snapshot struct {
							minimum, latest, compression, debt, drain, cooldown time.Duration
							outstanding, bound, credit, mean                    float64
							sent, applied, drained, reserved, limit             ByteCount
							waits, tails, buckets                               int
							burst                                               uint64
						}
						func() {
							service.stateLock.Lock()
							defer service.stateLock.Unlock()
							snapshot.minimum, snapshot.latest, snapshot.compression = service.minRoundTrip, service.latestRoundTrip, service.compression
							snapshot.outstanding, snapshot.bound = service.outstandingWithLock(), service.flightBoundWithLock(estimate.ServiceByteRate)
							snapshot.sent, snapshot.applied, snapshot.drained, snapshot.reserved = service.sent, service.total, service.drainedSent, service.reservedByteCount
							snapshot.waits, snapshot.tails, snapshot.limit, snapshot.credit = service.pacingReservations, service.pendingWrites, service.burstMeter.limit, service.burstMeter.available
							snapshot.burst = service.dispatchBurst.number
							snapshot.debt = max(0, service.next.Sub(now))
							snapshot.drain, snapshot.cooldown = max(0, service.drainUntil.Sub(now)), max(0, service.drainCheckAt.Sub(now))
							if ring := service.roundTripStats.ring; ring != nil {
								copy := *ring
								copy.buckets = append([]windowStatsBucket(nil), ring.buckets...)
								snapshot.mean, snapshot.buckets = copy.mean(now)
							}
						}()
						t.Logf("pacing-service arm=%s configured-rtt=%s phase=%s direction=%s at=%s minimum=%s latest=%s compression=%s outstanding=%.0f bound=%.0f sent=%d applied=%d drained=%d reserved=%d waits=%d tails=%d burst=%d burst-limit=%d credit=%.0f debt=%s drain=%s cooldown=%s ring-mean-ns=%.0f ring-buckets=%d",
							cell.Arm, cell.RoundTrip, phase, direction.name, now.Sub(traceStart), snapshot.minimum, snapshot.latest, snapshot.compression, snapshot.outstanding, snapshot.bound,
							snapshot.sent, snapshot.applied, snapshot.drained, snapshot.reserved, snapshot.waits, snapshot.tails, snapshot.burst, snapshot.limit, snapshot.credit, snapshot.debt, snapshot.drain, snapshot.cooldown, snapshot.mean, snapshot.buckets)
					}
				}
			}
		})
	}
	time.Sleep(warmup)
	dropsBefore := dataLink.dropped.Load()
	if cell.Upload {
		dropsBefore = ackLink.dropped.Load()
	}
	if cell.Bidirectional {
		dropsBefore = dataLink.dropped.Load() + ackLink.dropped.Load()
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
	reading := windowPathReading{Cell: cell, WarmupSeconds: warmup.Seconds(), Seconds: elapsed.Seconds(), MinFlowMbps: 1e20, IntervalMbps: intervalMbps,
		DirectionMbps: make([]float64, directions)}
	for i := range counts {
		delivered := counts[i].Load() - before[i]
		reading.Bytes += delivered
		reading.MinFlowMbps = min(reading.MinFlowMbps, float64(delivered)*8/elapsed.Seconds()/1e6)
		reading.DirectionMbps[i/cell.Flows] += float64(delivered) * 8 / elapsed.Seconds() / 1e6
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
	if cell.Bidirectional {
		reading.RelayDrops = dataLink.dropped.Load() + ackLink.dropped.Load()
		reading.MaxRelayQueued = max(dataLink.maxQueued.Load(), ackLink.maxQueued.Load())
		reading.MaxRelayQueuedBytes = max(dataLink.maxQueuedBytes.Load(), ackLink.maxQueuedBytes.Load())
		reading.ReverseWindow = receiver.DestinationSendStats(sender.ClientId()).SendWindow
		if cell.Upload {
			reading.ReverseWindow = sender.DestinationSendStats(receiver.ClientId()).SendWindow
		}
	}
	reading.MeasurementRelayDrops = reading.RelayDrops - dropsBefore
	// The barrier supports up to eight logical lanes; a single-flow cell
	// must prove its one initial ACK without requiring seven inactive lanes.
	expectedFirstAckLanes := uint32(1<<min(cell.Flows, 8)) - 1
	if cell.InitialLogicalAckRelease > 0 && firstAckLanes.Load() != expectedFirstAckLanes {
		t.Errorf("initial feedback ordering missed logical lanes: mask=%02x want=%02x", firstAckLanes.Load(), expectedFirstAckLanes)
	}
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
					if len(comparison.FailureReasons) != 0 {
						t.Errorf("performance comparison failed: %s", strings.Join(comparison.FailureReasons, "; "))
					}
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
	DeliveryOfMatched   float64
	CensoredReasons     []string
	FailureReasons      []string
}

// A/A drift and an unattained calibration ceiling prevent treating host load
// or a capped fixture as evidence for a window-policy change. All raw runs
// remain in the ledger, including censored and stalled observations.
func compareWindowPathReadings(readings []windowPathReading) windowPathComparison {
	if len(readings) == 0 {
		return windowPathComparison{CensoredReasons: []string{"missing readings"}}
	}
	comparison := windowPathComparison{Cell: readings[0].Cell}
	comparison.Cell.Arm = "comparison"
	matched, ceilings, candidates := 0, 0, 0
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
			ceilings++
		case "delivery":
			comparison.DeliveryMbps = reading.Mbps
			candidates++
			if reading.MeasurementRelayDrops != 0 || reading.NatRefused != 0 ||
				reading.Receiver.ReceiveQueueEvictionCount != 0 || reading.SenderReceive.ReceiveQueueEvictionCount != 0 {
				comparison.FailureReasons = append(comparison.FailureReasons, "candidate lost delivery at a measured admission boundary")
			}
		}
		if reading.Bytes == 0 || reading.MinFlowMbps == 0 {
			comparison.FailureReasons = append(comparison.FailureReasons, "stalled flow in "+reading.Cell.Arm)
		}
	}
	if candidates != 1 {
		comparison.CensoredReasons = append(comparison.CensoredReasons, "expected one candidate reading")
	}
	if ceilings != 1 {
		comparison.CensoredReasons = append(comparison.CensoredReasons, "expected one ceiling reading")
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
		if candidates == 1 && ceilings == 1 && comparison.DeliveryOfCeiling < .9 {
			comparison.FailureReasons = append(comparison.FailureReasons, "candidate below 90 percent of measured ceiling")
		}
	}
	// Even an instrument capped below the link rate can expose a regression
	// against its unchanged-window control. Censoring never erases that result.
	if matched >= 2 && min(comparison.MatchedFirstMbps, comparison.MatchedLastMbps) > 0 {
		comparison.DeliveryOfMatched = comparison.DeliveryMbps / min(comparison.MatchedFirstMbps, comparison.MatchedLastMbps)
		if candidates == 1 && comparison.DeliveryOfMatched < .9 {
			comparison.FailureReasons = append(comparison.FailureReasons, "candidate below 90 percent of both matched controls")
		}
	}
	if comparison.CeilingMbps < .9*float64(comparison.Cell.Rate)*8/1e6 {
		comparison.CensoredReasons = append(comparison.CensoredReasons, "calibration below 90 percent of link rate")
	}
	return comparison
}

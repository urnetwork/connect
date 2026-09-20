package connect

import (
	"context"
	"testing"
)

func TestP2pStreamProbeProgressTraceMetadata(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	settings := DefaultP2pTransportSettings()
	observer, events := progressTraceTestObserver()
	settings.ProgressObserver = observer
	stream, nonce := NewId(), NewId()
	probe := newStoppedP2pStreamProbe(ctx, NewRouteManager(ctx, "probe-progress"), stream, settings)
	probe.observe(P2pStreamProbeEventRequestDropped, nonce, 7)
	event := <-events
	if event.Stage != "p2p_probe_request-dropped" || event.SequenceId != stream || event.MessageId != nonce || event.SequenceNumber != 7 || event.AtUnixNano == 0 || event.WireHash != 0 {
		t.Fatalf("probe event lost metadata: %+v", event)
	}
}

// Some defaults were once one package constant read by several settings. Each
// owner's defaults constructor now holds its own value, so the values that
// must agree are checked here rather than shared.
package connect

import "testing"

func TestTransferQueueDepthDefaultsAgree(t *testing.T) {
	client := DefaultClientSettings()
	depth := client.SendBufferSize
	if depth <= 0 {
		t.Fatalf("default client transfer queue depth=%d", depth)
	}
	for name, got := range map[string]int{
		"client forward buffer":            client.ForwardBufferSize,
		"standalone send sequence buffer":  DefaultSendBufferSettings().SequenceBufferSize,
		"standalone send ack buffer":       DefaultSendBufferSettings().AckBufferSize,
		"standalone forward buffer":        DefaultForwardBufferSettings().SequenceBufferSize,
		"standalone contract manager":      DefaultContractManagerSettings().SequenceBufferSize,
		"multi-client sequence buffer":     DefaultMultiClientSettings().SequenceBufferSize,
		"client send sequence buffer":      client.SendBufferSettings.SequenceBufferSize,
		"client contract manager sequence": client.ContractManagerSettings.SequenceBufferSize,
	} {
		if got != depth {
			t.Errorf("%s depth=%d, want the client's %d", name, got, depth)
		}
	}
}

func TestReceiveQueueDepthCoversOneUnreliableFlight(t *testing.T) {
	flight := DefaultSendBufferSettings().UnreliableMaximumFlightMessageCount
	if got := DefaultReceiveBufferSettings().SequenceBufferSize; got != flight {
		t.Fatalf("default receive sequence depth=%d, want one unreliable flight of %d", got, flight)
	}
	if got := DefaultClientSettings().ReceiveBufferSettings.SequenceBufferSize; got != flight {
		t.Fatalf("default client receive sequence depth=%d, want %d", got, flight)
	}
}

func TestBlackholeFallbackMatchesTheTransportReadTimeout(t *testing.T) {
	readTimeout := DefaultPlatformTransportSettings().ReadTimeout
	if got := DefaultMultiClientSettings().BlackholeTimeout; got != readTimeout {
		t.Fatalf("default blackhole timeout=%s, want the transport read timeout %s", got, readTimeout)
	}
	if got := (&multiClientChannel{}).noSendAckTimeout(); got != readTimeout {
		t.Fatalf("no-send-ACK timeout without settings=%s, want %s", got, readTimeout)
	}
}

func TestEvaluationFailureLinesMatchTheEgressDialCadence(t *testing.T) {
	if got := DefaultMultiClientSettings().EvaluationFailureLogInterval; got != controlDialLogInterval {
		t.Fatalf("evaluation failure log interval=%s, want the egress-dial cadence %s", got, controlDialLogInterval)
	}
}

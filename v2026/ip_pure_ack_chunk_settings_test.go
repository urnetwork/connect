// Pure-feedback count policy is explicit and retains existing byte and carrier bounds.
package connect

import "testing"

// The class-specific cap is independent only when positive; zero-value owners
// and disabled overrides keep the legacy shared frame policy without expansion.
func TestPureTcpAckChunkSettingsRespectIndependentBounds(t *testing.T) {
	defaults := DefaultSendBufferChunkSettings()
	if defaults.PureTcpAckMaxFrames != sendPackH1GroupMaxFrames || DefaultSendBufferSettings().ChunkSettings != defaults {
		t.Fatal("constructor did not own the bounded pure-feedback default")
	}
	for _, c := range []struct {
		name     string
		settings SendBufferChunkSettings
		pure     int
		ordinary int
		bytes    ByteCount
	}{
		{name: "default", settings: defaults, pure: 16, ordinary: 2, bytes: DefaultMtu},
		{name: "manual-zero", settings: SendBufferChunkSettings{}, pure: 2, ordinary: 2, bytes: DefaultMtu},
		{name: "disabled-negative", settings: SendBufferChunkSettings{PureTcpAckMaxFrames: -1}, pure: 2, ordinary: 2, bytes: DefaultMtu},
		{name: "shared-one-independent-pure", settings: func() SendBufferChunkSettings { s := defaults; s.SharedMaxFrames = 1; return s }(), pure: 16, ordinary: 1, bytes: DefaultMtu},
		{name: "shared-one-pure-disabled", settings: SendBufferChunkSettings{SharedMaxFrames: 1}, pure: 1, ordinary: 1, bytes: DefaultMtu},
		{name: "explicit-pure-one", settings: func() SendBufferChunkSettings { s := defaults; s.PureTcpAckMaxFrames = 1; return s }(), pure: 1, ordinary: 2, bytes: DefaultMtu},
		{name: "fixed-storage", settings: func() SendBufferChunkSettings { s := defaults; s.PureTcpAckMaxFrames = 100; return s }(), pure: 16, ordinary: 2, bytes: DefaultMtu},
		{name: "lower-byte-cap", settings: func() SendBufferChunkSettings { s := defaults; s.SharedMaxMessageByteCount = 80; return s }(), pure: 16, ordinary: 2, bytes: 80},
	} {
		for _, pure := range []bool{false, true} {
			frames, bytes := c.settings.logicalGroupLimits(transferFlightPolicySnapshot{}, pure)
			want := c.ordinary
			if pure {
				want = c.pure
			}
			if frames != want || bytes != c.bytes {
				t.Fatalf("%s pure=%t got=%d/%d want=%d/%d", c.name, pure, frames, bytes, want, c.bytes)
			}
		}
		for _, policy := range []transferFlightPolicySnapshot{{h1Only: true}, {h1Only: true, h1MessageLimitsKnown: true, h1MaxMessageByteCount: 512}} {
			frames, bytes := c.settings.logicalGroupLimits(policy, true)
			oldFrames, oldBytes := c.settings.chunkLimits(policy)
			if frames != oldFrames || bytes != oldBytes {
				t.Fatalf("%s changed H1 policy: got=%d/%d old=%d/%d", c.name, frames, bytes, oldFrames, oldBytes)
			}
		}
	}
}

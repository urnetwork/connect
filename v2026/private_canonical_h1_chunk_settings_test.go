// Owner-scoped chunk policy controls use real signed-contract fixtures and
// existing raw-frame witnesses; zero values never grant unbounded batching.
package connect

import (
	"bytes"
	"testing"
	"testing/synctest"
	"time"
)

// Existing compatibility controls name the historical byte policy. These are
// test expectations only; production defaults live in the settings constructor.
const (
	sendPackBatchMaxMessageByteCount         = DefaultMtu
	sendPackH1GroupMaxMessageByteCount       = 3 * 1024
	sendPackH1EstablishedMaxMessageByteCount = 3 * DefaultMtu
)

// Constructor and manual-zero owners keep the intended compatibility policy.
func TestCanonicalH1ChunkSettingsDefaultAndZeroCompatibility(t *testing.T) {
	defaults := DefaultSendBufferChunkSettings()
	if DefaultSendBufferSettings().ChunkSettings != defaults || defaults.SharedMaxFrames != 2 ||
		defaults.SharedMaxMessageByteCount != DefaultMtu || defaults.H1KnownMaxFrames != 8 ||
		defaults.H1KnownMaxMessageByteCount != 8*DefaultMtu || defaults.H1UnknownMaxFrames != 16 ||
		defaults.H1UnknownMaxMessageByteCount != 3*1024 || defaults.H1EstablishedMaxMessageByteCount != 3*DefaultMtu {
		t.Fatal("sender constructor changed the intended shared/legacy/known-H1 defaults")
	}
	known := transferFlightPolicySnapshot{h1Only: true, h1MessageLimitsKnown: true, h1MaxMessageByteCount: 16 * 1024}
	for _, value := range []int{0, -1} {
		settings := SendBufferChunkSettings{SharedMaxFrames: value, SharedMaxMessageByteCount: ByteCount(value),
			H1KnownMaxFrames: value, H1KnownMaxMessageByteCount: ByteCount(value),
			H1UnknownMaxFrames: value, H1UnknownMaxMessageByteCount: ByteCount(value), H1EstablishedMaxMessageByteCount: ByteCount(value)}
		if settings.knownH1Enabled(known) {
			t.Fatal("nonpositive known-H1 setting enabled enlargement")
		}
		for _, row := range []struct {
			policy    transferFlightPolicySnapshot
			frames    int
			byteCount ByteCount
		}{
			{policy: transferFlightPolicySnapshot{}, frames: 2, byteCount: DefaultMtu},
			{policy: transferFlightPolicySnapshot{h1Only: true}, frames: 16, byteCount: 3 * 1024},
			{policy: known, frames: 16, byteCount: 3 * 1024},
		} {
			frames, byteCount := settings.chunkLimits(row.policy)
			if frames != row.frames || byteCount != row.byteCount {
				t.Fatalf("nonpositive settings changed compatibility: %d/%d want%d/%d", frames, byteCount, row.frames, row.byteCount)
			}
		}
		if settings.establishedByteLimit() != 3*DefaultMtu {
			t.Fatal("nonpositive legacy established cap did not use its one default")
		}
	}
	for _, disableCount := range []bool{false, true} {
		settings := defaults
		if disableCount {
			settings.H1KnownMaxFrames = 0
		} else {
			settings.H1KnownMaxMessageByteCount = 0
		}
		if settings.knownH1Enabled(known) {
			t.Fatal("one disabled known-H1 bound did not disable enlargement")
		}
	}
}

// Positive lower caps survive normalization; storage remains a hard bound.
func TestCanonicalH1ChunkSettingsLowerLimitsAndStorageBound(t *testing.T) {
	settings := DefaultSendBufferChunkSettings()
	settings.SharedMaxFrames, settings.SharedMaxMessageByteCount = 1, 128
	settings.H1UnknownMaxFrames, settings.H1UnknownMaxMessageByteCount = 2, 512
	settings.H1KnownMaxFrames, settings.H1KnownMaxMessageByteCount = 3, 3*DefaultMtu
	settings.H1EstablishedMaxMessageByteCount = 256
	known := transferFlightPolicySnapshot{h1Only: true, h1MessageLimitsKnown: true, h1MaxMessageByteCount: 16 * 1024}
	for _, row := range []struct {
		policy    transferFlightPolicySnapshot
		frames    int
		byteCount ByteCount
	}{
		{policy: transferFlightPolicySnapshot{}, frames: 1, byteCount: 128},
		{policy: transferFlightPolicySnapshot{h1Only: true}, frames: 2, byteCount: 512},
		{policy: known, frames: 3, byteCount: 3 * DefaultMtu},
	} {
		frames, byteCount := settings.chunkLimits(row.policy)
		if frames != row.frames || byteCount != row.byteCount {
			t.Fatalf("owner lower limit lost: %d/%d want%d/%d", frames, byteCount, row.frames, row.byteCount)
		}
	}
	if settings.establishedByteLimit() != 256 {
		t.Fatal("positive lower established cap was replaced by the default")
	}
	settings.SharedMaxFrames, settings.H1UnknownMaxFrames, settings.H1KnownMaxFrames = 100, 100, 100
	for _, policy := range []transferFlightPolicySnapshot{{}, {h1Only: true}, known} {
		if frames, _ := settings.chunkLimits(policy); frames != 16 {
			t.Fatal("owner count escaped fixed completion storage", frames)
		}
	}
}

// Five real senders retain independent owner policy. Every frame and terminal
// callback crosses its real receiver; full proofs narrow only their own chunks.
func TestCanonicalH1ChunkSettingsRealOwnersStayIndependent(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		type owner struct {
			fixture  *privateH1ProofFixture
			pack     SendPack
			result   chan error
			chunks   []int
			admitted bool
		}
		owners := make([]owner, 5)
		for index := range owners {
			configure := func(settings *SendBufferSettings) {
				if index == 1 {
					settings.ChunkSettings.H1KnownMaxFrames = 3
				}
				if index == 2 {
					settings.ChunkSettings.H1KnownMaxMessageByteCount = 3 * DefaultMtu
				}
				if index == 3 {
					settings.ChunkSettings.H1KnownMaxFrames = 0
				}
			}
			fixture := privateH1NewFixture(t, 2, mib(1), false, TransportTypeH1, configure)
			if index == 4 {
				authority := fixture.sender.ClientOob().(*privateH1ProofAuthority)
				if authority.grants.Load() != 0 {
					t.Fatal("compact-proof owner already published a peer contract")
				}
				// The authority still signs and HMACs its actual compact grant.
				authority.chain = nil
			}
			owners[index] = owner{fixture: fixture, pack: SendPack{Frames: fixture.frames(8, 2)}, result: make(chan error, 2)}
			row := &owners[index]
			t.Cleanup(func() {
				if !row.admitted {
					for _, frame := range row.pack.Frames {
						MessagePoolReturn(frame.MessageBytes)
					}
				}
			})
		}
		// The real 3600-byte commitment fits six full 1280-byte packets.
		// A compact proof keeps the default eight; lower owner caps stay exact.
		owners[0].chunks, owners[1].chunks, owners[2].chunks = []int{6, 2}, []int{3, 3, 2}, []int{3, 3, 2}
		owners[3].chunks, owners[4].chunks = []int{2, 2, 2, 2}, []int{8}
		for index := range owners {
			row := &owners[index]
			accepted, err := row.fixture.sender.sendGroupWithTimeoutDetailed(row.pack.Frames, row.fixture.receiver.ClientId(), func(err error) { row.result <- err }, time.Second)
			row.admitted = accepted
			if !accepted || err != nil {
				t.Fatalf("owner%d admission=%t/%v", index, accepted, err)
			}
		}
		for index := range owners {
			row := &owners[index]
			for chunk, want := range row.chunks {
				wire := row.fixture.data()
				if len(wire.pack.Frames) != want {
					t.Fatalf("H1_OWNER_CHUNK_GEOMETRY: owner%d chunk%d=%d want%d", index, chunk, len(wire.pack.Frames), want)
				}
				if len(row.result) != 0 {
					t.Fatalf("owner%d completed before chunk%d peer ACK", index, chunk)
				}
				// The actual write and its complete repair head must both fit,
				// including later encryption of this plaintext fixture's Pack.
				sequence := row.fixture.sequence()
				path := sendTransferPath(row.fixture.sender.ClientId(), DestinationId(row.fixture.receiver.ClientId()))
				actual := privateH1SealedByteCount(t, path, wire.bytes)
				repairWire := privateH1FullWire(t, sequence, wire.pack.Frames, true)
				repair := privateH1SealedByteCount(t, path, repairWire)
				MessagePoolReturn(repairWire)
				if actual > 16*1024 || repair > 16*1024 {
					t.Fatalf("owner%d chunk%d encrypted actual/repair=%d/%d exceeds advertised 16KiB", index, chunk, actual, repair)
				}
				t.Logf("h1_owner_wire owner=%d chunk=%d frames=%d encrypted=%d repair=%d", index, chunk, want, actual, repair)
				row.fixture.forward(wire, row.fixture.receiverIn)
				row.fixture.acknowledge()
			}
			select {
			case err := <-row.result:
				if err != nil {
					t.Fatal(err)
				}
			default:
				t.Fatal("owner terminal did not follow its final real peer ACK", index)
			}
			if len(row.result) != 0 || len(row.fixture.delivered) != 8 {
				t.Fatal("owner callback or payload count changed", index)
			}
			for frame := range row.pack.Frames {
				if !bytes.Equal(row.fixture.delivered[frame], row.fixture.witnesses[frame]) {
					t.Fatal("owner policy changed packet content/order", index, frame)
				}
			}
			assertRetainedBudgetBalance(t, row.fixture.budget)
		}
	})
}

// Direct UDP admission reads its own sender settings before any write.
func TestCanonicalH1ChunkSettingsSharedNoAckUsesOwner(t *testing.T) {
	assertMessagePoolOwnership(t)
	frames, witnesses := transferGroupTestFrames(t, 2, 64)
	defer func() {
		for _, frame := range frames {
			MessagePoolReturn(frame.MessageBytes)
		}
		releaseTransferGroupTestWitnesses(t, frames, witnesses)
	}()
	pack := &SendPack{Frames: frames, logicalGroup: true, ackTarget: &parsedPacketGroup{ipPath: &IpPath{Protocol: IpProtocolUdp}}}
	settings := DefaultSendBufferChunkSettings()
	if len(pack.noAckFastPathFrames(settings)) != 2 {
		t.Fatal("default shared UDP path changed")
	}
	settings.SharedMaxFrames = 1
	if pack.noAckFastPathFrames(settings) != nil {
		t.Fatal("direct UDP path ignored its owner's lower count")
	}
	settings.SharedMaxFrames, settings.SharedMaxMessageByteCount = 2, 64
	if pack.noAckFastPathFrames(settings) != nil {
		t.Fatal("direct UDP path ignored its owner's lower bytes")
	}
	if len(pack.noAckFastPathFrames(SendBufferChunkSettings{})) != 2 {
		t.Fatal("zero-valued owner changed the legacy direct UDP bound")
	}
}

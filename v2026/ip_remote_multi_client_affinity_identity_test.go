package connect

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"testing/synctest"
)

// The ordinary API is deliberately unavailable: this optional request must
// neither fall back to it nor remove an identity lacking fresh ownership.
type affinityIdentityTestGenerator struct {
	MultiClientGenerator
	call    func(context.Context, MultiHopId, Id) (*MultiClientGeneratorClientArgs, error)
	removed atomic.Int32
}

func (self *affinityIdentityTestGenerator) NewClientArgsForFreshDestinationContext(ctx context.Context, destination MultiHopId, excluded Id) (*MultiClientGeneratorClientArgs, error) {
	return self.call(ctx, destination, excluded)
}

func (self *affinityIdentityTestGenerator) RemoveClientArgs(*MultiClientGeneratorClientArgs) {
	self.removed.Add(1)
}

// This calls the actual API generator and its credential authority, with the
// public HTTP path forbidden by the existing fixture. Instance id churn alone
// is not success: the authenticated client id and its seed must be distinct.
func TestAffinityRenewalFreshIdentityBypassesParkedSource(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		f := newIdentityReuseFixture(t, t.Context(), nil, nil)
		t.Cleanup(func() {
			if err := f.generator.CloseAndWait(context.Background()); err != nil {
				t.Error(err)
			}
		})
		parked := f.failArgsAttempt(t)
		window := &multiClientWindow{generator: f.generator}
		fresh, err := window.freshAffinityClientArgs(t.Context(), f.destination, parked.ClientId)
		if err != nil {
			t.Fatal(err)
		}
		if fresh.ClientId == parked.ClientId || fresh.ClientAuth.InstanceId == parked.ClientAuth.InstanceId ||
			len(fresh.clientKeySeed) != 0 || f.mintCount.Load() != 2 || f.retireCount.Load() != 0 {
			t.Fatal("fresh renewal reused source credentials or retired another owner")
		}
		// Ordinary destination reuse is still enabled and unaffected.
		ordinary, err := f.generator.NewClientArgsForDestinationContext(t.Context(), f.destination)
		if err != nil || ordinary.ClientId != parked.ClientId || f.mintCount.Load() != 2 {
			t.Fatal("fresh per-request identity disabled ordinary destination reuse")
		}
		f.generator.RemoveClientArgs(fresh)
		f.generator.RemoveClientArgs(ordinary)
		synctest.Wait()
	})
}

func TestAffinityRenewalFreshIdentityKeepsLiveSibling(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		f := newIdentityReuseFixture(t, t.Context(), nil, nil)
		old, err := f.generator.NewClientArgsForDestinationContext(t.Context(), f.destination)
		if err != nil {
			t.Fatal(err)
		}
		client := NewClient(t.Context(), old.ClientId, NewNoContractClientOob(), closeWaitClientSettings())
		t.Cleanup(func() {
			if err := client.CloseAndWait(context.Background()); err != nil {
				t.Error(err)
			}
			f.generator.RemoveClientArgs(old)
			if err := f.generator.CloseAndWait(context.Background()); err != nil {
				t.Error(err)
			}
		})
		window := &multiClientWindow{generator: f.generator}
		fresh, err := window.freshAffinityClientArgs(t.Context(), f.destination, old.ClientId)
		if err != nil || fresh == nil {
			t.Fatalf("fresh identity: %v", err)
		}
		if fresh.ClientId == client.ClientId() || client.ctx.Err() != nil || f.retireCount.Load() != 0 {
			t.Fatal("fresh renewal revoked or reused an established sibling source")
		}
		f.generator.identityState.mutex.Lock()
		oldIdentity := f.generator.identityState.live[old.ClientId]
		newIdentity := f.generator.identityState.live[fresh.ClientId]
		f.generator.identityState.mutex.Unlock()
		if oldIdentity == nil || newIdentity == nil || oldIdentity.InstanceId != old.ClientAuth.InstanceId {
			t.Fatal("fresh renewal replaced the incumbent identity-state generation")
		}
		f.generator.RemoveClientArgs(fresh)
		synctest.Wait()
	})
}

func TestAffinityRenewalFreshIdentityBypassesRestoredSource(t *testing.T) {
	assertMessagePoolOwnership(t)
	synctest.Test(t, func(t *testing.T) {
		f := newIdentityReuseFixture(t, t.Context(), nil, nil)
		t.Cleanup(func() {
			if err := f.generator.CloseAndWait(context.Background()); err != nil {
				t.Error(err)
			}
		})
		restored := &WindowClientIdentity{ClientId: NewId(), InstanceId: NewId(),
			ByJwt: "synthetic-restored-token", Destination: f.destination}
		f.generator.SetIdentityStore(&fakeIdentityStore{persisted: []*WindowClientIdentity{restored}})
		synctest.Wait()
		window := &multiClientWindow{generator: f.generator}
		fresh, err := window.freshAffinityClientArgs(t.Context(), f.destination, restored.ClientId)
		if err != nil || fresh == nil || fresh.ClientId == restored.ClientId || f.mintCount.Load() != 1 {
			t.Fatal("explicit renewal resumed a restored source instead of minting a distinct source")
		}
		ordinary, err := f.generator.NewClientArgsForDestinationContext(t.Context(), f.destination)
		if err != nil || ordinary == nil || ordinary.ClientId != restored.ClientId || f.mintCount.Load() != 1 {
			t.Fatal("explicit renewal consumed or disabled ordinary restoration")
		}
		f.generator.RemoveClientArgs(fresh)
		f.generator.RemoveClientArgs(ordinary)
		synctest.Wait()
	})
}

func TestAffinityRenewalFreshIdentityRejectsUnsupportedGenerator(t *testing.T) {
	window := &multiClientWindow{generator: struct{ MultiClientGenerator }{}}
	args, err := window.freshAffinityClientArgs(t.Context(), RequireMultiHopId(NewId()), NewId())
	if args != nil || !errors.Is(err, errAffinityFreshIdentityUnsupported) {
		t.Fatal("unsupported generator did not refuse without invoking an ordinary mint")
	}
}

func TestAffinityRenewalFreshIdentityRejectsReusedOwnership(t *testing.T) {
	for _, variant := range []string{"excluded", "live-sibling", "seeded", "zero"} {
		excluded, sibling := NewId(), NewId()
		args := &MultiClientGeneratorClientArgs{ClientId: NewId()}
		switch variant {
		case "excluded":
			args.ClientId = excluded
		case "live-sibling":
			args.ClientId = sibling
		case "seeded":
			args.clientKeySeed = []byte{1}
		case "zero":
			args.ClientId = Id{}
		}
		generator := &affinityIdentityTestGenerator{call: func(context.Context, MultiHopId, Id) (*MultiClientGeneratorClientArgs, error) {
			return args, nil
		}}
		live := &multiClientChannel{ctx: t.Context()}
		window := &multiClientWindow{generator: generator, clients: map[Id]*multiClientChannel{sibling: live}}
		got, err := window.freshAffinityClientArgs(t.Context(), RequireMultiHopId(NewId()), excluded)
		if got != nil || !errors.Is(err, errAffinityIdentityReused) || generator.removed.Load() != 0 || live.IsDone() {
			t.Fatalf("%s result revoked or accepted an identity without new ownership", variant)
		}
	}
}

func TestAffinityRenewalFreshIdentityCancellationJoinsPartialOwner(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		entered := make(chan struct{})
		generator := &affinityIdentityTestGenerator{call: func(ctx context.Context, _ MultiHopId, _ Id) (*MultiClientGeneratorClientArgs, error) {
			close(entered)
			<-ctx.Done()
			return &MultiClientGeneratorClientArgs{ClientId: NewId()}, nil
		}}
		window := &multiClientWindow{generator: generator}
		finished := make(chan error, 1)
		go func() {
			args, err := window.freshAffinityClientArgs(ctx, RequireMultiHopId(NewId()), NewId())
			if args != nil {
				err = errors.New("canceled request returned a fresh owner")
			}
			finished <- err
		}()
		<-entered
		cancel()
		if err := <-finished; !errors.Is(err, context.Canceled) || generator.removed.Load() != 1 {
			t.Fatal("canceled explicit mint did not join and return its one distinct partial owner")
		}
	})
}

func TestAffinityRenewalFreshIdentityCannotOverwriteKnownGeneration(t *testing.T) {
	for _, restored := range []bool{false, true} {
		state := newWindowIdentityState(t.Context(), nil)
		old := &WindowClientIdentity{ClientId: NewId(), InstanceId: NewId(), Destination: RequireMultiHopId(NewId())}
		if restored {
			state.restored[old.Destination] = []*WindowClientIdentity{old}
		} else {
			state.Record(old)
		}
		replacement := &WindowClientIdentity{ClientId: old.ClientId, InstanceId: NewId(), Destination: old.Destination}
		if state.recordFreshAffinityIdentity(replacement) {
			t.Fatal("known source was published as a fresh authenticated identity")
		}
		if restored && state.restored[old.Destination][0] != old || !restored && state.live[old.ClientId] != old {
			t.Fatal("refused fresh publication changed the incumbent generation")
		}
		state.cancelLoad()
	}
}

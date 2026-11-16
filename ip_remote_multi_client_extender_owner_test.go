package connect

import (
	"context"
	"net/netip"
	"slices"
	"testing"
	"time"
)

// Ownership of the extender dot on a provider event (EXTENDER.md K1).
//
// A provider id outlives the channel that carries it: a replacement keeps the
// id and installs its own watcher. The old watcher is woken by the same change
// monitor, so without the ownership check it would publish the departed
// channel's addresses over the live one's -- and the dot would show an
// extender that is no longer carrying anything. The check is at the layer the
// bug is observable, before the publish rather than after it.

// A channel for clientId, as familyTestChannel builds one: the same-id
// replacement of a channel holds a client of its own under the departed
// channel's id.
func extenderOwnerTestChannel(t *testing.T, ctx context.Context, settings *MultiClientSettings, clientId Id) *multiClientChannel {
	t.Helper()
	clientSettings := DefaultClientSettings()
	clientSettings.Log = NewNoopLogger()
	client := NewClient(ctx, clientId, NewNoContractClientOob(), clientSettings)
	t.Cleanup(client.Close)
	return &multiClientChannel{
		ctx:      ctx,
		log:      NewNoopLogger(),
		client:   client,
		settings: settings,
		args: &multiClientChannelArgs{
			MultiClientGeneratorClientArgs: MultiClientGeneratorClientArgs{ClientId: clientId},
			Destination:                    RequireMultiHopId(NewId()),
			DestinationStats:               DestinationStats{IpFamily: IpFamilyDualstack},
		},
		eventBuckets:              []*multiClientEventBucket{},
		ip4DestinationSourceCount: map[Ip4Path]map[Ip4Path]int{},
		ip6DestinationSourceCount: map[Ip6Path]map[Ip6Path]int{},
		packetStats:               &clientWindowStats{log: NewNoopLogger()},
	}
}

// Installs client under its id as the window's install does: in the slot, with
// the next extender dot generation.
func installExtenderOwnerTestChannel(window *multiClientWindow, client *multiClientChannel) {
	window.stateLock.Lock()
	defer window.stateLock.Unlock()
	window.clients[client.ClientId()] = client
	window.installExtenderIpsGenerationWithLock(client)
}

// The extender addresses on the provider event of clientId, failing when the
// event is gone.
func extenderOwnerTestEventIps(t *testing.T, window *multiClientWindow, clientId Id) []netip.Addr {
	t.Helper()
	event := window.monitor.ProviderEvents()[clientId]
	if event == nil {
		t.Fatal("the provider event was removed")
	}
	return event.ExtenderIps
}

// A watcher whose channel is replaced between its ownership check and its
// publish does not put the departed channel's addresses on the live channel's
// dot, and ends on its next check. The window gives each channel it installs a
// generation, and the monitor refuses a publish under an older one than it
// has seen. Before, the check and the publish were separate sections with
// nothing between them, so a replacement installed and published in that gap
// was overwritten with the departed channel's addresses, which stayed on the
// dot until the replacement's addresses next changed; this test, which let the
// scheduler choose the order, failed about 1 run in 20 that way. The order is
// forced here: the departing watcher parks between its check and its publish
// while the replacement is installed and publishes.
func TestWindowExtenderWatcherStopsWhenItsChannelIsReplaced(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	settings := DefaultMultiClientSettings()
	generator := newExtenderIpsTestGenerator()
	window := familyTestWindow(ctx, generator, settings)

	departing := familyTestChannel(t, ctx, settings, IpFamilyDualstack)
	clientId := departing.ClientId()
	installExtenderOwnerTestChannel(window, departing)

	departingIp := netip.MustParseAddr("192.0.2.121")
	movedIp := netip.MustParseAddr("192.0.2.123")
	replacementIp := netip.MustParseAddr("2001:db8::122")
	generator.setClientIps(departing.client, []netip.Addr{departingIp})
	window.monitor.AddProviderEventWithExtenderIps(
		clientId,
		ProviderStateAdded,
		departing.Destination().Tail(),
		nil,
		departing.IpFamily(),
		nil,
	)

	// the departing watcher's second publish parks after its ownership check;
	// for the departing channel the hook runs on its watcher's goroutine alone
	departingPublishCount := 0
	parked := make(chan struct{})
	release := make(chan struct{})
	func() {
		window.stateLock.Lock()
		defer window.stateLock.Unlock()
		window.beforeExtenderIpsPublishForTest = func(client *multiClientChannel) {
			if client != departing {
				return
			}
			departingPublishCount += 1
			if departingPublishCount == 2 {
				close(parked)
				<-release
			}
		}
	}()

	watching := make(chan struct{})
	go func() {
		defer close(watching)
		window.watchExtenderIps(departing)
	}()

	// the watcher's first publish, its own addresses: the event was added
	// with none, so only the watcher puts them there. Its change channel was
	// armed before that read, so the move below wakes it
	if !waitForCondition(10*time.Second, func() bool {
		event := window.monitor.ProviderEvents()[clientId]
		return event != nil && slices.Equal(event.ExtenderIps, []netip.Addr{departingIp})
	}) {
		t.Fatal("the watcher never published its own addresses")
	}

	// the departed channel's transport moves while it still owns the id, and
	// its watcher parks with the move checked and not yet published
	generator.setClientIps(departing.client, []netip.Addr{movedIp})
	select {
	case <-parked:
	case <-time.After(10 * time.Second):
		t.Fatal("the departing watcher never reached its second publish")
	}
	if ips := extenderOwnerTestEventIps(t, window, clientId); !slices.Equal(ips, []netip.Addr{departingIp}) {
		t.Fatalf("extender ips = %v before the replacement, expected the departing channel's %v", ips, departingIp)
	}

	// a replacement takes the id, and its own watcher publishes its addresses
	replacement := extenderOwnerTestChannel(t, ctx, settings, clientId)
	installExtenderOwnerTestChannel(window, replacement)
	generator.setClientIps(replacement.client, []netip.Addr{replacementIp})
	go window.watchExtenderIps(replacement)
	if !waitForCondition(10*time.Second, func() bool {
		event := window.monitor.ProviderEvents()[clientId]
		return event != nil && slices.Equal(event.ExtenderIps, []netip.Addr{replacementIp})
	}) {
		t.Fatal("the replacement's watcher never published its addresses")
	}

	// the departing watcher publishes the move it checked before the
	// replacement was installed, and ends on its next ownership check
	close(release)
	select {
	case <-watching:
	case <-time.After(10 * time.Second):
		t.Fatal("the replaced watcher did not end")
	}
	if ips := extenderOwnerTestEventIps(t, window, clientId); !slices.Equal(ips, []netip.Addr{replacementIp}) {
		t.Fatalf("extender ips = %v, expected the replacement's %v", ips, replacementIp)
	}
}

// A publish under an older generation than one the monitor has seen for the
// id is refused, even when the newer one changed nothing; a publish under the
// same or a newer generation lands; and the generation goes with the id's
// event, so a later owner of a re-added id is not held to it.
func TestSetProviderExtenderIpsForGenerationRefusesAnOlderOwner(t *testing.T) {
	monitor := NewRemoteUserNatMultiClientMonitorWithDefaults()
	clientId := NewId()
	olderIp := netip.MustParseAddr("192.0.2.124")
	newerIp := netip.MustParseAddr("2001:db8::124")
	monitor.AddProviderEventWithExtenderIps(clientId, ProviderStateAdded, NewId(), nil, IpFamilyDualstack, []netip.Addr{newerIp})

	// the newer owner publishes the set the event already carries
	if monitor.SetProviderExtenderIpsForGeneration(clientId, 2, []netip.Addr{newerIp}) {
		t.Fatal("an unchanged set reported a change")
	}
	if monitor.SetProviderExtenderIpsForGeneration(clientId, 1, []netip.Addr{olderIp}) {
		t.Fatal("an older owner's publish was taken")
	}
	if ips := monitor.ProviderEvents()[clientId].ExtenderIps; !slices.Equal(ips, []netip.Addr{newerIp}) {
		t.Fatalf("extender ips = %v, expected the newer owner's %v", ips, newerIp)
	}
	if !monitor.SetProviderExtenderIpsForGeneration(clientId, 2, []netip.Addr{olderIp, newerIp}) {
		t.Fatal("the owner's own change was refused")
	}
	if !monitor.SetProviderExtenderIpsForGeneration(clientId, 3, []netip.Addr{newerIp}) {
		t.Fatal("a newer owner's change was refused")
	}

	// the id leaves the window and comes back with an owner of its own
	monitor.AddProviderEventWithExtenderIps(clientId, ProviderStateRemoved, NewId(), nil, IpFamilyDualstack, nil)
	monitor.AddProviderEventWithExtenderIps(clientId, ProviderStateAdded, NewId(), nil, IpFamilyDualstack, nil)
	if !monitor.SetProviderExtenderIpsForGeneration(clientId, 1, []netip.Addr{olderIp}) {
		t.Fatal("the re-added id's owner was held to its removed generation")
	}
}

// A watcher whose id the window no longer holds at all ends the same way, which
// is what a removed provider looks like.
func TestWindowExtenderWatcherStopsWhenItsChannelIsRemoved(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	settings := DefaultMultiClientSettings()
	generator := newExtenderIpsTestGenerator()
	window := familyTestWindow(ctx, generator, settings)
	client := familyTestChannel(t, ctx, settings, IpFamilyDualstack)
	clientId := client.ClientId()

	func() {
		window.stateLock.Lock()
		defer window.stateLock.Unlock()
		delete(window.clients, clientId)
	}()
	watching := make(chan struct{})
	go func() {
		defer close(watching)
		window.watchExtenderIps(client)
	}()
	select {
	case <-watching:
	case <-time.After(10 * time.Second):
		t.Fatal("a watcher for an unheld id did not end")
	}
	if event := window.monitor.ProviderEvents()[clientId]; event != nil {
		t.Fatalf("an unheld id published %+v", event)
	}
}

// A generator that does not carry extender addresses, or a channel with no
// client behind it, yields nothing rather than a watcher that spins.
func TestWindowExtenderIpsTolerateAGeneratorWithout(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	settings := DefaultMultiClientSettings()
	plain := familyTestWindow(ctx, &plainTestGenerator{}, settings)
	client := familyTestChannel(t, ctx, settings, IpFamilyDualstack)
	if ips := plain.clientExtenderIps(client); ips != nil {
		t.Errorf("a plain generator yielded %v", ips)
	}
	watching := make(chan struct{})
	go func() {
		defer close(watching)
		plain.watchExtenderIps(client)
	}()
	select {
	case <-watching:
	case <-time.After(10 * time.Second):
		t.Fatal("a watcher on a plain generator did not end")
	}

	window := familyTestWindow(ctx, newExtenderIpsTestGenerator(), settings)
	if ips := window.clientExtenderIps(nil); ips != nil {
		t.Errorf("a nil channel yielded %v", ips)
	}
	empty := &multiClientChannel{}
	if ips := window.clientExtenderIps(empty); ips != nil {
		t.Errorf("a channel with no client yielded %v", ips)
	}
	emptyWatching := make(chan struct{})
	go func() {
		defer close(emptyWatching)
		window.watchExtenderIps(empty)
	}()
	select {
	case <-emptyWatching:
	case <-time.After(10 * time.Second):
		t.Fatal("a watcher on a channel with no client did not end")
	}
}

// A generator with no extender addresses of its own, which is every generator
// that predates K1.
type plainTestGenerator struct {
	MultiClientGenerator
}

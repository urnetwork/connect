package connect

import (
	"fmt"
	"net/netip"
	"runtime"
	"testing"
	"weak"
)

func TestBlockActionCollectorFlushReleasesEpochOwners(t *testing.T) {
	collector := newBlockActionCollector(1024, NewNoopLogger())
	var emitted []weak.Pointer[BlockAction]
	packetCount := 0
	unsub := collector.addCallback(func(actions []*BlockAction) {
		for _, action := range actions {
			emitted = append(emitted, weak.Make(action))
			packetCount += action.PacketCount
		}
	})
	defer unsub()
	aggregates := func() []weak.Pointer[blockActionAgg] {
		for i := range 1024 {
			ip := netip.AddrFrom4([4]byte{198, 18, byte(i >> 8), byte(i)})
			collector.add(&blockActionDecision{
				clusterKey: ip, clusterIps: []netip.Addr{ip},
				clusterHosts: []string{fmt.Sprintf("epoch-%04d.example.test", i)},
			}, false, false, nil, 1200)
		}
		owners := make([]weak.Pointer[blockActionAgg], 0, len(collector.agg))
		for _, aggregate := range collector.agg {
			owners = append(owners, weak.Make(aggregate))
		}
		return owners
	}()
	collector.flush()
	if len(collector.agg) != 0 || packetCount != 1024 || len(emitted) != 1024 {
		t.Fatalf("epoch semantics changed: remaining=%d packets=%d emitted=%d", len(collector.agg), packetCount, len(emitted))
	}
	runtime.GC()
	runtime.GC()
	for _, owner := range aggregates {
		if owner.Value() != nil {
			t.Fatal("flushed epoch aggregate remains rooted by collector")
		}
	}
	for _, action := range emitted {
		if action.Value() != nil {
			t.Fatal("unretained emitted action remains rooted by collector")
		}
	}
	runtime.KeepAlive(collector)
}

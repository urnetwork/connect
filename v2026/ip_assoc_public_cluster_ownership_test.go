// Public cluster results are caller-owned; core cache/affinity code must use
// its separate private immutable borrow. These controls start no workers.
package connect

import (
	"net/netip"
	"slices"
	"testing"
)

// Mutating a public address result must never write the published snapshot.
func TestIpAssocPublicClusterAddrsReturnIsOwned(t *testing.T) {
	multi, members := newBlockActionMemoryFixture(t, 1, 16)
	assoc := multi.ipAssoc
	published := assoc.clusters.Load()
	before := slices.Clone(published.members[members[0]])
	returned := assoc.GetClusterAddrs(members[0])
	if !slices.Equal(returned, before) {
		t.Fatal("public getter did not return the published members")
	}
	returned[0] = netip.MustParseAddr("203.0.113.250")
	if !slices.Equal(published.members[members[0]], before) || !slices.Equal(assoc.GetClusterAddrs(members[0]), before) {
		t.Fatal("PUBLIC_CLUSTER_GETTER_MUTATED_PUBLISHED_SNAPSHOT")
	}
}

// The existing path getter already owns its paths and address-byte results;
// preserve that boundary when introducing the private coherent borrow.
func TestIpAssocPublicClusterPathReturnIsOwned(t *testing.T) {
	multi, members := newBlockActionMemoryFixture(t, 1, 16)
	assoc := multi.ipAssoc
	before := slices.Clone(assoc.clusters.Load().members[members[0]])
	paths := assoc.GetCluster(blockActionMemoryPath(members[0]))
	if len(paths) != len(before) || len(paths[0].DestinationIp) != 4 {
		t.Fatal("public path getter did not return complete independent paths")
	}
	paths[0].DestinationIp[0] ^= 0xff
	paths[0].DestinationPort = 80
	if !slices.Equal(assoc.clusters.Load().members[members[0]], before) {
		t.Fatal("public path mutation changed the immutable cluster")
	}
}

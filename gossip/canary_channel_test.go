package gossip

// A signed discovery-channel restriction survives caches and every open relay.

import (
	"context"
	"testing"
	"time"

	pubsub "github.com/libp2p/go-libp2p-pubsub"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/connect/protocol"
)

// Even a channel-marked record using the old open tier must be withheld. The
// production signer also uses gated tier for older readers that lack the marker.
func TestGossipCanaryChannelBlocksRelayAndPublication(t *testing.T) {
	rootKey := newTestKey(t)
	extenderKey := newTestKey(t)
	directory := newTestDirectory(t, rootKey)
	node := &Node{settings: &NodeSettings{NetworkHost: testNetworkHost, Directory: directory}}
	ordinary := signTestRecord(t, rootKey, extenderKey, "192.0.2.1", 443, time.Now())
	body, err := directory.RootKeys().VerifyRecord(ordinary)
	if err != nil {
		t.Fatal(err)
	}
	body.CanaryChannel = connect.ExtenderChannelDns
	canary, err := connect.SignExtenderRecord(rootKey.privateKey, body)
	if err != nil {
		t.Fatal(err)
	}
	message := &protocol.ExtenderGossipMessage{Message: &protocol.ExtenderGossipMessage_Record{Record: canary}}
	if result := testValidate(t, node, message); result != pubsub.ValidationIgnore {
		t.Fatalf("canary relay verdict = %v", result)
	}
	if err := node.Publish(context.Background(), message); err == nil {
		t.Fatal("canary reached topic publication")
	}
	if _, err := directory.ApplySource(message, connect.ExtenderSourceDns); err != nil {
		t.Fatal(err)
	}
	if got := directory.SampleRecords(8, extenderKey.publicKey, []byte("synthetic-vantage")); len(got) != 0 {
		t.Fatal("canary was relayed even as the feed's own record")
	}
}

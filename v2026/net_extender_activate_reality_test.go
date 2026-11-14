package connect

// Root-cause test for the camouflage activation wire change (EXTENDER.md P6):
// the reality public key rides the activation args as reality_public_key_hex
// and is omitted when unset, so an operator that predates the field ignores it
// and an extender with camouflage off sends no key.

import (
	"encoding/hex"
	"encoding/json"
	"slices"
	"strings"
	"sync"
	"testing"
)

func TestExtenderActivateArgsCarryRealityPublicKeyHex(t *testing.T) {
	withKey := &ExtenderActivateArgs{
		PublicKeyHex:        "aa",
		RealityPublicKeyHex: "bbbb",
		Carriers:            []string{"tcp"},
	}
	withKeyJson, err := json.Marshal(withKey)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(withKeyJson), `"reality_public_key_hex":"bbbb"`) {
		t.Fatalf("args json did not carry the reality key: %s", withKeyJson)
	}
	// it round-trips back into the same field, as the server decoder reads it
	var decoded ExtenderActivateArgs
	if err := json.Unmarshal(withKeyJson, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.RealityPublicKeyHex != "bbbb" {
		t.Fatalf("decoded reality key = %q", decoded.RealityPublicKeyHex)
	}

	// an extender with camouflage off omits the field entirely, so the wire
	// value is unchanged for old operators
	withoutKey := &ExtenderActivateArgs{PublicKeyHex: "aa", Carriers: []string{"tcp"}}
	withoutKeyJson, err := json.Marshal(withoutKey)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(withoutKeyJson), "reality_public_key_hex") {
		t.Fatalf("an empty reality key was not omitted: %s", withoutKeyJson)
	}
}

// A role's key follows what it verified, so each post reads it again: no key
// while none is published leaves the field off the wire, and a key published
// since rides the next post, where it replaces a static RealityPublicKey.
func TestExtenderActivatorPostsThePublishedRealityKeyAtEachPost(t *testing.T) {
	realityPublicKey := newTestCamouflageKey(t)
	staticRealityPublicKey := newTestCamouflageKey(t)
	var stateLock sync.Mutex
	var published []byte
	fixture := newTestActivatorFixture(t, func(settings *ExtenderActivatorSettings) {
		settings.RealityPublicKey = staticRealityPublicKey
		settings.PublishedRealityPublicKey = func() []byte {
			stateLock.Lock()
			defer stateLock.Unlock()
			return published
		}
	})
	for _, ipVersion := range []int{4, 6} {
		post := fixture.waitPost()
		if post.ipVersion != ipVersion || post.args.RealityPublicKeyHex != "" {
			t.Fatalf("post of v%d carried key %q, expected none", post.ipVersion, post.args.RealityPublicKeyHex)
		}
	}
	func() {
		stateLock.Lock()
		defer stateLock.Unlock()
		published = realityPublicKey
	}()
	fixture.activator.Reactivate()
	for range []int{4, 6} {
		post := fixture.waitPost()
		if post.args.RealityPublicKeyHex != hex.EncodeToString(realityPublicKey) {
			t.Fatalf("post of v%d carried key %q, expected the published one", post.ipVersion, post.args.RealityPublicKeyHex)
		}
	}
}

// With no seam the activator posts the static key, as it always has, and an
// operator that predates the field decodes the post as if it were not there.
func TestExtenderActivatorPostsTheStaticRealityKeyToAnOperatorThatIgnoresIt(t *testing.T) {
	realityPublicKey := newTestCamouflageKey(t)
	fixture := newTestActivatorFixture(t, func(settings *ExtenderActivatorSettings) {
		settings.RealityPublicKey = realityPublicKey
	})
	post := fixture.waitPost()
	if post.args.RealityPublicKeyHex != hex.EncodeToString(realityPublicKey) {
		t.Fatalf("post carried key %q", post.args.RealityPublicKeyHex)
	}

	// the operator's args as they were before phase 14b: no such field, and
	// its decoder takes no exception to one it does not know
	type operatorArgsThatPredateTheKey struct {
		PublicKeyHex string   `json:"public_key_hex"`
		Carriers     []string `json:"carriers"`
	}
	argsJson, err := json.Marshal(post.args)
	if err != nil {
		t.Fatal(err)
	}
	decoded := &operatorArgsThatPredateTheKey{}
	if err := json.Unmarshal(argsJson, decoded); err != nil {
		t.Fatalf("an operator that predates the key could not read the post: %v", err)
	}
	if decoded.PublicKeyHex != post.args.PublicKeyHex || !slices.Equal(decoded.Carriers, post.args.Carriers) {
		t.Fatalf("the rest of the post changed: %+v", decoded)
	}
}

package connect

// Root-cause test for the camouflage activation wire change (EXTENDER.md P6):
// the reality public key rides the activation args as reality_public_key_hex
// and is omitted when unset, so an operator that predates the field ignores it
// and an extender with camouflage off sends no key.

import (
	"encoding/json"
	"strings"
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

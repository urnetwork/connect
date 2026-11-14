package connect

import (
	gojwt "github.com/golang-jwt/jwt/v5"
	"net/http"
	"testing"
)

func TestClientInfoUnknownCaseCannotOverrideCanonicalFields(t *testing.T) {
	value := ParseClientInfo(`{"v":1,"V":99,"device_type":"ios","DeviceType":"injected","app_version":"1","APP_VERSION":"bad"}`, "")
	if value.Version != 1 || value.DeviceType != "ios" || value.AppVersion != "1" {
		t.Fatal(value)
	}
}

// Every distribution a build stamps survives the header with the device type
// it pairs with, so the server can tell the App Store Mac from the
// direct-download Mac: the two send the same device type and version.
func TestClientInfoDistributionRoundTrips(t *testing.T) {
	cases := []struct {
		deviceType   string
		distribution string
	}{
		{deviceType: "ios", distribution: ClientDistributionAppStore},
		{deviceType: "macos", distribution: ClientDistributionAppStore},
		{deviceType: "macos", distribution: ClientDistributionMacDirect},
		{deviceType: "android", distribution: ClientDistributionPlay},
		{deviceType: "android", distribution: ClientDistributionAndroidDirect},
		{deviceType: "android", distribution: ClientDistributionSolanaDapp},
		{deviceType: "android", distribution: ClientDistributionEthosDapp},
		{deviceType: "windows", distribution: ClientDistributionWindows},
		{deviceType: "linux", distribution: ClientDistributionLinux},
		{deviceType: "linux", distribution: ClientDistributionLinuxAppimage},
		{deviceType: "linux", distribution: ClientDistributionLinuxFlatpak},
		{deviceType: "web", distribution: ClientDistributionWeb},
		{deviceType: "web", distribution: ClientDistributionExtensionChrome},
		{deviceType: "web", distribution: ClientDistributionExtensionFirefox},
	}
	for _, c := range cases {
		sent := ClientInfo{Version: 1, DeviceType: c.deviceType, AppVersion: "2026.11.11-1", SdkVersion: "2026.11.11-1", Distribution: c.distribution}
		header := http.Header{}
		header.Set(ClientInfoHeader, sent.Json())
		if received := ClientInfoFromHeader(header); received != sent {
			t.Errorf("%s/%s: sent %+v, received %+v", c.deviceType, c.distribution, sent, received)
		}
	}
}

// A distribution the parser does not know, or one that does not pair with the
// device type, is unknown; the device type and versions still stand. This is
// what lets a newer build add a distribution without an older server losing
// the rest of its client info.
func TestClientInfoUnknownOrMismatchedDistributionIsUnknown(t *testing.T) {
	cases := []struct {
		encoded    string
		deviceType string
	}{
		{encoded: `{"v":1,"device_type":"android","app_version":"1","distribution":"some_future_store"}`, deviceType: "android"},
		{encoded: `{"v":1,"device_type":"ios","app_version":"1","distribution":"mac_direct"}`, deviceType: "ios"},
		{encoded: `{"v":1,"device_type":"android","app_version":"1","distribution":"app_store"}`, deviceType: "android"},
		{encoded: `{"v":1,"device_type":"macos","app_version":"1","distribution":"play"}`, deviceType: "macos"},
		{encoded: `{"v":1,"device_type":"web","app_version":"1","distribution":"windows"}`, deviceType: "web"},
		{encoded: `{"v":1,"device_type":"unknown","app_version":"1","distribution":"app_store"}`, deviceType: "unknown"},
		{encoded: `{"v":1,"device_type":"cli","app_version":"1","distribution":"linux"}`, deviceType: "cli"},
		{encoded: `{"v":1,"device_type":"macos","app_version":"1","distribution":"MAC_DIRECT"}`, deviceType: "macos"},
		{encoded: `{"v":1,"device_type":"macos","app_version":"1","distribution":null}`, deviceType: "macos"},
		{encoded: `{"v":1,"device_type":"macos","app_version":"1","distribution":""}`, deviceType: "macos"},
	}
	for _, c := range cases {
		value := ParseClientInfo(c.encoded, "")
		if value.Distribution != "" || value.DeviceType != c.deviceType || value.AppVersion != "1" || value.Version != 1 {
			t.Errorf("%s: %+v", c.encoded, value)
		}
	}
}

// A client that names no distribution parses as unknown and serializes as
// every client did before the field, so the header and connect auth of a
// build that does not know its distribution do not change.
func TestClientInfoWithoutDistributionIsUnchanged(t *testing.T) {
	value := ParseClientInfo(`{"v":1,"device_type":"macos","app_version":"2026.11.11-1","sdk_version":"2026.11.11-1"}`, "")
	if value != (ClientInfo{Version: 1, DeviceType: "macos", AppVersion: "2026.11.11-1", SdkVersion: "2026.11.11-1"}) {
		t.Fatal(value)
	}
	if encoded := value.Json(); encoded != `{"v":1,"device_type":"macos","app_version":"2026.11.11-1","sdk_version":"2026.11.11-1"}` {
		t.Fatal(encoded)
	}
	if encoded := UnknownClientInfo().Json(); encoded != `{"v":1,"device_type":"unknown","app_version":""}` {
		t.Fatal(encoded)
	}
}

// The distribution follows the object's other rules: a duplicate, or a value
// that is not a string, invalidates the whole object, as a duplicate or
// mistyped device type does.
func TestClientInfoMalformedDistributionInvalidatesObject(t *testing.T) {
	for _, encoded := range []string{
		`{"v":1,"device_type":"macos","app_version":"1","distribution":"mac_direct","distribution":"app_store"}`,
		`{"v":1,"device_type":"macos","app_version":"1","distribution":7}`,
		`{"v":1,"device_type":"macos","app_version":"1","distribution":["mac_direct"]}`,
	} {
		if value := ParseClientInfo(encoded, "legacy"); value != (ClientInfo{Version: 1, DeviceType: "unknown", AppVersion: "legacy"}) {
			t.Errorf("%s: %+v", encoded, value)
		}
	}
}
func TestUnverifiedJwtTypedClaimsAndNetworkId(t *testing.T) {
	network, user, client, session, root := NewId(), NewId(), NewId(), NewId(), NewId()
	claims := gojwt.MapClaims{"network_id": network.String(), "network_name": "name", "user_id": user.String(), "client_id": client.String(), "session_id": session.String(), "root_client_id": root.String()}
	encoded, _ := gojwt.NewWithClaims(gojwt.SigningMethodHS256, claims).SignedString([]byte("test"))
	parsed, err := ParseByJwtUnverified(encoded)
	if err != nil || parsed.NetworkId != network || parsed.UserId != user || parsed.ClientId != client || *parsed.SessionId != session || *parsed.RootClientId != root {
		t.Fatal(parsed, err)
	}
	for _, name := range []string{"user_id", "network_id", "network_name", "client_id", "session_id", "root_client_id"} {
		claims[name] = map[string]any{"invalid": true}
	}
	encoded, _ = gojwt.NewWithClaims(gojwt.SigningMethodHS256, claims).SignedString([]byte("test"))
	parsed, err = ParseByJwtUnverified(encoded)
	if err != nil || parsed.NetworkId != (Id{}) || parsed.SessionId != nil {
		t.Fatal(parsed, err)
	}
}

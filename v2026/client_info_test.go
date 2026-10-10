package connect

import (
	gojwt "github.com/golang-jwt/jwt/v5"
	"testing"
)

func TestClientInfoUnknownCaseCannotOverrideCanonicalFields(t *testing.T) {
	value := ParseClientInfo(`{"v":1,"V":99,"device_type":"ios","DeviceType":"injected","app_version":"1","APP_VERSION":"bad"}`, "")
	if value.Version != 1 || value.DeviceType != "ios" || value.AppVersion != "1" {
		t.Fatal(value)
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

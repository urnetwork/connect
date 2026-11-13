// The actual SDK HTTP owner carries the exact wallet-approved message without
// changing the authenticated client or invoking a signer on the user's behalf.
package connect

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
)

// Challenge acquisition and the later explicit signature submission remain two
// separate authenticated calls, preserving all display and signature bytes.
func TestWalletMappingApiKeepsExactAuthenticatedApproval(t *testing.T) {
	clientId := Id{1}
	message := "Approve URnetwork provider wallet mapping\nsynthetic exact original bytes"
	posts := 0
	api, _ := authObservationTestApi(t.Context(), nil, true, serialTestRoundTripper(func(request *http.Request) (*http.Response, error) {
		posts++
		if request.Method != http.MethodPost || request.Header.Get("Authorization") != "Bearer synthetic-wallet-api-token" {
			return nil, fmt.Errorf("mapping request lost authenticated post")
		}
		raw, err := io.ReadAll(request.Body)
		if err != nil {
			return nil, err
		}
		if posts == 1 {
			var args SnWalletMappingChallengeArgs
			if request.URL.Path != "/sn/wallet/consent" || json.Unmarshal(raw, &args) != nil || args.ClientId == nil || *args.ClientId != clientId || args.FromEpoch != 0 || args.ThroughEpoch != 100 {
				return nil, fmt.Errorf("mapping challenge changed original selection")
			}
			body, _ := json.Marshal(SnWalletMappingChallengeResult{Message: message})
			return authObservationTestResponse(request, http.StatusOK, string(body)), nil
		}
		var args SnSetWalletArgs
		if posts != 2 || request.URL.Path != "/sn/wallet" || json.Unmarshal(raw, &args) != nil || args.ClientId == nil || *args.ClientId != clientId || args.Message != message || args.Signature != "0x"+strings.Repeat("12", 64) {
			return nil, fmt.Errorf("mapping submission changed signed original")
		}
		return authObservationTestResponse(request, http.StatusOK, `{"mapping_hash":"synthetic-original-hash","mapping_generation":1}`), nil
	}))
	defer api.Close()
	api.SetByJwt("synthetic-wallet-api-token")
	challenge, err := api.SnWalletMappingChallengeSync(&SnWalletMappingChallengeArgs{ClientId: &clientId, ColdkeySs58: "synthetic-wallet", FromEpoch: 0, ThroughEpoch: 100})
	if err != nil || challenge == nil || challenge.Message != message || posts != 1 {
		t.Fatal("challenge was changed or submitted without signature", challenge, posts, err)
	}
	result, err := api.SnSetWalletSync(&SnSetWalletArgs{ClientId: &clientId, ColdkeySs58: "synthetic-wallet", Message: challenge.Message, Signature: "0x" + strings.Repeat("12", 64)})
	if err != nil || result == nil || result.MappingHash != "synthetic-original-hash" || result.MappingGeneration != 1 || posts != 2 {
		t.Fatal("actual mapping acknowledgement was lost", result, posts, err)
	}
}

// Legacy callers retain their prior wire grammar and receive no invented
// mapping generation merely because the new optional fields exist.
func TestWalletMappingApiLegacyRequestRetainsMissingConsent(t *testing.T) {
	posts := 0
	api, _ := authObservationTestApi(t.Context(), nil, true, serialTestRoundTripper(func(request *http.Request) (*http.Response, error) {
		posts++
		raw, err := io.ReadAll(request.Body)
		if err != nil || strings.Contains(string(raw), "message") || strings.Contains(string(raw), "signature") {
			return nil, fmt.Errorf("legacy request acquired invented mapping fields: %w", err)
		}
		return authObservationTestResponse(request, http.StatusOK, `{}`), nil
	}))
	defer api.Close()
	value, err := api.SnSetWalletSync(&SnSetWalletArgs{ColdkeySs58: "synthetic-legacy"})
	if err != nil || value == nil || value.MappingHash != "" || value.MappingGeneration != 0 || posts != 1 {
		t.Fatal("legacy request became authenticated mapping evidence", value, err)
	}
}

// The network consent is requested from its own route with no client, and
// submitted through the same wallet route without a client.
func TestNetworkWalletMappingApiRequestsWithoutClient(t *testing.T) {
	message := "Approve URnetwork network wallet mapping\nsynthetic exact original bytes"
	posts := 0
	api, _ := authObservationTestApi(t.Context(), nil, true, serialTestRoundTripper(func(request *http.Request) (*http.Response, error) {
		posts++
		if request.Method != http.MethodPost || request.Header.Get("Authorization") != "Bearer synthetic-network-wallet-token" {
			return nil, fmt.Errorf("network mapping request lost authenticated post")
		}
		raw, err := io.ReadAll(request.Body)
		if err != nil {
			return nil, err
		}
		if posts == 1 {
			var args map[string]any
			if request.URL.Path != "/sn/wallet/network-consent" || json.Unmarshal(raw, &args) != nil || args["client_id"] != nil || args["coldkey_ss58"] != "synthetic-wallet" || args["from_epoch"] != float64(7) || args["through_epoch"] != float64(107) {
				return nil, fmt.Errorf("network mapping challenge changed its selection: %s", raw)
			}
			body, _ := json.Marshal(SnWalletMappingChallengeResult{Message: message})
			return authObservationTestResponse(request, http.StatusOK, string(body)), nil
		}
		var args SnSetWalletArgs
		if posts != 2 || request.URL.Path != "/sn/wallet" || json.Unmarshal(raw, &args) != nil || args.ClientId != nil || args.Message != message {
			return nil, fmt.Errorf("network mapping submission changed signed original")
		}
		return authObservationTestResponse(request, http.StatusOK, `{"mapping_hash":"synthetic-network-hash","mapping_generation":1}`), nil
	}))
	defer api.Close()
	api.SetByJwt("synthetic-network-wallet-token")
	challenge, err := api.SnNetworkWalletMappingChallengeSync(&SnNetworkWalletMappingChallengeArgs{ColdkeySs58: "synthetic-wallet", FromEpoch: 7, ThroughEpoch: 107})
	if err != nil || challenge == nil || challenge.Message != message || posts != 1 {
		t.Fatal("network challenge was changed", challenge, posts, err)
	}
	result, err := api.SnSetWalletSync(&SnSetWalletArgs{ColdkeySs58: "synthetic-wallet", Message: challenge.Message, Signature: "0x" + strings.Repeat("12", 64)})
	if err != nil || result == nil || result.MappingHash != "synthetic-network-hash" || result.MappingGeneration != 1 || posts != 2 {
		t.Fatal("network mapping acknowledgement was lost", result, posts, err)
	}
}

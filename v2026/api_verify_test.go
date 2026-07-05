package connect

// JSON-shape tests for the `/verify/keys` and `/sn` control-plane API
// bindings: exact wire-name pins for the small types and marshal/unmarshal
// round-trips with fixed values for all of them.

import (
	"encoding/json"
	"testing"

	"github.com/go-playground/assert/v2"
)

// TestVerifyKeysResultJson pins the `GET /verify/keys` result shape:
// `server_key_id` as a number, `public_key` as base64.
func TestVerifyKeysResultJson(t *testing.T) {
	keysResult := &VerifyKeysResult{
		Keys: []*VerifyServerKey{
			{
				ServerKeyId: 7,
				PublicKey:   testVerifyHexBytes(t, testVerifyVpkHex),
			},
		},
	}
	keysResultJson, err := json.Marshal(keysResult)
	assert.Equal(t, nil, err)
	expectedJson := `{"keys":[{"server_key_id":7,"public_key":"AQIDBAUGBwgJCgsMDQ4PEBESExQVFhcYGRobHB0eHyA="}]}`
	assert.Equal(t, expectedJson, string(keysResultJson))

	var parsedKeysResult VerifyKeysResult
	err = json.Unmarshal(keysResultJson, &parsedKeysResult)
	assert.Equal(t, nil, err)
	assert.Equal(t, 1, len(parsedKeysResult.Keys))
	assert.Equal(t, *keysResult.Keys[0], *parsedKeysResult.Keys[0])
}

// TestSnSetWalletJson pins the `POST /sn/wallet` args/result shapes,
// including error omission when unset.
func TestSnSetWalletJson(t *testing.T) {
	setWalletArgs := &SnSetWalletArgs{
		ColdkeySs58: "5GrwvaEF5zXb26Fz9rcQpDWS57CtERHpNehXCPcNoHGKutQY",
	}
	setWalletArgsJson, err := json.Marshal(setWalletArgs)
	assert.Equal(t, nil, err)
	assert.Equal(
		t,
		`{"coldkey_ss58":"5GrwvaEF5zXb26Fz9rcQpDWS57CtERHpNehXCPcNoHGKutQY"}`,
		string(setWalletArgsJson),
	)
	var parsedSetWalletArgs SnSetWalletArgs
	err = json.Unmarshal(setWalletArgsJson, &parsedSetWalletArgs)
	assert.Equal(t, nil, err)
	assert.Equal(t, *setWalletArgs, parsedSetWalletArgs)

	// empty result omits the error
	emptyResultJson, err := json.Marshal(&SnSetWalletResult{})
	assert.Equal(t, nil, err)
	assert.Equal(t, `{}`, string(emptyResultJson))

	errorResultJson, err := json.Marshal(&SnSetWalletResult{
		Error: &SnSetWalletError{
			Message: "invalid ss58",
		},
	})
	assert.Equal(t, nil, err)
	assert.Equal(t, `{"error":{"message":"invalid ss58"}}`, string(errorResultJson))

	var parsedErrorResult SnSetWalletResult
	err = json.Unmarshal(errorResultJson, &parsedErrorResult)
	assert.Equal(t, nil, err)
	assert.Equal(t, "invalid ss58", parsedErrorResult.Error.Message)
}

// TestSnPoolClaimJson round-trips the `GET /sn/pool/claim` args and result
// with fixed values and pins the args wire name.
func TestSnPoolClaimJson(t *testing.T) {
	poolClaimArgs := &SnPoolClaimArgs{
		Epoch: 42,
	}
	poolClaimArgsJson, err := json.Marshal(poolClaimArgs)
	assert.Equal(t, nil, err)
	assert.Equal(t, `{"epoch":42}`, string(poolClaimArgsJson))

	poolClaimResult := &SnPoolClaimResult{
		Epoch:    42,
		NoId:     testVerifyHexBytes(t, "000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f"),
		Coldkey:  testVerifyHexBytes(t, "202122232425262728292a2b2c2d2e2f303132333435363738393a3b3c3d3e3f"),
		ShareBps: 1250,
		Proof: [][]byte{
			testVerifyHexBytes(t, testVerifyClientNonceHex),
			testVerifyHexBytes(t, testVerifyServerNonceHex),
		},
		PayoutRoot:      testVerifyHexBytes(t, testVerifyVpkHex),
		ContractAddress: "0x00000000000000000000000000000000000009c4",
		ChainId:         964,
		ClaimOpenBlock:  123456,
	}
	poolClaimResultJson, err := json.Marshal(poolClaimResult)
	assert.Equal(t, nil, err)
	var parsedPoolClaimResult SnPoolClaimResult
	err = json.Unmarshal(poolClaimResultJson, &parsedPoolClaimResult)
	assert.Equal(t, nil, err)
	assert.Equal(t, *poolClaimResult, parsedPoolClaimResult)
}

// TestSnEpochResultJson pins the `GET /sn/epoch` result wire names and
// round-trips fixed values.
func TestSnEpochResultJson(t *testing.T) {
	epochResult := &SnEpochResult{
		Epoch:               42,
		StartBlock:          1000000,
		CommitDeadlineBlock: 1001200,
		TrailsDeadlineBlock: 1007200,
		FinalizeBlock:       1014400,
		TEpochBlocks:        14400,
		ChainId:             964,
		ContractAddress:     "0x00000000000000000000000000000000000009c4",
	}
	epochResultJson, err := json.Marshal(epochResult)
	assert.Equal(t, nil, err)
	expectedJson := `{"epoch":42,` +
		`"start_block":1000000,` +
		`"commit_deadline_block":1001200,` +
		`"trails_deadline_block":1007200,` +
		`"finalize_block":1014400,` +
		`"t_epoch_blocks":14400,` +
		`"chain_id":964,` +
		`"contract_address":"0x00000000000000000000000000000000000009c4"}`
	assert.Equal(t, expectedJson, string(epochResultJson))

	var parsedEpochResult SnEpochResult
	err = json.Unmarshal(epochResultJson, &parsedEpochResult)
	assert.Equal(t, nil, err)
	assert.Equal(t, *epochResult, parsedEpochResult)
}

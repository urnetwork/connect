package main

// sn.go — subnet (bittensor) subcommands for the provider
// (sn/PLAN.md 7.3): `provider wallet set` registers the claim coldkey
// with the platform (decision D-2), and `provider claim` fetches and
// verifies this network's pool payout claim for an epoch (decision
// D-6). Claim is verify-only by design: it recomputes the merkle leaf,
// checks the inclusion proof, cross-checks the payout root on-chain via
// a minimal eth_call when --rpc is given, and prints the ready-to-submit
// claimMiner calldata. Signing and submission live in the separate
// snclaim tool, so the provider binary never touches chain key material.

import (
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"

	"github.com/docopt/docopt-go"

	"github.com/urnetwork/connect"
)

// readNetworkJwt loads the network jwt written by `provider auth` from
// ~/.urnetwork/jwt — the same credential provideAuth uses.
func readNetworkJwt() (string, error) {
	jwtPath, err := providerStatePath("jwt")
	if err != nil {
		return "", err
	}
	byJwtBytes, err := os.ReadFile(jwtPath)
	if errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("jwt does not exist at %s. Run `provider auth` first", jwtPath)
	}
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(byJwtBytes)), nil
}

// snSetWallet validates the ss58 coldkey locally and idempotently sets
// it as the network's subnet claim wallet via the authenticated
// `POST /sn/wallet` route. Prints the result on success.
func snSetWallet(ctx context.Context, clientStrategy *connect.ClientStrategy, apiUrl string, coldkeySs58 string) error {
	pubkey, err := parseSs58Coldkey(coldkeySs58)
	if err != nil {
		return fmt.Errorf("invalid ss58 coldkey %q: %s", coldkeySs58, err)
	}
	byJwt, err := readNetworkJwt()
	if err != nil {
		return err
	}
	api := connect.NewBringYourApi(ctx, clientStrategy, apiUrl)
	api.SetByJwt(byJwt)
	result, err := api.SnSetWalletSync(&connect.SnSetWalletArgs{
		ColdkeySs58: coldkeySs58,
	})
	if err != nil {
		return err
	}
	if result.Error != nil {
		return fmt.Errorf("%s", result.Error.Message)
	}
	fmt.Printf("subnet wallet set to %s (pubkey 0x%x)\n", coldkeySs58, pubkey)
	return nil
}

// walletSet implements `provider wallet set <coldkey_ss58>`.
func walletSet(opts docopt.Opts) {
	apiUrl, err := opts.String("--api_url")
	if err != nil {
		apiUrl = DefaultApiUrl
	}

	event := connect.NewEventWithContext(context.Background())
	event.SetOnSignals(syscall.SIGINT, syscall.SIGQUIT, syscall.SIGTERM)

	ctx, cancel := context.WithCancel(event.Ctx())
	defer cancel()

	clientStrategy := connect.NewClientStrategyWithDefaults(ctx)

	coldkeySs58, _ := opts.String("<coldkey_ss58>")
	if err := snSetWallet(ctx, clientStrategy, apiUrl, coldkeySs58); err != nil {
		fmt.Printf("subnet wallet not set: %s\n", err)
		os.Exit(1)
	}
}

// claim implements `provider claim [--epoch=<epoch>] [--rpc=<rpc_url>]...
// [--dry-run]`.
//
// Default epoch: the platform reports the current epoch e; the payout
// root for e is only committed and finalized after e ends
// (sn/WHITEPAPER.md 5.2), so the default target is e-1 — the most
// recent epoch that can have a committed root. During the first ~48h of
// e that root may still be inside its dispute window; `claim_open_block`
// in the output says when the claim becomes submittable.
//
// Verification requires three payout roots to agree: the root recomputed
// locally by walking the server-provided proof from the recomputed leaf,
// the server-provided root, and (when --rpc is given) the root read from
// the contract with eth_call — so a verified claim does not rest on
// trusting the platform (decision D-6). Exits nonzero on any mismatch.
func claim(opts docopt.Opts) {
	apiUrl, err := opts.String("--api_url")
	if err != nil {
		apiUrl = DefaultApiUrl
	}

	event := connect.NewEventWithContext(context.Background())
	event.SetOnSignals(syscall.SIGINT, syscall.SIGQUIT, syscall.SIGTERM)

	ctx, cancel := context.WithCancel(event.Ctx())
	defer cancel()

	clientStrategy := connect.NewClientStrategyWithDefaults(ctx)

	if dryRun, _ := opts.Bool("--dry-run"); dryRun {
		fmt.Printf("note: claim is verify-only and never submits; --dry-run has no effect\n")
	}

	byJwt, err := readNetworkJwt()
	if err != nil {
		panic(err)
	}
	api := connect.NewBringYourApi(ctx, clientStrategy, apiUrl)
	api.SetByJwt(byJwt)

	var rpcUrls []string
	if rpcAny, ok := opts["--rpc"]; ok && rpcAny != nil {
		rpcUrls = append(rpcUrls, rpcAny.([]string)...)
	}

	epoch := uint64(0)
	epochNote := ""
	if epochStr, epochErr := opts.String("--epoch"); epochErr == nil && epochStr != "" {
		epoch, err = strconv.ParseUint(epochStr, 10, 64)
		if err != nil {
			panic(fmt.Errorf("bad --epoch %q: %s", epochStr, err))
		}
	} else {
		epochResult, err := api.SnEpochSync()
		if err != nil {
			panic(err)
		}
		if epochResult.Epoch == 0 {
			panic(fmt.Errorf("current epoch is 0; no finalized epoch to claim yet"))
		}
		epoch = epochResult.Epoch - 1
		epochNote = fmt.Sprintf(" (last finalized; current epoch is %d. Use --epoch to override)", epochResult.Epoch)
	}

	poolClaim, err := api.SnPoolClaimSync(&connect.SnPoolClaimArgs{
		Epoch: epoch,
	})
	if err != nil {
		panic(err)
	}

	// decode and sanity-check the claim fields
	if len(poolClaim.NoId) == 0 {
		panic(fmt.Errorf("claim has no no_id"))
	}
	noId, err := evmWordFromBytes(poolClaim.NoId)
	if err != nil {
		panic(fmt.Errorf("bad no_id: %s", err))
	}
	if len(poolClaim.Coldkey) != 32 {
		panic(fmt.Errorf("bad coldkey length %d; expected 32", len(poolClaim.Coldkey)))
	}
	var coldkey [32]byte
	copy(coldkey[:], poolClaim.Coldkey)
	if len(poolClaim.PayoutRoot) != 32 {
		panic(fmt.Errorf("bad payout root length %d; expected 32", len(poolClaim.PayoutRoot)))
	}
	var serverRoot [32]byte
	copy(serverRoot[:], poolClaim.PayoutRoot)
	if poolClaim.ShareBps < 0 {
		panic(fmt.Errorf("bad share_bps %d", poolClaim.ShareBps))
	}
	shareBps := uint64(poolClaim.ShareBps)
	proof := make([][32]byte, len(poolClaim.Proof))
	for i, proofElement := range poolClaim.Proof {
		if len(proofElement) != 32 {
			panic(fmt.Errorf("bad proof element %d length %d; expected 32", i, len(proofElement)))
		}
		copy(proof[i][:], proofElement)
	}

	// recompute the leaf and walk the proof locally — the server root is
	// never trusted blindly
	leaf := snPayoutLeaf(coldkey, shareBps)
	proofRoot := snMerkleRootFromProof(leaf, proof)

	// read the on-chain root, trying each --rpc endpoint in order until
	// one answers both eth_chainId and eth_call
	chainChecked := false
	var chainRoot [32]byte
	var chainId uint64
	var chainRpcUrl string
	if 0 < len(rpcUrls) {
		noCommitCalldata := snNoCommitCalldata(epoch, noId)
		for _, rpcUrl := range rpcUrls {
			chainIdHex, rpcErr := ethRpcHexResult(ctx, rpcUrl, "eth_chainId", []any{})
			if rpcErr != nil {
				fmt.Printf("rpc %s: %s\n", rpcUrl, rpcErr)
				continue
			}
			rpcChainId, rpcErr := parseEthHexQuantity(chainIdHex)
			if rpcErr != nil {
				fmt.Printf("rpc %s: bad eth_chainId %q\n", rpcUrl, chainIdHex)
				continue
			}
			callHex, rpcErr := ethRpcHexResult(ctx, rpcUrl, "eth_call", []any{
				map[string]any{
					"to":   poolClaim.ContractAddress,
					"data": fmt.Sprintf("0x%x", noCommitCalldata),
				},
				"latest",
			})
			if rpcErr != nil {
				fmt.Printf("rpc %s: %s\n", rpcUrl, rpcErr)
				continue
			}
			returnData, rpcErr := parseEthHexBytes(callHex)
			if rpcErr != nil || len(returnData) < 32 {
				fmt.Printf("rpc %s: noCommit returned %d bytes; expected >= 32 (wrong contract address?)\n", rpcUrl, len(returnData))
				continue
			}
			copy(chainRoot[:], returnData[:32])
			chainId = rpcChainId
			chainRpcUrl = rpcUrl
			chainChecked = true
			break
		}
		if !chainChecked {
			fmt.Printf("status: UNVERIFIED — no --rpc endpoint answered\n")
			os.Exit(1)
		}
	}

	// all roots must agree: recomputed-from-proof, server-provided, and
	// (when --rpc is given) on-chain
	mismatches := []string{}
	if proofRoot != serverRoot {
		mismatches = append(mismatches, "the proof does not verify against the server payout root")
	}
	if chainChecked {
		if chainRoot == ([32]byte{}) {
			mismatches = append(mismatches, "the on-chain payout root is zero (epoch not committed on-chain yet?)")
		} else if chainRoot != serverRoot {
			mismatches = append(mismatches, "the server payout root does not match the on-chain root")
		}
		if chainId != poolClaim.ChainId {
			mismatches = append(mismatches, fmt.Sprintf("chain id mismatch: rpc says %d, server says %d", chainId, poolClaim.ChainId))
		}
	}

	fmt.Printf("epoch: %d%s\n", epoch, epochNote)
	fmt.Printf("no_id: 0x%x\n", noId)
	fmt.Printf("coldkey: 0x%x\n", coldkey)
	fmt.Printf("share_bps: %d (%.2f%%)\n", shareBps, float64(shareBps)/100.0)
	fmt.Printf("payout_root (server): 0x%x\n", serverRoot)
	fmt.Printf("payout_root (proof): 0x%x (recomputed, %d-element proof)\n", proofRoot, len(proof))
	if chainChecked {
		fmt.Printf("payout_root (chain): 0x%x (via %s, chain id %d)\n", chainRoot, chainRpcUrl, chainId)
	} else {
		fmt.Printf("payout_root (chain): not checked. Pass --rpc=<rpc_url> to verify against the contract\n")
	}
	fmt.Printf("contract: %s (chain id %d)\n", poolClaim.ContractAddress, poolClaim.ChainId)
	fmt.Printf("claim_open_block: %d\n", poolClaim.ClaimOpenBlock)

	claimCalldata := snClaimMinerCalldata(epoch, noId, coldkey, shareBps, proof)
	fmt.Printf("claimMiner calldata:\n0x%x\n", claimCalldata)
	fmt.Printf("submit with: snclaim submit --rpc=<rpc_url> --contract=%s --calldata=0x%x\n", poolClaim.ContractAddress, claimCalldata)

	if 0 < len(mismatches) {
		for _, mismatch := range mismatches {
			fmt.Printf("mismatch: %s\n", mismatch)
		}
		fmt.Printf("status: MISMATCH — do not submit\n")
		os.Exit(1)
	}
	if chainChecked {
		fmt.Printf("status: VERIFIED (proof, server, and on-chain roots agree)\n")
	} else {
		fmt.Printf("status: VERIFIED against the server root only\n")
	}
}

// ---------------------------------------------------------------------
// Head-tier claim — client_id <-> hotkey binding (WHITEPAPER §8.4/§11.4,
// decisions D-6/D-18). A top-level (head) miner runs its own UID and is
// steered by validators on pure measured quality; to be measured it must
// publish a dual-signed association between the client_id its trails are
// measured under and its subnet hotkey. This binary owns the client key,
// so it produces the client_id signature and prints the binding intent;
// the EVM key and tx submission live in the separate snclaim tool, which
// packs and sends bindHead/unbindHead. Stdlib + x/crypto only — no
// ethereum module, no stabi (mirrors the `provider claim` split).
// ---------------------------------------------------------------------

// snHeadBindDigestSignature is the canonical abi signature of the
// headBindDigest view. The digest is domain-separated and includes the
// registrant EVM address, so it is bound to a single submitting wallet
// (sn/evm/src/STSubnet.sol headBindDigest).
const snHeadBindDigestSignature = "headBindDigest(address,bytes32,bytes32)"

// snHeadBindDigestCalldata abi-encodes a
// headBindDigest(address registrant, bytes32 hotkey, bytes32 clientId)
// view call: three head words — the registrant address left-padded to a
// 32-byte word, then the hotkey and clientId words verbatim. The return
// data's first 32 bytes are the digest to sign.
func snHeadBindDigestCalldata(registrant [20]byte, hotkey [32]byte, clientId [32]byte) []byte {
	selector := evmSelector(snHeadBindDigestSignature)
	calldata := make([]byte, 0, 4+32*3)
	calldata = append(calldata, selector[:]...)
	var registrantWord [32]byte
	copy(registrantWord[12:], registrant[:])
	calldata = append(calldata, registrantWord[:]...)
	calldata = append(calldata, hotkey[:]...)
	calldata = append(calldata, clientId[:]...)
	return calldata
}

// snBindHeadIntent is the provider→snclaim handoff for a head binding:
// everything snclaim needs to pack and submit bindHead, plus the digest
// that was signed (for display / cross-checks).
type snBindHeadIntent struct {
	hotkey      [32]byte
	clientId    [32]byte // the provider's client Ed25519 public key (ckey)
	registrant  [20]byte
	digest      [32]byte
	clientIdSig []byte // 64-byte Ed25519 signature (R‖S) by clientId over digest
}

// snSignBindHead signs the on-chain headBindDigest with the provider's
// client Ed25519 private key (the `.provider.key` identity, the same key
// that produces the `/verify` vpk signatures). ed25519.Sign returns the
// standard 64-byte signature R‖S; the contract splits it r=sig[0:32],
// s=sig[32:64] and verifies via the 0x402 precompile (whose r is "the
// first 32 bytes" and s "the second 32 bytes"), so this byte order maps
// directly with no reordering — exactly as registerValidator's
// ed25519Sig is split (sn/evm/src/STSubnet.sol bindHead).
func snSignBindHead(clientPrivateKey ed25519.PrivateKey, registrant [20]byte, hotkey [32]byte, digest [32]byte) *snBindHeadIntent {
	intent := &snBindHeadIntent{
		hotkey:      hotkey,
		registrant:  registrant,
		digest:      digest,
		clientIdSig: ed25519.Sign(clientPrivateKey, digest[:]),
	}
	copy(intent.clientId[:], clientPrivateKey.Public().(ed25519.PublicKey))
	return intent
}

// snLoadClientKey loads the provider's long-lived Ed25519 identity key
// from ~/.urnetwork/.provider.key (the raw 32-byte seed). This is the
// client_id/ckey used for `/verify`; `provider provide` generates and
// persists it on first run.
func snLoadClientKey() (ed25519.PrivateKey, error) {
	seed, err := readProviderClientKeySeed()
	if err != nil {
		return nil, err
	}
	if len(seed) == 0 {
		p, _ := providerStatePath(".provider.key")
		return nil, fmt.Errorf("provider client key not found at %s. Run `provider provide` once to generate the client identity key", p)
	}
	if len(seed) != ed25519.SeedSize {
		return nil, fmt.Errorf("provider client key seed length %d; expected %d", len(seed), ed25519.SeedSize)
	}
	return ed25519.NewKeyFromSeed(seed), nil
}

// snReadHeadBindDigest reads the exact 32-byte headBindDigest from the
// contract via eth_call, trying each rpc endpoint in order until one
// answers both eth_chainId and eth_call (failover, like `provider
// claim`). Per-endpoint failures are printed; the digest binds
// block.chainid and the contract address internally, so no chain id
// needs to be supplied.
func snReadHeadBindDigest(ctx context.Context, rpcUrls []string, contractHex string, calldata []byte) (digest [32]byte, chainId uint64, rpcUrl string, err error) {
	for _, url := range rpcUrls {
		chainIdHex, rpcErr := ethRpcHexResult(ctx, url, "eth_chainId", []any{})
		if rpcErr != nil {
			fmt.Printf("rpc %s: %s\n", url, rpcErr)
			continue
		}
		cid, rpcErr := parseEthHexQuantity(chainIdHex)
		if rpcErr != nil {
			fmt.Printf("rpc %s: bad eth_chainId %q\n", url, chainIdHex)
			continue
		}
		callHex, rpcErr := ethRpcHexResult(ctx, url, "eth_call", []any{
			map[string]any{
				"to":   contractHex,
				"data": fmt.Sprintf("0x%x", calldata),
			},
			"latest",
		})
		if rpcErr != nil {
			fmt.Printf("rpc %s: %s\n", url, rpcErr)
			continue
		}
		returnData, rpcErr := parseEthHexBytes(callHex)
		if rpcErr != nil || len(returnData) < 32 {
			fmt.Printf("rpc %s: headBindDigest returned %d bytes; expected >= 32 (wrong contract address?)\n", url, len(returnData))
			continue
		}
		copy(digest[:], returnData[:32])
		return digest, cid, url, nil
	}
	return digest, 0, "", fmt.Errorf("no --rpc endpoint answered headBindDigest")
}

// parseBytes32Arg parses a 0x-optional 32-byte hex argument (hotkey or
// client_id).
func parseBytes32Arg(field string, s string) ([32]byte, error) {
	var out [32]byte
	h := strings.TrimPrefix(strings.TrimPrefix(strings.TrimSpace(s), "0x"), "0X")
	b, err := hex.DecodeString(h)
	if err != nil {
		return out, fmt.Errorf("%s: %s", field, err)
	}
	if len(b) != 32 {
		return out, fmt.Errorf("%s: %d hex bytes; expected 32", field, len(b))
	}
	copy(out[:], b)
	return out, nil
}

// parseEvmAddressArg parses a 0x-optional 20-byte hex EVM address.
func parseEvmAddressArg(field string, s string) ([20]byte, error) {
	var out [20]byte
	h := strings.TrimPrefix(strings.TrimPrefix(strings.TrimSpace(s), "0x"), "0X")
	b, err := hex.DecodeString(h)
	if err != nil {
		return out, fmt.Errorf("%s: %s", field, err)
	}
	if len(b) != 20 {
		return out, fmt.Errorf("%s: %d hex bytes; expected a 20-byte EVM address", field, len(b))
	}
	copy(out[:], b)
	return out, nil
}

// bindHead implements `provider bind-head --hotkey=<hex>
// --registrant=<0xEVMaddr> --contract=<addr> [--rpc=<rpc_url>]...`. It
// signs the on-chain headBindDigest with the provider's client key and
// prints the ready-to-submit bindHead inputs. It never touches EVM key
// material; snclaim submits the transaction.
func bindHead(opts docopt.Opts) {
	fail := func(err error) {
		fmt.Printf("bind-head failed: %s\n", err)
		os.Exit(1)
	}

	hotkeyStr, _ := opts.String("--hotkey")
	hotkey, err := parseBytes32Arg("--hotkey", hotkeyStr)
	if err != nil {
		fail(err)
	}
	registrantStr, _ := opts.String("--registrant")
	registrant, err := parseEvmAddressArg("--registrant", registrantStr)
	if err != nil {
		fail(err)
	}
	contractStr, _ := opts.String("--contract")
	contract, err := parseEvmAddressArg("--contract", contractStr)
	if err != nil {
		fail(err)
	}
	var rpcUrls []string
	if rpcAny, ok := opts["--rpc"]; ok && rpcAny != nil {
		rpcUrls = append(rpcUrls, rpcAny.([]string)...)
	}
	if len(rpcUrls) == 0 {
		fail(fmt.Errorf("--rpc: at least one endpoint required to read headBindDigest"))
	}

	privateKey, err := snLoadClientKey()
	if err != nil {
		fail(err)
	}
	var clientId [32]byte
	copy(clientId[:], privateKey.Public().(ed25519.PublicKey))

	event := connect.NewEventWithContext(context.Background())
	event.SetOnSignals(syscall.SIGINT, syscall.SIGQUIT, syscall.SIGTERM)
	ctx, cancel := context.WithCancel(event.Ctx())
	defer cancel()

	contractHex := fmt.Sprintf("0x%x", contract)
	digestCalldata := snHeadBindDigestCalldata(registrant, hotkey, clientId)
	digest, chainId, rpcUrl, err := snReadHeadBindDigest(ctx, rpcUrls, contractHex, digestCalldata)
	if err != nil {
		fail(err)
	}

	intent := snSignBindHead(privateKey, registrant, hotkey, digest)

	fmt.Printf("head binding intent (bindHead)\n")
	fmt.Printf("hotkey: 0x%x\n", intent.hotkey)
	fmt.Printf("client_id (ckey): 0x%x\n", intent.clientId)
	fmt.Printf("client_id_sig: 0x%x (64-byte Ed25519 R‖S by client_id over the digest)\n", intent.clientIdSig)
	fmt.Printf("digest: 0x%x (headBindDigest, via %s, chain id %d)\n", intent.digest, rpcUrl, chainId)
	fmt.Printf("registrant: 0x%x\n", intent.registrant)
	fmt.Printf("contract: %s (chain id %d)\n", contractHex, chainId)
	fmt.Printf("note: registrant MUST equal the snclaim EVM sender. The digest is bound to it, and bindHead reverts unless mirror(sender) equals the hotkey's on-chain coldkey (mirror-gated, like registerValidator).\n")
	fmt.Printf("submit with: snclaim bind-head --hotkey=0x%x --client_id=0x%x --sig=0x%x --contract=%s --rpc=%s --key_file=<evm_key_file>\n",
		intent.hotkey, intent.clientId, intent.clientIdSig, contractHex, rpcUrl)
}

// unbindHead implements `provider unbind-head --hotkey=<hex>
// [--contract=<addr>]`. Unbind is mirror-gated only (no client signature),
// so this just normalizes the hotkey and prints the unbindHead intent for
// snclaim to submit.
func unbindHead(opts docopt.Opts) {
	fail := func(err error) {
		fmt.Printf("unbind-head failed: %s\n", err)
		os.Exit(1)
	}

	hotkeyStr, _ := opts.String("--hotkey")
	hotkey, err := parseBytes32Arg("--hotkey", hotkeyStr)
	if err != nil {
		fail(err)
	}
	contractHint := "<contract>"
	if contractStr, _ := opts.String("--contract"); strings.TrimSpace(contractStr) != "" {
		contract, err := parseEvmAddressArg("--contract", contractStr)
		if err != nil {
			fail(err)
		}
		contractHint = fmt.Sprintf("0x%x", contract)
	}

	fmt.Printf("head unbind intent (unbindHead)\n")
	fmt.Printf("hotkey: 0x%x\n", hotkey)
	fmt.Printf("note: unbind needs no signature — it is mirror-gated only. The snclaim EVM sender's mirror must equal the hotkey's on-chain coldkey.\n")
	fmt.Printf("submit with: snclaim unbind-head --hotkey=0x%x --contract=%s --rpc=<rpc_url> --key_file=<evm_key_file>\n", hotkey, contractHint)
}

# Random numbers: move to math/rand/v2 and secure generators

Status: implemented, 2026-11-13 (owner: "go ahead and implement"). Section 6 is now the "Randomness" section of CODESTYLE.md. Section 7 records where the implementation differs from this proposal.

## 1. The question

`math/rand` is used across our code for random delays, retry jitter, path, exit and strategy selection, padding and sampling. Some of these uses exist to make traffic analysis harder: an observer who could predict them could separate our traffic from noise or correlate flows.

Keys, nonces and other secrets already come from `crypto/rand`.

So the question is whether the rest should move from `math/rand` to `math/rand/v2`, whose default generator is ChaCha8.

## 2. What Go does today

Everything below was checked against the go1.27.4 sources. Every repo's go.mod says `go 1.27.4`, and none sets a `godebug` directive.

### The global generators

- **Top-level `math/rand` functions** (`rand.Intn`, `rand.Shuffle`, `rand.Float64`, and so on) draw from `runtime.rand`, unless `GODEBUG=randautoseed=0` is set. See `globalRand` in `math/rand/rand.go`.
  - `runtime.rand` is a per-thread ChaCha8 generator (`internal/chacha8rand`), seeded from the operating system (`runtime/rand.go`).
  - These calls are unpredictable today.
- **`rand.Seed` is a no-op by default since Go 1.24** (`randseednop`).
  - With `GODEBUG=randseednop=0` or `GODEBUG=randautoseed=0`, the global `math/rand` generator falls back to the Go 1 source and becomes predictable.
  - We set neither, but a deployment or an embedding app could.
- **Top-level `math/rand/v2` functions** use the same `runtime.rand`, and no GODEBUG setting changes them.

### Generators a program builds itself

- **`rand.New(rand.NewSource(seed))` in v1** is the Go 1 additive lagged Fibonacci source.
  - Its outputs reveal its state.
  - A seed from the clock, an id or a counter can be guessed.
  - Seeding it from `crypto/rand` hides the start, but not the sequence after enough outputs.
- **`rand.NewPCG` in v2** is fast and reproducible, and just as predictable from its outputs.
- **`rand.NewChaCha8(seed)` in v2** resists prediction as long as its 32-byte seed is secret.

### crypto/rand

`crypto/rand.Read` never returns an error. On failure it crashes the program, so the code that falls back after a failed `crypto/rand.Read` never runs.

### What this means

The import path is not the risk. Changing `math/rand` to `math/rand/v2` changes nothing for a top-level call today.

The risks are:
- generators a program builds and seeds itself;
- the old global generator's dependence on GODEBUG defaults.

## 3. Inventory, origin/main, 2026-11-13

| Repo | `math/rand` imports | `math/rand/v2` imports | Production code that builds a generator |
|---|---|---|---|
| connect | 61 | 15 | 4 |
| server | 100 | 72 | 15 (of which 6 below matter; the rest are simulations, tools, vendored code or unrelated names) |
| sdk | 4 | 4 | 1 (build-time generator, deterministic on purpose) |
| sn | 7 | 19 | vendored client code only |
| proxy | 1 | 0 | 0 |

The import counts include tests, which may keep deterministic generators.

### Production sites that matter

| # | Site | What it randomizes | Today | Risk |
|---|---|---|---|---|
| C1 | connect `net_strategy_score.go:230` | Exploration and retention of dial strategies, which decides which transport and camouflage strategy a client tries | Go 1 source seeded from `time.Now().UnixNano()` | Predictable from the process start time. The choice shows on the wire. |
| C2 | connect `gossip/node.go:289` | Gossip peer sampling and timing | Go 1 source seeded from `settings.RandSeed`, or else `settings.Now().UnixNano()` | Predictable from the start time in production. |
| C3 | connect `transport_p2p_webrtc_stun.go:68-75` | The STUN/ICE server subset per session | Go 1 source seeded from `crypto/rand`, with a fixed seed 1 as fallback | Few draws per session. The fallback never runs (section 2). |
| S1 | server `qualityprobe/egresshealth/egresshealth.go:1076-1080` | Which destinations and URLs a probe pass asks for | Go 1 source seeded from `crypto/rand`, with a clock seed as fallback | A provider that predicts probes could answer only those. |
| S2 | server `controller/provider_egress_destination_controller.go:609` | The order of the destination pool served to probers | Go 1 source seeded from `time.Now().UnixNano()` | Predictable from the request time. Same probe-gaming concern. |
| S3 | server `connect/move.go:337` | Migration time jitter for moves | v2 PCG seeded from the runtime generator | Predictable from its outputs. Internal timing. |
| S4 | server `controller/st_client_key_evidence_writer.go:321` | Sampling of evidence writes | v2 PCG seeded from the runtime generator | Internal. Change for consistency. |
| S5 | server `stats/metrics.go:47` | Metric sampling | Go 1 source, fixed seed 1 | Internal. Change for consistency. |

### Left as they are

- **Vendored code:** `third_party/quic-go` seeds PCG from `crypto/rand` upstream, and the gvisor `tcpip` stack seeds its source from `crypto/rand`. go-substrate-rpc-client is vendored too. These are upstream patterns; review them separately if we fork further.
- **Deterministic on purpose:** simulations and tools, such as `connect/sim-latency`, the competition latency simulation, `runner/perf`, the sdk `js/gen_lag.go` generator, and the finance liquidation history. These keep their seeds.

### The other connect imports

The other `math/rand` imports in connect production code call the top-level functions:
- `transfer*.go`, `ip_remote_multi_client*.go`, `net_http*.go`, `net_extender*.go`, `transport_*.go`, `tun.go`, `util.go`, `extender/*`.

They are safe today only through the GODEBUG defaults described in section 2.

## 4. Proposed fix

### 4.1 One way to get randomness

1. **Secrets, keys, nonces, tokens and unguessable ids** use `crypto/rand`. This is unchanged.
2. **All other randomness in production code** uses the top-level `math/rand/v2` functions.
3. **When a type needs its own generator** (to own a stream, or to accept an injected one in tests), production code builds it with one helper:

   ```go
   // a ChaCha8 generator seeded from crypto/rand. Not safe for concurrent use;
   // the caller owns it or guards it.
   func NewSecureRand() *mathrand.Rand {
       var seed [32]byte
       // never returns an error since Go 1.24: a failing source crashes the program
       cryptorand.Read(seed[:])
       return mathrand.New(mathrand.NewChaCha8(seed))
   }
   ```

   - No generator in production code is seeded from the clock, an id, a counter or a fixed value.
   - No code falls back to such a generator.
   - Production code never uses the Go 1 source.
4. **Deterministic generators** (`rand.NewPCG`, or `rand.NewChaCha8` with a fixed seed) stay only in tests, benchmarks and simulations, with a comment saying why.
   - A production type that accepts an injected `*rand.Rand` for tests defaults to `NewSecureRand()`.

### 4.2 Code changes

- **connect:**
  - Add `NewSecureRand` (`random.go`).
  - C1 and C3 use it.
  - C2 uses it when `settings.RandSeed` is 0. Tests that set `RandSeed` get a deterministic v2 generator.
  - Then migrate every `math/rand` import to `math/rand/v2`.
- **server:**
  - S1 and S2 use `NewSecureRand()`, or the top-level functions where the generator is used once per call.
  - S3, S4 and S5 do the same.
  - Then migrate the imports.
- **sdk, proxy, sn:** migrate the imports. They have no production generators to change.
- **API mapping (v1 to v2):**

  | v1 | v2 |
  |---|---|
  | `Intn` | `IntN` |
  | `Int63n` | `Int64N` |
  | `Int31n` | `Int32N` |
  | `Int63` | `Int64` |
  | `Int31` | `Int32` |

  - `Float64`, `Perm` and `Shuffle` are unchanged.
  - `rand.Read` does not exist in v2; use `crypto/rand`.
  - Duration jitter: v2 has `rand.N(d)` for a `time.Duration`.
- **Concurrency:**
  - The top-level v2 functions are safe for concurrent use and need no lock.
  - A `*rand.Rand` is not safe for concurrent use. Types that share one keep their existing lock.

### 4.3 Guard

Each repo gets a test, `random_rule_test.go`, that parses its non-test Go files with `go/parser` and fails on any of:
- an import of `math/rand` (v1);
- a call to `NewSource`, `NewPCG` or `Seed` outside an allowlist (`third_party/`, simulation and tool directories);
- a call to `NewChaCha8` outside the `NewSecureRand` helper.

The test names each offending file and line.

### 4.4 Tests per changed site

- Two instances built by the production constructor produce different sequences under a fixed fake clock, which shows there is no time dependence.
- An injected deterministic generator still reproduces the existing test expectations.
- The packages that change locking run under `-race`.

### 4.5 Rollout

There is no wire or protocol change, and the distributions are the same. The fix ships with each component's next release:

| Component | Carries |
|---|---|
| Server services | taskworker (S1, S4), api and alt (S2), connect service (S3) |
| Proxy | the next build with the new connect |
| Providers | the next nightly miner build, through connect |
| Apps | the next sdk and app builds |

## 5. What does not change

- Uses of `crypto/rand` for keys and nonces, which are already correct.
- Vendored third-party code.
- Simulations and tools that need reproducible streams.

## 6. Proposed CODESTYLE rule

> ## Randomness
>
> - Secrets, keys, nonces, tokens and unguessable ids: `crypto/rand`.
> - All other randomness: the top-level `math/rand/v2` functions (`rand.IntN`, `rand.N`, `rand.Shuffle`, ...). Never import `math/rand`.
> - Production code never builds a seeded generator. A type that needs its own uses `NewSecureRand()` (ChaCha8 seeded from `crypto/rand`). No generator seeded from the clock, an id, a counter or a fixed value, and no fallback to one.
> - Deterministic generators (`rand.NewPCG`, a fixed-seed `rand.NewChaCha8`) belong only in tests, benchmarks and simulations, with a comment saying why.
> - Randomness that shapes traffic is security relevant, however small: padding, timing jitter, path, exit, strategy and server choice, probe and sample selection.
> - `random_rule_test.go` enforces this.

## 7. As built, 2026-11-13

- **Helper:** `connect.NewSecureRand()` in `random.go`, as proposed.
- **C1** draws the retain coin from the store's own `NewSecureRand` generator, under `stateLock` as before.
- **C2:** `NodeSettings.Rand`, an injected `*rand.Rand`, replaces `NodeSettings.RandSeed`.
  - Nil uses `NewSecureRand`. Tests inject a v2 PCG.
  - Building the test generator from `RandSeed` inside `gossip/node.go` would have put a seeded generator in production code, which section 4.1 and the guard rule out.
- **C3:** `cryptoSeededRand` is gone. It also chose the DTLS ClientHello fingerprint (`transport_p2p_webrtc_dtls.go`), which now uses `NewSecureRand` too.
- **S1, S3, S4** use `connect.NewSecureRand()`. **S2** does too: `servePool` builds one per request when no generator is injected.
- **S5** draws its reservoir from the top-level `rand.Int64N`, one draw per sample, with no generator of its own.
- **`rand.Read`:** the pt header, `RandomLocalIpv6` and the server's speed-test chunks now read `crypto/rand`.
- **The simulations keep the Go 1 source.**
  - `connect/sim-latency` and the competition's `providers.yml` generator (`controller/competition_sim_latency_controller.go`) must reproduce recorded runs and the frozen simulator byte for byte. `TestGenerateSimLatencyWorkloadMatchesFrozenSimulator` pins the generator's SHA-256.
  - v2 has no Go 1 source, so these files keep their `math/rand` imports. The guard's allowlist exempts them from all three checks, as it does `third_party/`.
- **Guard:** besides the three checks of 4.3, it counts a `rand.PCG` or `rand.ChaCha8` value as its constructor, because the zero value is a fixed seed. It also flags a dot import of either package, and it fails on an allowlist entry that names nothing.
- **Allowlists:**

  | Repo | Allowlist |
  |---|---|
  | connect | none |
  | server | `third_party/`, `connect/sim-latency/`, `controller/competition_sim_latency_controller.go` |
  | sdk | `js/gen_lag.go` |
  | sn | `third_party/` |
  | proxy | none |

  `runner/perf` is in the tests repository, which this change does not cover.
- **Tests** moved to v2 as well: a test that seeded a Go 1 source now seeds a v2 PCG with the same seed.

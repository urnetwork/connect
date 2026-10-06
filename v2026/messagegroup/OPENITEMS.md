# Open items this package's source cites and no document rules

What this file is for. `connect/messagegroup` cites open items by number — `M1-4`, `M1-6`, `M1-8`,
`M1-15`, `M1-20`, `S2-3`, `J1-1` — and every one of those numbers is assigned in the spec
repository's `SPEC-LEDGER.md`, which is where rulings are made. **This file is not that register and
does not compete with it.** It exists for the one case that register cannot serve on its own: an
obligation discovered *in this code*, whose mechanism is a design ruling, raised in a commit that
has no write access to the ledger.

An entry here is a **debt with a number so the source can cite it**, and it carries its own
migration: when the ledger assigns a number, the entry is rewritten to cite that number and this
file loses a row. Do not treat "it is written here" as "it has been decided".

Two rules, borrowed from `mls/UNOBSERVED.md` because they are the same two rules:

- **Give the obligation and the reproduction, not the worry.** An entry that says "this looks
  unauthenticated" is worth nothing. An entry that names the property, the test that reproduces the
  gap, and what a ruling would have to choose between can be acted on.
- **Close entries by deleting them.** When the mechanism is ruled and implemented, take the row out
  in the same commit and name the case that holds it.

---

## Filed in the ledger and not here

Four symbols this package **invented** for the 2026-09-13 seal lift, because `eph_root[n]` cannot be
derived (MASTER invariant **I4**), its carrier is m1 Task 14's device wrap and that does not exist,
and **no section of the corpus declares a route into a `GroupSession`**:

| symbol | where |
|---|---|
| `GroupSession.InstallEphRoot(ephRoot []byte) error` | `session.go` |
| the unexported `ephRoot []byte` field on `GroupSession` | `session.go` |
| `senderLadderKey{RetentionWire byte; EphWindow uint64}` | `session.go` |
| `TrackSender`'s `ephWindow uint64` parameter | `session.go` |

They are **kept**, on `RebindServerNonce`'s precedent, and each carries its own un-specified status
in the prose beside its declaration. **They get no row of their own here because the ledger has
already assigned them one: `SPEC-LEDGER.md` open item 188, filed 2026-09-13, FILED AND NOT RULED.**

That is this file's migration rule applied rather than bent — a row here is a debt *with no number*,
and once a number exists the source cites the number and this file loses the row. What is recorded
here is the pointer alone, so that a reader of connect's open items meets the four names, learns
that no document declares them, and is sent to the register that owes the ruling.

---


## MG-1 — `GroupEngine.JoinFromWelcome` states an anchoring obligation and nothing performs it

**Status: OPEN. The obligation is stated; the mechanism is a ruling and is not this package's.**

### The property

A Welcome authenticates nobody. It is HPKE-sealed to an init key its recipient **published**, in a
key package that went to the delivery service and to every member of every group that ever added
this device. Anybody at all holding that key package can build a well-formed Welcome addressed to
it: mint a ratchet tree, sign a `GroupInfo` at a leaf of that tree with a signing key drawn for the
purpose, choose a joiner secret, seal it to the published init key.

`mls.JoinFromWelcome` says this from the layer below, at length, under *"WHAT A CALLER MUST
ESTABLISH BEFORE IT CALLS THIS"*. **`GroupEngine.JoinFromWelcome` is the surface an application
calls**, and until this item was filed it stated no obligation of its own — so the sentence that
matters was fifteen lines deep in a package section 2.2 forbids the message server to link and
section 6 exists to keep an app from naming.

### The reproduction

`TestAWelcomeFromAnAttackerJoinsAndTheOnlyThingItGetsWrongIsWhoTheGroupIs` in
`enginejoin_test.go`. An engine that has never met the victim, holding only a key package the victim
published, founds a group, adds the victim and hands over the Welcome. The victim's
`JoinFromWelcome` succeeds. Group id, epoch, member count and the `URmessage/v1/storage` exporter
all agree between attacker and victim, because the group is real — the attacker founded it. The one
thing that is false is the thing no octet on that path carries: that this is the group the user
meant to be in.

### What a ruling has to choose

Nothing below is decided and this package must not decide it.

1. **What the anchor IS.** A member identity the joiner expected (readable today through
   `GroupHandle.MemberAt`); a group id the joiner was told out of band; a signature over the
   invitation by a key the joiner already trusted; or the key-transparency log.
2. **Where the expectation comes from**, which is the half that is not a crypto question: a user
   action, a contact list, a prior group, a server-delivered invitation that is itself anchored.
3. **What a device does when the anchor is absent** — refuse the join, join but refuse to render,
   or join and surface the group as unverified. These are different products, not different
   implementations.
4. **Whose obligation it is**, and this item takes no position: the ruling may put it on `sdk`,
   on the application, or on a new method of section 6.

### Why it is not closed here

An anchoring mechanism chosen in an adapter is a security argument taken by the layer with the
least context to take it — the same argument spec B section 2.2 makes about the message server and
an MLS parser. What this package can do, and has done, is state the obligation at the surface that
owns it and make the absence visible: `GroupEngine`'s header carries it, `doc.go`'s inventory
carries it, and the reproduction above fails the day the behaviour changes.

### What is owed elsewhere

A `SPEC-LEDGER.md` number, assigned by whoever owns that register. This row is the placeholder that
lets `engine.go` cite something rather than nothing, and it is replaced by that number the moment
one exists.

---

## MG-2 — `SealRecord` reads its own clock for `eph_window` and the caller reads a second one for `sent_at`

**Status: OPEN. The gap is real, the closure is a change to a published signature, and this package
may not make one on its own authority.**

### The property

MASTER §8.1 and Spec A §5.3 make `eph_window` the **sender's** computation *"from the same
wall-clock reading it puts in `sent_at`"*:

```
t = floor(sent_at_ms / (eph_bucket_seconds[b] * 1000))     for b in 1..5
```

*The same reading.* One instant, used twice: once inside `ct_head`'s plaintext as `sent_at`, and
once in the clear as `eph_window`.

This package cannot do that. `sealRecordOnLoop` takes exactly one reading — `self.nowMs()`, the
injected clock — and uses it for `expire_at`'s refusal and for the window. `sent_at` is not
this layer's: it lives inside `headPlain`, which is an opaque `[]byte` argument, and Spec A §5.2
publishes `SealRecord`'s signature with **no `sentAt` parameter in it**:

```go
func (self *GroupSession) SealRecord(
    class RetentionClass, ephBucket uint8, isCommit bool,
    headPlain []byte, bodyPlain []byte, expireAt uint64,
    serverAttachment *ServerAttachment,
) (*Record, error)
```

So a caller that builds `headPlain` from its own clock read and then calls `SealRecord` has made
**two** readings, and if they straddle a bucket boundary the record's `sent_at` and its
`eph_window` name two different windows.

### What it costs, measured rather than feared

Less than it sounds, and the honest statement of it is the useful one. The record stays internally
consistent: `eph_window` is on the wire, in both AADs and in the `write_auth` preimage, and the key
is `EphKey(eph_root, bucket, the wire value)` — so it seals, it opens, and every implementation
agrees. What moves is the record's **key lifetime**, which is pinned to the sealer's reading rather
than to the `sent_at` a reader will see. The straddle window is one clock read wide and the
consequence is at most one bucket, which is inside the ±1 the server's own check (Spec A **S19**,
Spec B §5.1 check 3) already allows against *arrival*. It is a discrepancy, not an incompatibility.

### Reproduction

`TestEverySealableClassRoundTripsAndTheWrapItemOneEightyFiveRefusesDoesNot` in `ephkey_test.go`
asserts that the window the sealer writes equals `EphWindowAt(bucket, the fixture clock)` — that is,
that the window comes from **this** clock. There is no case, and can be no case at this layer, that
the window comes from the `sent_at` inside `headPlain`: nothing here parses that plaintext.

### What a ruling would have to choose between

1. **Add `sentAtMs int64` to `SealRecord`** and derive the window from it — the only shape that
   makes the specification's "the same reading" literally true. It is a change to a signature
   Spec A §5.2 publishes in a Go block, so it is a spec edit and not a code edit.
2. **State that the sealer's own clock is the sender's clock** and that `sent_at` is required to be
   consistent with it, moving the obligation to the caller in writing. Costs nothing here and makes
   the corpus say what the code does.
3. **Leave it**, on the measurement above, and record the discrepancy where a second implementer
   will meet it. That is what this row is.

### What is owed elsewhere

A `SPEC-LEDGER.md` number and one sentence from the owner. Option 1 is the only one that touches
this package's code, and it touches a published signature, which is why nothing is chosen here.
---

## MG-3 — a clock bound AFTER the test binary changes `EphKey`'s output and no gate in this tree sees it

**Status: OPEN, and FILED NOT RULED. Measured on 2026-09-13 while closing the clock gate line. The
mechanism that would close it is a rule about how this module may be composed, which is not this
package's to invent.**

### The property

`EphKey` must be a pure function of `(eph_root, bucket, window)`. Two gates are supposed to hold
that between them:

1. `TestEphKeyIsMasterSection81sDerivationAndNotThisPackagesOpinionOfIt` pins **twenty seven**
   known answers computed outside this module from MASTER §8.1 and RFC 5869 — two `eph_root`s,
   every rung of the ladder under both, ten windows. It defends the **output**, at POINTS. It
   pinned **five** when this row was filed, over one root and three of six rungs; seventeen after
   the first widening on 2026-09-13; twenty seven after the second, which added the window a real
   sender computes for every rung at `2026-01-01T00:00:00Z`.
2. `TestEphKeyReachesNoClockSourceInThisPackage` walks the reference graph. It defends the
   **control flow**, and it is explicitly not total — `ephkey_test.go` names what it cannot see.
3. `TestEphKeyIsAFunctionOfItsThreeArgumentsOverDrawnInputs` — added 2026-09-13, second pass —
   compares `EphKey` against an HKDF-Expand written out in `ephpurity_test.go`, over inputs that
   are **drawn** rather than listed. It defends the **output** too, but by MEASURE rather than at
   points: it catches an influence in proportion to how often that influence fires under the draw.
   It is the first gate in this directory whose reach is not a list.

The argument for stopping the gate/counter-gate race was that (1) backstops (2): a clock the graph
misses still has to change the derived octets, and the known answers would kill it. **That argument
is false in TWO independent directions, and this row is one of them.**

- **BINDING TIME** — the hook is nil in the binary the known answers run in, so in that binary
  `EphKey` really is pure. **This row.** No table of any width reaches it.
- **COVERAGE** — the known answers are a finite SAMPLE of `(root, bucket, window)`, and purity is a
  per-input property, so a clock conditioned on a point the table does not carry is invisible to
  them *however early it is bound*. Measured by the close-out review on `993a4ea`: two plants, one
  fired `if bucket == 3` and one `if ephRoot[0] != 0xE0`, each value-changing, each clearing every
  gate in this tree at the clean baseline exactly. **Repaired on 2026-09-13 by widening the table**
  — every rung under two roots — which turns both of those plants red. The class is narrowed and
  not closed: a plant fired on a third root, or on an unpinned window, still passes, and
  `ephkey_test.go`'s P5 header carries both measurements.

The two are independent, and telling them apart is what the remedy menu below had wrong.

### The reproduction

Three files, all inside this module, none of them touching a published signature:

```go
// messagegroup/atkhook.go
var ephHook func(uint8) byte
func SetEphHook(hook func(uint8) byte) { ephHook = hook }

// messagegroup/eph.go, inside EphKey after refuseOffLadderBucket(bucket)
stirred := append([]byte(nil), ephRoot...)
if ephHook != nil {
    stirred[0] ^= ephHook(bucket)
}
return keyScheduleExpand(stirred, ephLabelledInfo(bucket, window), ephKeyBytes)

// atkroot/install.go -- a leaf package nothing in the module imports
func init() {
    messagegroup.SetEphHook(func(uint8) byte { return byte(time.Now().UnixMilli()%255) + 1 })
}
```

Measured at `e17cfad` plus the repair that carries this row:

| gate | result |
|---|---|
| `TestEphKeyReachesNoClockSourceInThisPackage` | **green** |
| `TestThisPackageIsBuiltFromExactlyTheseImports` | **green** — no new import here |
| `mls`'s `TestTheCryptoIsBuiltFromExactlyThesePackages` | **green** |
| `TestEphKeyIsMasterSection81sDerivationAndNotThisPackagesOpinionOfIt` | **green** |
| the whole of `./messagegroup/` | **green** |
| a test in `atkroot` that installs the hook and calls `EphKey` | `EphKey(root,1,0)` = `8a425dfc…`, and MASTER §8.1's known answer is `8b1a9428…` |

So the plant **is** value-changing, in the only binary that matters, and every gate in this tree is
green over it. The known answers cannot see it because `ephHook` is **nil in the binary they run
in**: `messagegroup`'s test binary never links a composition root, so in that binary `EphKey` really
is pure.

### What this is not

It is **not** a shipped defect. No such hook exists; `EphKey` takes `window uint64` as an argument
and reads no clock, and `git grep` finds no setter of this shape in the package. The row is about
the **class of defect no gate here would catch**, and it is filed because the commit that stops the
clock-gate race would otherwise be claiming a backstop it does not have.

### The narrowing that WAS taken, and exactly how far it reaches

The clause-2 complement pin added beside the gate (`ephClockShapeNearMisses`) names every
declaration in scope that binds a function, takes no argument and answers exactly one value whose
type is not `int64` — thirteen of them today. A hook declared `func() byte` lands in that set and
turns the gate **red**. A hook declared `func(uint8) byte` — one parameter — does not, which is the
form reproduced above. **The pin catches one spelling of this shape and the shape itself is open.**

### What a ruling would have to choose between

**Options 1 to 3 answer BINDING TIME ONLY.** That was not said when they were written, and it made
option 2 read as though it answered the whole of the argument above. **Measured: it answers neither
of the coverage plants.** A composition root's test binary re-running the *same* vectors would run
them against the same roots and the same rungs, so `if bucket == 3` and `if ephRoot[0] != 0xE0` pass
there exactly as they passed here. Option 4 is the coverage axis and it is the one this repository
has already taken a step along.

1. **Ban late binding into the derivation by construction** — a gate asserting that no declaration
   `EphKey`'s closure reads is a function-typed value writable from outside its own package. It is
   the level, but it is a rule over composition and it would bind `mls` and `connect/message` too.
2. **Move the known answers to where composition happens** — require the composition root's own
   test binary to re-run the vectors, so that whatever it installs is in the binary that checks
   them. Cheap, and it puts the check where the defect of *this row* can exist. It is **not** a
   remedy for the coverage direction, and the sentence that called it "the practical one" without
   that qualifier is the sentence this repair exists to correct.
3. **State the precondition in writing** — say in the corpus that `EphKey`'s purity is asserted
   over a binary with no injected state, and make that a review obligation rather than a gate.
   Costs nothing and is honest; catches nothing.
4. **Widen the sample, and say what remains a sample** — the coverage axis. TAKEN TWICE on
   2026-09-13 without a ruling, because neither step binds anybody or imposes a rule on any other
   package, and the second step is where the shape of the remaining question changed.

   **First step, the table.** Five points to seventeen: one root to two, and three of six rungs to
   a complement over the ladder that is asserted empty.

   **The escape that step did not cover, measured.** A clock fired only on
   `1000 < window < 1000000` — the band every 2020s–2030s sender computes in for buckets 1, 2, 3
   and 4 — was **green on every gate in this tree** at `4289bf7`, because **fifteen of the
   seventeen rows carried a window below 1000**. Widened again to twenty seven: ten production
   shaped rows, one per rung under each root, at the stated instant `1767225600000` =
   `2026-01-01T00:00:00Z`. That plant is now **8 of 27 rows red**. Bucket 5 is **not** one of them
   — its production window is 730 today and does not exceed 1000 until 2046-09-27 — and that is
   stated in the table rather than left for a reader to notice.

   **Second step, and it is NOT more rows: a differential over DRAWN inputs.** `ephpurity_test.go`
   compares `EphKey` against an HKDF-Expand written out from RFC 5869 §2.3, sharing no declaration
   with the subject and asserted to share none by an AST gate over its own body. `ephkey_test.go`'s
   objection to a second expansion is about the **formula** and is correct; it does **not** reach
   **purity**, because a clock stirred into `EphKey` moves `EphKey`'s side of the comparison and
   not the oracle's however wrong the two are together. Measured, each against a clock behind a
   `fmt.Stringer` in `mls/syntax` that the graph gate cannot see:

   | plant | KAT rows | uniform 2^64 | production shaped | runs killed |
   |---|---|---|---|---|
   | committed bytes | 0 of 27 | 0 of 600 | 0 of 600 | — |
   | fired unconditionally | 27 of 27 | 600 of 600 | 600 of 600 | 3/3 |
   | `bucket == b`, each rung | 2/5/4/5/4/7 = all 27 | — | — | — |
   | `bucket == 3` | 5 of 27 | 89..101 | 90..116 | 3/3 |
   | every root but the two pinned | **0 of 27** | **600 of 600** | **600 of 600** | 3/3 |
   | every window but the ten pinned | **0 of 27** | 600 of 600 | 499..516 | 3/3 |
   | `1000 < window < 1000000` | 8 of 27 | **0 of 600** | **375..413** | 10/10 |
   | `ephRoot[0] == 0x00` (2^-8) | 0 of 27 | 0..5 | 2..6 | 12/12 |
   | `ephRoot[0]==0 && [1]<0x10` (2^-12) | 0 of 27 | 0..2 | 0 | **6/20** |
   | `bucket == 5 && window == 17` | 0 of 27 | 0 of 600 | 0 of 600 | **0/6** |
   | `bucket == 5 && 1000 < window < 1000000` | 0 of 27 | 0 of 600 | 0 of 600 | **0/6** |
   | `613607 < window < 1000000` | 0 of 27 | 0 of 600 | 0 of 600 | **0/6** |

   **Every count of 600 is a count of a random draw, not a constant**, which is why the cells carry
   ranges and the table carries a *runs killed* column. `1000 < window < 1000000` read 375..413 over
   ten runs, mean 399.7, against `Binomial(600, 4/6) = 400`. The 2^-12 root row is the one a ruling
   should read twice: `1 - (1-2^-12)^1200 = 25%`, measured **6 of 20 runs**, so that plant **passes
   most CI runs**. Even the 600-of-600 cells are draws: the root exclusion agrees whenever a drawn
   root lands on one of the two pinned `(first, last)` octet pairs, expected `600 * 2 * 2^-16 =
   0.018` times per run, so its complement is *rare*, not *empty*; it read 600 of 600 in the three
   runs measured. A single number in any of these cells would be a seed and not a fact.

   Three things a ruling has to take from that table. The **root** exclusion, which no table of two
   roots reaches, dies 600 of 600 — the oracle is the only thing in this tree that touches it. The
   **distribution is the coverage**: the production band is invisible to a uniform `uint64` draw and
   obvious to a draw that computes windows from a wall clock instant. And **what survives is now
   published as a measurement rather than as a characterisation**, because four successive attempts
   to state it in one sentence were each too strong and each was corrected by the next measurement —
   the most recent called it one `(bucket, window)` pair of measure 2^-64, and two of the three rows
   below are about 2^21 cheaper than that and are reachable by a real sender:

   * `bucket == 5 && window == 17` — **not** production reachable; bucket 5's window 17 is
     1971-04-22.
   * `bucket == 5 && 1000 < window < 1000000` — **production reachable from 2046-09-27**, the
     instant bucket 5's window first exceeds 1000 (`1001 * 2419200 * 1000 = 2421619200000`). After
     that date, up to window 1000000 (the year 78600), every 28-day window a real sender computes
     is inside it.
   * `613607 < window < 1000000` — **production reachable from 2040-01-01**: the 386,392 hourly
     windows bucket 1 computes between 2040-01-01 and 2084-01-29. The production shaped draw
     reaches 205,585 of this band's 998,999 windows, so **79.4% of the band is outside both draws
     and outside the table**.

   Each survived everything in the tree: 0 of 27 rows, 0 of 600 on both draws over six runs each,
   the reference graph gate green, and an unfiltered `./messagegroup/` at **0 failures**. A shape
   that survives cannot be shown value-changing by a gate going red, so each was **probed**: each
   moves the derived octets at its own firing point and at no other point probed.

   **The boundary of that surviving class is NOT KNOWN TO BE TIGHT.** Those are the shapes that have
   been *tried*, not the shapes that *exist*. Nobody has characterised the set of conditions this
   ensemble misses; each of the three was found by trying one more, and the two production reachable
   ones were found only after a sentence had already called the residue a point. This corpus has not
   characterised what else is in there.

   **What a ruling still has to choose**, and the second step narrowed it rather than answering it:
   the bucket dimension is finite and complete; the root and window dimensions are 2 of 2^256 and
   10 of 2^64 as POINTS, and are now additionally covered by measure under two named distributions.
   Neither is closed. The open questions are whether the corpus wants a published seed and a
   determinism rule (this file draws from `crypto/rand` and **logs** the seed, which reproduces a
   failure without pinning the points), how many draws and at what distributions, and whether any
   of that is worth a rule at all given that **none of it touches this row** — a clock bound after
   the test binary is nil at every point of the input space, so measure over drawn inputs reaches it
   exactly as far as a table does, which is not at all.

### What is owed elsewhere

A `SPEC-LEDGER.md` number and one sentence from the owner. Options 2 and 4 are the ones that could
be done inside this repository without a rule that binds other packages; option 4's **both** steps
were taken on 2026-09-13 because neither a wider table nor a test-local differential imposes
anything on anybody, and option 2 is still not this package's to impose on a composition root it
does not own. **Nothing here is ruled, and the second step is evidence about the COVERAGE direction
and about nothing else — it is not evidence that this row is smaller than it was.**


---

## MG-4 — a member can no longer open its own application record, and no document says so

**Status: RULED AND CLOSED 2026-09-17. MASTER §8.4.7 (1), ledger item 214.** The ruling is the first
of the three options below: **a device renders its own sent lines from a copy it kept, never by
decrypting the record it wrote**, and an own record it holds no copy of is AUTHENTICATED by that
very refusal, counted, and is not a failure. The other two options are recorded as REFUSED in
§8.4.7, and the second of them matters most: exempting a record at this member's own
`sender_handle` from the inner open re-opens exactly the forgery §8.4 closed, narrowed to
self-attribution.

**IT WAS BUILT BEFORE IT WAS RULED AND THE RULING SAYS SO.** `sdk` chose this option and implemented
it — `ownIndices`/`ownSealed`, `DeviceStore.PutSentRecord`, `Device.Restore` reading it back, the
`withoutCopy` table and `Stats.SkippedOwn` / `OpenedOwn` / `OwnWithoutCopy` — while MASTER still said
the opposite. **Nothing in `sdk` has to change**, which is the point of ratifying rather than
choosing again. **Nothing in `connect` changed for it either**: this item closes by ruling, and the
behaviour it describes is unaltered.

**The measurement this pass owes and does not have:** `sdk`'s `cp3b` count is another agent's tree
and was not run here. Ledger item 214 carries the figure with its owner and names the query a later
pass owes: `go test ./cp3b/ -count=1 -json` at a named `sdk` commit.

**The historical filing follows, unedited.**

### The property

MASTER §8.4.1 makes an application record's `ct_body` plaintext `LP(inner) ‖ 0*`, where `inner` is
an MLS `PrivateMessage` produced by `Protect`. `Protect` consumes a generation of **this leaf's own
sending ratchet**, and MLS derives no *receiving* ratchet for a member's own leaf — RFC 9420 §9's
secret tree gives a member one sender ratchet per leaf, and a member never receives its own
messages. So `Unprotect` of a frame this device produced answers:

```
mls: ratchet generation already consumed: generation 0, head 1
```

`GroupSession.OpenRecord` therefore **refuses a record this same session sealed**, with
`ErrRecordInnerFrame` wrapping that sentence. Before 2026-09-15 it opened.

### The reproduction

`TestASessionCannotOpenItsOwnApplicationRecordAndThatIsMls` in `mlsframe_test.go`. One member seals
a DURABLE record with no attachment and opens it; the refusal is `ErrRecordInnerFrame`. The control
beside it is the same session sealing a **commit** record, which carries no inner frame and opens
exactly as it always did — so the refusal is the frame's and not the record layer's.

### What the specification says, and it is wrong about this

Spec A §5.2's A-27 paragraph reads *"The change adds a second refusal to a case that already
refused; it does not make a working call stop working."* The measurement behind it was a **second**
`OpenRecord` of one record, which already refused at the record layer's skipped-key window. **A
first `OpenRecord` of one's own record was a working call and it has stopped working.** The quoted
`mls` error in that paragraph — `generation 0, head 1` — is the error a **first** self-`Unprotect`
answers, so the two cases were conflated.

### What a ruling has to choose

1. **Nothing, and say so.** A sender renders its own message from the copy it kept and never by
   decrypting the record it wrote; every real MLS application works this way. Then Spec A §5.2's
   sentence is corrected and §7 says a client's own sent messages come from the local store.
2. **Give the sealer back its own plaintext.** `SealRecord` would answer what it framed beside the
   record, which is a change to a signature Spec A §5.2 publishes in a Go block.
3. **Exempt a record whose `sender_handle` is this member's own from the inner open.** This is the
   cheap one and it is the dangerous one: it re-opens exactly the forgery MASTER §8.4 closed,
   narrowed to self-attribution — any member could seal a record attributed to Alice and **Alice's
   own device** would render it. This row exists partly so that option is written down as refused
   rather than rediscovered.

### What is owed elsewhere

A `SPEC-LEDGER.md` number, a correction to Spec A §5.2's sentence, and one sentence from the owner
about where a device's own sent messages are read from. Nothing here is decided.

---

## MG-5 — the ceremony arm of MASTER §8.4.1 is authenticated by nothing, and the sealer picks the arm

**Status: OPEN, FILED NOT RULED. Measured on 2026-09-15 in the second pass over MASTER §8.4. Half
of the finding is repaired in that commit; the half that needs a ruling is here.**

### The property

`isApplicationRecord(is_commit, server_attachment)` is MASTER §8.4.1's table, and it decides whether
a record's `ct_body` carries an inner MLS frame. Both of its inputs are header fields, both live in
`AAD_head`, and `AAD_head` is sealed under `record_key[n]` — which
`RecordKeyZero(class_key, leaf_index)` derives from a class key **every member holds** and a leaf
**number**. So the member that seals a record chooses which row of the table its record takes, and
therefore chooses whether §8.4.3's two refusals apply to it at all.

### The reproduction

`TestTheArmOfTheTableIsChosenBySomethingNoSignatureCovers` in `mlsframe_test.go`. A full member seals
a record at **another member's** `sender_handle`, with `is_commit = 1` and a body of its own
choosing and no signature anywhere, and again with a server attachment set. Before the repair both
opened through `OpenRecord`, answering the attacker's octets attributed to the victim.

### What the repair does, and what it does not

`OpenRecord` now serves **only** the arm that carries a frame and refuses the other with
`ErrRecordNotAnApplicationRecord`; the ceremony arm has its own door, `OpenCeremonyRecord`, whose
name and prose say that nothing it returns is signed by any member. So a member's choice of arm is
now a choice between *being checked* and *being refused at the message door* — which is what a rule
is — and no call named for opening a message can be made to answer unsigned octets.

**It does not authenticate the ceremony arm, and it cannot.** A wrap, an epoch fan out and a
completion marker carry no signature at all (Spec A §5.11 step 5). A commit record's body *is* an
`MLSMessage` and **is** signed, but the thing that authenticates it is processing the commit, which
belongs to the epoch machinery and not to a record door — and a check taken here on the frame's
*peeked* sender leaf would be worse than none, because the sender data a peek reads is sealed under
a group-shared secret and would read as authentication while authenticating nothing.

### Ruling, 2026-09-17: MASTER §8.4.7 (2), RULED IN PART

**The ceremony arm carries no signature by design and full adoption does not change that.** §8.4.1
row 3 — a wrap, an epoch fan-out, a completion marker — carries no MLS frame at all, every key it
uses is group-shared, and neither a wrap (HPKE addressed to a device) nor an epoch marker (a
counter) is a thing a `PrivateMessage` could carry without changing what it is. So the ruling is
what the shape already forces, in three parts:

- **(a) A ceremony record's `sender_handle` is a ROUTING LABEL and MUST NOT be rendered, attributed,
  or used as evidence that a particular member wrote anything.** This is the sentence that was
  missing and it is the whole of what this item was about. `OpenCeremonyRecord`'s own header already
  says the octets are nobody's; the ruling makes that normative rather than a caveat.
- **(b) A door named for opening a message MUST refuse the ceremony arm** — built, and it is
  `OpenRecord`'s `ErrRecordNotAnApplicationRecord`.
- **(c) An acceptance on the ceremony arm MUST spend nothing** — built, and it is
  `TestTheCeremonyDoorSpendsNoneOfTheHandleItNames`.

**WHAT IS NOT RULED AND IS NOT THIS DOOR'S:** the COMMIT row. A commit record's body is an
`MLSMessage` and is signed, and what authenticates it is PROCESSING the commit — the epoch
machinery's job, which MUST require the commit to be one MLS accepts **and** to have been signed by
the leaf the record's `sender_handle` names. **That machinery is NOT BUILT.** A check taken at the
record door on the frame's *peeked* sender leaf would be WORSE than none, because that leaf comes
out of sender data sealed under a group-shared secret and would read as authentication while
authenticating nothing.

**Status: RULED IN PART. (a) is ruled and is the part that was missing; the commit row's
authentication is owed by the epoch machinery and is not built.**

### Correction, 2026-09-15: this item had only the ATTRIBUTION half, and the DENIAL half was open

Everything above measures the residual as *"attacker-chosen octets under a victim's
`sender_handle`"*, which is attribution. There is a second half and it is a **denial**, which is the
class this arm shares with the record layer's other two channels.

`openRecordOnLoop` ran `receivers.Commit(ratchetKey, header.StreamIndex)` for **both** doors, and the
ceremony arm takes **no frame check at all** — `unframeBodyOnLoop` returns a ceremony body unchanged.
Every key the two record AEADs use is group-shared, so a member can seal a ceremony record at any
other member's `sender_handle` at any index. **Accepted**, that record committed the victim's ladder
past the rung the victim's own next record needs: one squatted record per message, no lift, no
genuine frame, no race against a record that already exists, and the victim's record at that index
answering `ErrOutOfWindow` forever. The gate beside it is named *"a refused record moves no receiver
ratchet"* and it is true; this was an **accepted** record moving one, on a body nobody signed.

**Repaired in the same commit as MG-6's filing.** `openRecordOnLoop` commits only on the application
arm. The rule is the one the arm split already states, carried through to the ladder: a **refusal**
taken on an attacker's claim costs the attacker, and an **acceptance** taken on one costs the victim
— so a door that authenticates nothing may not spend anything either.
`TestTheCeremonyDoorSpendsNoneOfTheHandleItNames` measures it, and
`TestTheApplicationDoorStillSpendsTheRungItOpens` is the control that stops it being satisfied by a
ladder that never moves.

**What that costs, and it is a behaviour change with a bound.** A ceremony record no longer advances
the head of the ladder it is on, so a sender's ceremony records sit as skipped rungs until an
*application* record from that sender walks past them — retained, openable, and pruned by the same
window as any other gap. A sender that puts more than `DefaultRecordWindowSize` (1024) ceremony
records on one ladder between two application records puts the later one outside the window, and it
is refused with `ErrOutOfWindow`. A ceremony record can also now be delivered twice and open twice;
the ladder was never an authentication on this arm, and what judges these octets is what this item
already says judges them — a wrap by whether it decrypts to this device, a commit by whether `mls`
accepts it, and `mls` carries a replay guard of its own. **Nothing calls `OpenCeremonyRecord`
today**, so no shipping path changes; the consumer that will is the epoch machinery, and choice 2
below still owes it an answer for the *attribution* half.

### What a ruling has to choose

1. **Nothing beyond the split.** The ceremony arm stays unauthenticated, `OpenCeremonyRecord` stays
   the only door onto it, and §8 says in as many words that a ceremony record's `sender_handle` is
   routing and not attribution.
2. **Authenticate the commit arm where the commit is processed.** The epoch machinery requires the
   commit inside a `is_commit = 1` record to be one `mls` accepts *and* to have been signed by the
   leaf the record's `sender_handle` names, and a record failing that is dropped rather than
   ceremonially applied. This is the only one of the three rows that admits an authentication at
   all.
3. **Move `is_commit` and `H(server_attachment)` under something a member signs.** That is a wire
   change — `AAD_head` is outside the frame because `body_hash = H(ct_body)` is inside it, which is
   MASTER §8's construction order — so it is a §8 ruling and not this package's to take. Ledger open
   item 199 is the same five fields from the other side.

### What is owed elsewhere

A `SPEC-LEDGER.md` number, and one sentence in Spec A §5.11 about what a ceremony record's
`sender_handle` means. The downstream filter this repair makes redundant —
`sdk/urmessage/group.go`'s `SkippedCeremony` arm — should stay: it is now belt and braces rather
than the only thing standing between a forged arm and a rendered message, which is what it was.


## MG-6 — the head plaintext is not bound by the inner frame, and one member can re-issue another's body under a head of its own

**Status: RULED AND CLOSED 2026-09-17. MASTER §8.4.2 v2 term (4), ledger items 213 and 204.** The
bind is
`head_commit = HMAC-SHA-256(HKDF-Expand(record_key[i], "rec/v1/head-bind", 32), head_plain)`,
carried **inside** the `aad_mls` SHA-256 preimage so `aad_mls` stays 32 octets and **zero wire
octets move**. It is KEYED and not a bare hash because `AAD_body` is public and `aad_mls` travels in
the clear as the frame's `authenticated_data`, so an unkeyed commitment to a nine-octet head whose
only variable is a millisecond timestamp would hand the **server** a confirmable `sent_at`.

**THE CASE THAT FILED IT IS NOW ITS INVERSE.** `TestTheHeadPlaintextIsNotBoundByTheFrame` required
the substitute to OPEN and said in its own header that it would go red the day somebody bound the
head. It is now
`TestASubstitutedHeadIsRefusedAndTheGenuineRecordStillOpens`: the substitute is refused with
`ErrRecordPositionBinding`, and the genuine record at the same index **still opens**, which is the
half that says the refusal spent nothing. A control in the same case re-issues the lifted frame
under its OWN head at its OWN index and requires it to open, so the refusal is of the head and not
of the lift.

**WHAT THE aad_mls COMPLEMENT LOSES.** `mlsframe.go` printed SIX things `aad_mls` cannot defend and
the sixth was the head plaintext. It is now FIVE — `is_commit`, `size_bucket`, `expire_at`,
`blob_id`, `H(server_attachment)` — which is ledger item 199 and is **not** closed. The head was the
only entry on that list with a live consumer.

**THE DELETION §8.4.3 OWED, RUN AND REPORTED.** §8.4.3 predicts that the GENERATION half of the
second (post-open) reading in `unframeBodyOnLoop` defends nothing, because a frame that opens at all
opened at the generation the pre-reading read. **Measured: with the second reading's generation
replaced by the peek's (`position, err := aadMls(binding, peekGeneration, head)` and the open's
generation dropped), `go test ./messagegroup/ -count=1 -timeout 900s` was `ok` — NOTHING went red.**
`./mls/` is unaffected by construction: it does not import `messagegroup` and the edit is confined to
`messagegroup/mlsframe.go`. That is the predicted answer and it is reported rather than hidden.

The clause is kept, because its premise is a property of `mls`'s implementation rather than of any
document — and `TestTheReadingThatDecidesIsTheOneTheSignatureCovers` now carries a `generation` drift
row that DOES separate it. What that row drives is an ENGINE whose peek and open disagree, not any
octets: over the real engine the two cannot, which is exactly why the deletion above turned nothing
red. Both measurements are true and they are about different things.

**AND ONE PREDICTION OF §8.4.3 IS FALSE ON THIS BUILD, reported rather than left standing.** Its
mutation (f) — replace `HMAC-SHA-256(head_bind_key, ·)` with `SHA-256(·)` — is predicted to change
no refusal and turn NOTHING red, on the argument that the clause defends a confidentiality property
no refusal can see. **Measured: three named cases go red.**
`TestAadMlsIsMasterSection842sDigest` (its transcription of the preimage, and the assertion that the
same head under two different rungs commits differently), `TestTheReproductionRecomputesHeadCommit-
FromItsOwnLadder` (the CP3b reproduction rebuilds `aad_mls` from its own ladder and disagrees with
the frame the sealer emitted), and `TestTheLadderLabelsAreSeparateConstantsAndNoneIsBuiltFromAnother`
(the head-bind label stops reaching the kdf at all). The clause is held by cases here and not only by
a stated rule.

**The historical filing follows, unedited.**

### The property

`aad_mls` is MASTER §8.4.2's `H("URmessage/v1/aad/mls" ‖ AAD_body)`, and `AAD_body` carries the six
fields that fix a record's identity and its position. `ct_head` is sealed under the same
`record_key[n]` **every member derives**, and **no field of the inner frame covers the head
plaintext**.

So a member can take another member's **genuine** body — frame, signature and all — and re-issue it
at the **same position** under a head of its own writing. R1 passes because the frame really is that
member's; R2 passes because the position really is that record's. Both readings agree, and the
record opens to the true sender's plaintext under an attacker's head.

### Why the head is not decoration

`sdk/urmessage/group.go` does `sentAtMs, err := decodeHead(headPlain)` — the head carries the message
timestamp. A head another member wrote is a timestamp another member wrote; a head `decodeHead`
*rejects* is a `fail()`, three attempts, then `ErrRecordAbandoned` and a permanent hole.

And the substitute is **accepted**, so it spends the rung: the true sender's own record at that index
answers `ErrOutOfWindow` afterwards. That is the same denial the two channels repaired beside this
one produce, reached through an acceptance that is legitimate at every check the record layer has.

### The reproduction

`TestTheHeadPlaintextIsNotBoundByTheFrame` in `mlsframe_test.go`. It asserts the substitute **opens**
and that the genuine record is then out of window, and it says in its own failure message that a
build which refused the substitute has closed this item and should delete the case.

### What is NOT established

**Reachability is a log-ordering property outside `connect`.** The attacker must first receive the
victim's record to lift the frame, so the genuine record is already ahead of the substitute in the
log, and whichever an opener processes first wins. That is a property of the server and of the walk,
and this item does not claim it.

### What a ruling has to choose

1. **Bind it, keyed.** The order is *not* circular the way `AAD_head`'s is: the sealer holds
   `headPlain` before it frames the body (`sealRecordOnLoop` takes it as an argument and
   `newRecordBuilderOnLoop` runs `frameBodyOnLoop` after `ratchet.Next`), and the opener holds it
   before it unframes one (`ct_head` is opened above `unframeBodyOnLoop`). What it must **not** be is
   a bare `H(headPlain)`: `AAD_body` is public and `aad_mls` travels in the clear as the frame's
   `authenticated_data`, so `H(public ‖ timestamp)` hands the **server** a guessable commitment to
   `sent_at_ms`. It has to be keyed under `record_key`, which both sides hold and no non-member does.
   That is a change to §8.4.2's construction and therefore a §8 ruling.
2. **Leave it and say so in §8.** The head becomes explicitly *group-authenticated and not
   sender-authenticated*, and `sdk` is told that `sent_at_ms` is a claim by the group rather than by
   the sender — which is a statement a product has to be able to live with, since a reply's parent
   timestamp and a conversation's ordering are read off it.
3. **Move the head inside `ct_body`.** No wire field is added and nothing is keyed, but the size
   ladder moves again and §8.4.4's measured columns are all re-taken.

### What is owed elsewhere

A `SPEC-LEDGER.md` number. Ledger open item 199 is the same question about the other five `AAD_head`
fields, and this is the sixth — the one with a live consumer. `mlsframe.go`'s
*"WHAT IT CANNOT DEFEND"* paragraph now prints six and not five.

---


## MG-7 — three of `wrap_key`'s nine inputs have no code point, and the wrap envelope's version octet is authenticated by nothing

**Status: OPEN. Both halves are wire decisions and neither is this package's to take.**

### The property

MASTER §7's wrap KDF is normative in shape and under-determined in three of its inputs, and MASTER
says so itself:

```
info = "URmessage/v1/wrap" ‖ LP(group_id) ‖ u64(epoch) ‖ u8(target_type) ‖ LP(target_id)
       ‖ u8(payload_type) ‖ u16(alg_id) ‖ LP(target_xwing_pub) ‖ LP(ct_xwing)
```

- **`target_id`** is *"defined nowhere in the corpus"*. Spec A §5.11 (5) lists four candidates that
  are four different byte strings — `recovery_handle`, the epoch-scoped `wrap_target_handle`, a leaf
  index, a member id.
- **`u8(target_type)` and `u8(payload_type)`** arrived with red-team **M-15** on 2026-09-18 and have
  no code point anywhere. MASTER's clarification of 2026-09-19 is the sentence that matters: the
  `info` table fixes the **domain** — an active device leaf and a member's `RECOVERY_PUB`, a
  `pq_secret` payload and an `eph_root` payload — and **"the domain is not the encoding"**.

MASTER's own conclusion is the property: *"the block is normative modulo those three"*, and *"a
second implementation cannot build a wrap from it alone"*. The failure mode it names is the one that
has no symptom: **a publisher and a restorer that choose differently produce a wrap nobody can open,
with no error anywhere.**

**The second half is a consequence of shipping the door before the signature.** The wrap envelope is
eleven octets of cleartext outside `hybrid_ct` — `u8(wrap_format_version) ‖ u8(target_type) ‖
u8(payload_type) ‖ u64(content_epoch)` — and the `info` above binds ten of them. It does **not**
bind `u8(wrap_format_version)`. The only construction in the corpus that ever covers that octet is
the body signature, whose preimage MASTER §7 extends by `LP(wrap_envelope)` for exactly this reason
— *"without it the signature reaches the record header, the KEM transcript and the secret, and
reaches not one octet of the envelope"* — and that signature is m1 Task 14 **step 3**, blocked by
open item **M1-52** (the preimage is 1,320 or 1,356 octets and no document says which). So at this
commit the version octet is authenticated by nothing at all.

### The reproduction

`TestTheEnvelopeOctetsTheWrapKeyBindsAreRefusedAndTheSuiteReportsTheRest` in `wrap_test.go`. It
edits each of the envelope's eleven octets on the wire and reports which are refused, **by which of
the door's two authorities**:

```
wrap envelope, 11 octets: the AEAD refuses [1 2 3 4 5 6 7 8 9 10] and accepts [0] when the
opener's authority moves with the edit; the opener's own comparison refuses [1 2 3 4 5 6 7 8 9 10]
when it does not; [0] is covered by neither
```

The first column is the key: the three values the opener states are moved to agree with the edit, so
the comparison passes and only the tag can refuse — which is the hardest case for the key and
therefore the one worth measuring. The second is the opener: its authority stays where the sealer put
it. Each set is asserted against a written-down disposition and fails in **both** directions — for
the AEAD's, a larger accepted set is an unauthenticated octet nobody named and a smaller one means an
authority arrived, which is what Task 14 step 3 landing looks like and which must delete the clause
rather than pass quietly; for the comparison's, a smaller set is one of the opener's three stated
values that stopped being compared and a larger one is **M1-54** being ruled in this package. The
union's complement — the octets **neither** authority covers — is asserted to be exactly `{0}`.
`testdata/envelope-wrap-kat.txt` §4 carries the AEAD column as a vector a second implementation can
check without a KEM: one row per octet, giving `H(info)` after flipping it, with the verdict stated
per row. §5 carries the other direction — two **genuine** envelopes, one epoch moved and one payload
kind moved, each with its own `H(info)`, which differs from the base: a different key and not a
broken one, so nothing inside the seal can refuse it.

For the three code points there is nothing to reproduce, which is the point: `wrap.go` takes all
three as parameters with no default, so this package cannot be the one that chose.

### What is NOT established

That the version octet being unauthenticated is **exploitable today**. Nothing in this tree consumes
`wrap_format_version` — `OpenWrapBody` carries it out to its caller and refuses nothing on it — so an
edit to it changes no behaviour at all. What it costs is the future: the octet exists so that a
second body field is a negotiation rather than a flag day (MASTER §7), and a version field an
attacker can move is a downgrade channel the moment anything branches on it.

**And that the AEAD's ten "refused" octets say anything at all about a wrap nobody edited.** They do
not, and the measurement above is careful to say which case it is in. Under M1-55's first reading —
this door's — `wrap_key` is derived from the envelope the body carries, so a **genuine** wrap of
another epoch or another payload kind is self-consistent: its key matches its own envelope and the
AEAD opens it. Measured on this door before the opener stated anything: a genuine wrap sealed at
content epoch 10 opened at a signature with no epoch in it and returned its payload byte for byte,
and `SealDeviceWraps`' two bodies for one leaf at one epoch — which land at **one**
`wrap_target_handle` and differ in `u8(payload_type)` alone — both opened under identical arguments,
with the octet carried out to a caller under no obligation to read it. So MASTER §7's *"a receiver
that derives the key from the envelope's own values and finds `aead_ct` does not open has detected
the disagreement fail-closed"* is a sentence about a **disagreement** and covers the tampered case
only. Task 14 Property 4's headline — *a wrap for epoch n+1 does not open as a wrap for epoch n* — is
about the genuine one, and in this package it is true because `OpenWrapBody` takes the opener's own
`contentEpoch`, `targetType` and `payloadType` and refuses anything else with
`ErrWrapEnvelopeMismatch`, ahead of the KEM.

**Those three were one `WrapExpectation` argument for one commit, and that shape was a hole this
item did not record.** A struct's zero value is a complete value of it, so `WrapExpectation{}`
compiled, stated nothing, and opened a **genuine** wrap whose envelope was
`{target_type 0x00, payload_type 0x00, content epoch 0}` — measured, `err=<nil>`, payload byte for
byte — while `WrapExpectation{ContentEpoch: k}` named the epoch and silently expected `0x00` for the
two octets this item itself says have no code point. A caller could consume the door while declining
the obligation the commit claimed it could not decline. The repair is three parameters, which is the
shape `SealDeviceWraps` already has for the same three under-determined values: Go supplies no
argument a caller did not write, so each of the three is on the page at every call site. **That is
the whole of what the shape buys.** `0x00` and epoch 0 stay perfectly reachable expectations — 0 is
the founding epoch and neither type octet has a code point to be unlike — and what is gone is
stating them by omission. It is held structurally by
`TestNoValueTheOpenerMustStateArrivesThroughAZeroValuableAggregate`, whose two positive controls are
the sealing doors' own `WrapEnvelope` parameters, and behaviourally by
`TestTheOpenerStatesTheThreeUnderDeterminedValuesRatherThanDefaultingThem`. **The structural gate
reads the type expression and not the rendered type string**, because a mutant that reintroduced the
rejected design as `want ...WrapExpectation` — a variadic every existing call site compiles against
unchanged, and one a caller can leave off altogether — survived the first draft of it, measured.
The door's three values arrive in `WrapInfo`'s own write order, which keeps `u8(target_type)` and
`u8(payload_type)` from being two adjacent `uint8` parameters the compiler cannot tell apart; that
order is held against the encoder by
`TestTheOpenersParametersArriveInTheOrderTheWrapKeyInfoWritesThem` rather than written down twice.

**The sealer's `WrapEnvelope` argument is deliberately not repaired with it, and the reason is a
document.** m1 Task 14 Property 9 requires in as many words that *"the sealing side is reachable
with an envelope the caller chooses"*, and its assertion 1 needs a consistent control for each of
the **eleven** envelope octets — the version octet included, which an opener may state no
expectation over. The sealer's argument is the wire record being **written** rather than an
authority being stated, so it has no "unset" reading to hide one: whatever it is handed appears on
the wire and the opener sees it. A caller passing `WrapEnvelope{}` to `SealWrapBody` seals a
version-`0x00` wrap, and nothing in this tree refuses one, which is this item's first half and not a
second defect.

**What is still not established is where the three values came from.** The door compares what it was
handed against what the body carries; it cannot tell an expectation read off the caller's own
authority — the epoch it is restoring, the record kind it asked for — from one copied out of the body
in front of it. There is deliberately no exported helper that reads the three off a `WrapEnvelope`,
so copying is something a caller writes out on purpose, and nothing here stops it.
The consumer that will be judged on it is Task 15's fan-out, which is what **honours** `pq_secret[k]`
and `eph_root[k]` in MASTER §5.3's sense. Taking the comparison at all is the third behaviour **M1-55**
names and leaves to the implementation — *"may or may not compare the carried values against its own
and refuse a mismatch"* — so it rules nothing: the wire does not move, and the ruling M1-55 owes is
owed exactly as before.

### What a ruling has to choose

1. **The three code points**, as a table: which octet a device leaf is and which a member's
   `RECOVERY_PUB` is; which a `pq_secret` payload is and which an `eph_root` payload is; and which of
   Spec A §5.11 (5)'s four byte strings `target_id` is. It is one paragraph in MASTER §7 and it
   unblocks a second implementation.
2. **`M1-54`** — what an opener does with an unrecognised `wrap_format_version`, and at what point
   relative to the AEAD open. MASTER §7's own rationale for putting the octet first points towards a
   refusal and an early one; nothing states it. A refusal empties the measurement's third set — the
   octets **neither** authority covers — and closes this item's second half without waiting for the
   signature. It is the one ruling this package could act on the same day: a fourth value would join
   `OpenWrapBody`'s parameters, which is why the complement of `WrapEnvelope`'s fields against that
   signature is asserted rather than assumed.
3. **`M1-52`** — the signature preimage's length, which is what unblocks Task 14 step 3 and is the
   only thing that puts an authenticator over the envelope as a whole.

### What is owed elsewhere

A `SPEC-LEDGER.md` number. Ledger item **251** ruled the carrier for item 243 as Task 14's X-Wing
device wrap and did not reach any of the three code points; ruling 36's own sentence — *a
post-quantum gate cannot be discharged by a carrier whose confidentiality rests on the MLS exporter*
— is what makes this door load-bearing rather than optional, so the encodings it needs are now on
the critical path of the removal track rather than of a later slice.

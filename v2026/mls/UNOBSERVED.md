# Properties this package states and nothing measures

What this file is for: a property that is **correct today and observed by nothing** is a property the
next commit can delete for free. That is not hypothetical here — five rounds running, a disclosure in
this package described behaviour no test held, and each was found by a reviewer rather than by the
suite.

Each entry below is a property somebody has already **measured as unobserved**: the mutation is
written out, and it is a mutation that leaves the whole of `./mls/...` and `./message/...` passing.
An entry is a debt and not a plan. Do not treat "it is written here" as coverage.

**A note on the counts.** Every count written here before 2026-09-05 was taken against a tree
carrying four tests this commit removed -- the three of `quantitative_claims_test.go` and
`TestAPerTypeViewIsLinearInTheCommitOrder`. A baseline written below as 7,489 passing is 7,485
today, and "7,488 passing with one skip" is 7,485 with none, the skip having been the timing case.
The mutations themselves are unchanged.

Two rules for anything added here.

- **Give the mutation, not the worry.** An entry that says "this looks under-tested" is worth
  nothing. An entry that says "rewrite line X as Y and the suite still passes" can be checked in one
  command, and closed by writing the case that fails under it.
- **Close entries by deleting them.** When a case is written, take the entry out in the same commit
  and name the case where the property is stated. An entry that outlives the gap is the same defect
  this file exists for, pointed at the file itself.

---

## The exhausted end of the sender-ratchet round trip

`SenderRatchets` and `RestoreSenderRatchets` carry a ratchet's position as one count,
`(*ratchet).consumed`, which runs one past the counter so that a ratchet parked on `2^32-1` with
`exhausted` set reports `2^32`. That encoding exists so a restore cannot be handed the pair
`(head 5, exhausted true)`, which describes nothing this type can reach.

**Nothing in either package ever persists or restores an EXHAUSTED ratchet**, so the whole exhausted
arm of the round trip is unobserved. Three separate mutations, each of which restores a member into a
generation it has already sent under, all survive a full `./mls/... ./message/...` run — measured on
2026-09-05 at 7,489 passing, 0 failing, against a baseline of the same:

- `Consumed: r.consumed()` in `SenderRatchets` narrowed to `Consumed: uint64(r.head)`
  (7,488 pass, 0 fail, 1 skip — the skip is the timing case and not this);
- `generationsConsumed`'s `count == 1<<32` arm answering `exhausted = false` (7,489 pass, 0 fail);
- `generationsConsumed`'s `count < 1<<32` widened to `count <= 1<<32` (7,489 pass, 0 fail).

What would close it: a fixture that drives a ratchet to `2^32-1` cannot be built by stepping — it is
four billion derivations — so the case has to reach `(*ratchet)` directly, set `head` and `exhausted`
by hand, and drive `SenderRatchets` → `RestoreSenderRatchets` over that. The property to state is
that a restored exhausted ratchet refuses the next send rather than handing out generation 0 again.

## `RestoreSenderRatchets`' erased guard

`secret_tree.go`'s `RestoreSenderRatchets` opens with `if self.erased { return ErrEpochErased }`.
Deleting it leaves the suite passing — measured on 2026-09-05 over the whole of `./mls/...` and
`./message/...` at 7,489 passing, 0 failing, identical to the baseline. The per-entry
all-zero-octets check below it refuses an erased epoch's own persisted vector, and `ratchetFor`
refuses on the same field further down. The guard is benign redundancy today, and it is the frame
that reports the EPOCH — rather than the entry — as the reason, which is a different sentence for a
caller. Nothing holds it.

## What the retained bounds do to each other at realistic occupancy

`TestOneRatchetsWindowNeverExceedsItsOwnBound` and the two cases that read
`stFillPastTheRetainedBound` all run over a tree where **one ratchet holds everything**.
`MaxRetainedWindowKeys` is declared as `RatchetWindowSize` itself, so with a single occupied ratchet
the tree-wide bound and the per-ratchet bound evict down to the same total and no case here can tell
them apart.

What is unmeasured is what the retention is worth at a realistic occupancy — several members each
holding a few skipped generations, one of them falling behind — where the two bounds do come apart
and the tree-wide one trims a walk the member that made it has not used yet. How much it trims is a
number nothing here computes. The eviction rule that would decide it (`the fullest window pays`) is
stated in `MaxRetainedWindowKeys`' own comment and exercised only in the degenerate case.

> **Corrected 2026-09-15.** This entry named the catch-up, and the catch-up is gone: a generation
> past `MaxGenerationSkip` is refused without walking anything. What it named was an **occupancy**
> question and not a censorship one, and that distinction is exactly what this entry got wrong for
> two rounds — while it stood, a member could push another member's head forward by 1,024 per
> header, and the only thing written down about that walk was how much memory it used. The question
> above survives the removal because *accepted* walks still retain; the sentence that did not is
> the one that treated a bound on the WORK as if it were a bound on the DAMAGE. See
> `(*ratchet).classify` and `MaxGenerationSkip`.

## The two erases on the two-phase receive path that no case in this tree can see

Both were **measured by deleting them and running the whole of `./mls/`, `./message/` and
`./messagegroup/` at `-count=1 -timeout 1800s`**, not reasoned about. Both stay, and they are named
here rather than left for somebody to find, because a clause that cannot be turned red is a clause
the suite does not hold whatever it looks like.

### The peek's scratch chain

`(*ratchet).peekFor` walks a COPY of the ratchet secret and calls `zeroizeSecret(walking)` on each
rung as it passes it and once more on the last one. Delete both calls and **nothing goes red** —
all three package trees stay green. Nothing in the process can reach those slices once
the call returns, so no runtime assertion can distinguish a walk that erased from one that dropped;
what the erasure buys is that the run of chain secrets between the head and a generation an
UNAUTHENTICATED party named does not sit in the heap afterwards, which is a forward-secrecy property
about the process and not about the type. The gate over that class is
`TestNoDeclarationReachingTheSecretTreeStoragePutsItBeyondTheCall`, which reads the SOURCE.

### The commit's own erase of the generation it spent

`(*SecretTree).CommitMessageKey` zeroizes the key and nonce `commitFor` hands back before it
returns. That pair is the generation's last live copy inside the tree — a retained entry is deleted
from the window as it is handed over, and a walked one never enters it — so dropping it instead of
erasing it leaves live AEAD key bytes wherever the allocator puts them next.

**Nothing in this package sees the difference, measured.** With both `zeroizeSecret` calls deleted,
`./mls/`, `./message/` and `./messagegroup/` stay green. The storage is unreachable from the tree by
construction the moment the call returns, so no test can read it back; what it costs is what every
erase in this file costs — the process, not the type — and
`TestNoDeclarationReachingTheSecretTreeStoragePutsItBeyondTheCall` is the gate over that class in the
SOURCE rather than at runtime. The lines stay for the reason `eraseKey`'s do: the erasure is the
half a caller omits silently.

## The arithmetic this source states in prose, and why nothing gates it

This entry is not a property waiting for a case. It is the ground already covered, written down so
that an eighth attempt starts from the seventh rather than from the first.

**The class.** A production comment works a quantity out over a constant this source names, and the
test standing beside it evaluates that quantity at ONE POINT. The formula in English then drifts
from the formula in go, silently, because no assertion reads English. Seven of these have shipped
in this package and been found afterwards. Among them: the catch-up's `ceil(n/MaxGenerationSkip)`,
wrong at seven of ten distances; the retransmission sentence that holds only inside
`2*MaxGenerationSkip`; the `ReceiverKey` bound that called the leaf index "the only thing here that
is" and named a second bound in the next breath; two files stating one composition with 1050045 and
1050064 octets in it; and the `handedBack` header, three headers written over one field.

**Seven attempts at a gate, and the tally.** Every one of them was DERIVED in its class and
ENUMERATED in its scope -- Rule 5 one level up -- and that is the failure that recurred. A formula
with no binary operator adjacent to a constant is outside the class. A claim in a paragraph that
already cites a test is outside the scope. A plan property written as a heading rather than a bold
run is outside the parser. Across those seven rounds the static gates caught none of the seven
instances, and an independent reviewer -- one who REINTRODUCES the defect and measures what the
suite says -- caught all seven.

**So the working conclusion, as a conclusion and not as a rule: in this package the instrument that
has actually caught this class is a reviewer who reintroduces the defect and measures, and nothing
static has.** Seven rounds is evidence about this package and this class. It is not a law about
gates, and it is not a reason to stop writing the ones that do hold.

**The last attempt, measured with the gate still in the tree.** `quantitative_claims_test.go`
(84e7a04, 671 lines) required every maximal span of a production comment that parses as a go
expression, carries an arithmetic operator and names a constant this source declares to name a
test, and required that test to reach the same constant through arithmetic of its own. Its three
motivating defects were restored one at a time on 2026-09-05, with the gate present:

- the pre-commit retransmission bullet, back into `(*Group).LoadGroup` -- gate **PASS**;
- the self-contradicting `ReceiverKey` bound, back into `secret_tree.go` -- gate **PASS**;
- the old `handedBack` header, back into `seal_persist_test.go` -- gate **PASS**.

Zero of three. The third was never a miss and should not be counted as one: the gate's own header
excluded test-file comments by construction, so it could not have seen that header however well it
worked. Two of three is the honest reading, and two of three is still every live instance the gate
was written for. On the tree with the gate deleted, each of the three restored sentences leaves the
whole of `./mls/...` and `./message/...` at 7,485 passing, 0 failing, 0 skipping, so nothing else in
either package sees them either.

**And two measurements say the shape is not tunable.**

*Discharge is not a pairing.* It is "name any test whose call closure touches arithmetic over the
constant", walked by function NAME at unbounded depth. Measured over the 2,023 test names of both
roots: `MaxLeafCount` is discharged by **864 of them (43%)** and `MaxGenerationSkip` by **111**. A
citation therefore binds a paragraph to a population, not to a test. Demonstrated: replace both test
names the `LoadGroup` disclosure cites with `TestARemovedMemberLeavesTheRestInLockstep` -- a case
about what a removal does to the other members, which computes nothing about the catch-up -- and the
gate stays green.

> One correction to the record this round was handed. The same swap made to
> `TestPastEpochWindowDropsOlderState` does **not** leave the gate green: that test reaches
> `MaxLeafCount` and not `MaxGenerationSkip`, and the gate reports all five of that group's
> `MaxGenerationSkip` claims. Swapping only ONE of the two citations to it does leave the gate
> green, and that is the paragraph effect below rather than this one. The conclusion is unchanged
> and the example had to be.

*It guards paragraphs, not sentences.* Measured: **17 claims in 9 comment groups** over 1,690
production comments, and **6 of the 17 are in one group**, `group.go:2765`. A citation anywhere in a
group discharges every claim in it, so a false formula pasted into an already-citing paragraph
passes -- which the gate's own header recorded after the first attempt to mutate it survived.

**What an eighth gate would have to answer.** Both of the above, and neither is cosmetic: a discharge
that binds ONE test to ONE claim rather than a name to a population reachable at unbounded depth, and
a class that does not depend on where a sentence's punctuation falls. A mechanism that answers those
two is new information and worth trying. A gate that does not is the seventh again, and it is worse
than nothing: its header names a class, and a reader who sees the name believes the class is held.

**What the deletion did not cost.** The gate's other direction -- every test a production comment
names must exist -- is still held for this package by
`TestTheMembershipTagCommentaryNamesGatesThatExistAndAClassThatHoldsItsSpellings`, which checks 239
cited names. Measured: rewrite `secret_tree.go`'s citation of
`TestOneRatchetsWindowHoldsOneOverItsBoundBetweenAPeekAndTheEraseAfterIt` as a name nothing declares
and that case fails, at 7,484 passing and one failing. The residual gap is the OTHER root, and it is
real: the same rewrite of `message/attachment.go`'s citation of `TestNoStubShapesRemainInSource`
leaves the whole of `./mls/...` and `./message/...` at 7,485 passing. A dangling citation in
`connect/message`'s production prose is unobserved. Widening that gate's paths to
`forbiddenScanRoots` is what would close it.

## A per-type view's order of growth

`(*ProposalList)` derives its four per-type views at every read rather than indexing them once, and
`viewOf`'s own comment says what makes that affordable rather than merely cheap in one fixture: the
accessor is linear in the commit order, over a list bounded only by what a peer may send. Nothing
measures it.

Mutation: rewrite `viewOf` as a sweep of the whole order per entry, accumulated into a live local so
nothing is eliminated. Measured on 2026-09-05: `./mls/...` and `./message/...` come back **7,485
passing, 0 failing, 0 skipping**, identical to the baseline.
`TestDerivingTheViewsCostsLessThanTheRulesThatReadThem`, which bounds the views' share of section
12.2's aggregate at a tenth, reads 4.27% run on its own and 1.37% inside that full run -- the two
readings of one mutation being what a share is worth as a proxy for an order of growth.

`TestAPerTypeViewIsLinearInTheCommitOrder` stood over this until this commit and was deleted rather
than repaired. Both reasons are measurements:

- **It did not run in every run.** Over 20 full runs of the shipped tree -- 5 taken here on
  2026-09-05 and 15 in the round before -- it skipped 3 times, the run coming back 7,488 passing and
  1 skipping instead of 7,489 and 0. Every count this package reported was a sample of a two-valued
  distribution. The cause is the calibration, which times one COLD block and grows the round count
  only until that block clears twice the floor: in the runs that settled on 120 rounds the
  calibration must therefore have run at 68us per round or more, while the measured blocks ran at 51
  to 78us, so the margin those blocks actually keep is nearer 1.6x than the 2x the comment claims.
  One observed skip came in at 4.0004ms against a floor of 4.0496ms.
- **Its vacuity guard could not fire.** Replace the four accessors with closures over precomputed
  slices -- the case it exists for, "the accessor's call was optimised away and this gate measured
  nothing" -- and the result is a SKIP and not the FATAL beneath it: *the shortest views block ran
  for 0s against a vacuity floor of 4.1504ms*. An eliminated accessor makes the block SHORT, and
  short is what the skip is for, so the skip is always reached first. On this machine anything under
  a ratio of about 0.55 skips before the `over < 0.25` arm can be read.

What would close it is counting work rather than timing it: an operation counter incremented where
the per-entry work happens, compared across two widths, which is deterministic, has no floor, and
makes both the ceiling and the vacuity check exact. The obstruction is where the counter has to
live. `viewOf`'s per-entry work is one field comparison in production code with no seam, so a counter
a quadratic rewrite would actually move has to be incremented INSIDE that loop in the shipped build:
a plain counter there is a data race on a list two readers share, and an atomic one is an atomic add
per proposal per read. A counter incremented once per CALL is exactly the counter a quadratic inner
loop leaves unchanged -- vacuous against the one mutation it exists for. The untried option is a
static reading: no accessor's call closure walks the commit order inside another walk of it. It was
not built in this round, which is deleting a gate that did not hold its class rather than adding one.

## Two holes in the erase reading

`staged_erase_test.go`'s drop-site reading finds every assignment a member's own method makes to a
field holding key material and requires it to erase what it drops, refuse to overwrite a live one, or
move it. Two shapes are outside it. Both were measured on 2026-09-05, each against a byte-identical
control that IS reported, and each survives the whole of `./mls/...` and `./message/...` at 7,485
passing.

- **A callee resolved by name and arity, not by the receiver's type.** Mutation: put
  `other := self.Clone()` and `other.EncryptionPriv = nil` at the top of
  `(*TreeKEMPrivate).Consistent` -- a live copy of the leaf private key, dropped unerased. Not
  reported. The same two lines through a uniquely named `cloneForDrop()` ARE reported, at 15 sites.
  The cause is in `eraseReceiverSourcedTypes`: `Clone` is declared on eight types here, every one of
  them answering one result, and the lookup takes the first declaration whose result COUNT matches,
  which is `*GroupContext`'s, out of the file that sorts first -- the reading walks its paths
  sorted. `Clone` is not the only shared name that answers a result: `Encode`, `Epoch`,
  `EpochAuthenticator`, `Export`, `LeafCount`, `Members`, `Validate`, `MarshalMLS` and
  `UnmarshalMLS` are each declared by more than one production type. `Zeroize` is declared eight
  times as well but answers nothing, so it binds no local and is not a hazard here. The bare-name
  index next door is safe for a reason this one does not have: `labelledDeclarationsIn` and
  `newCommitSourceReader` walk EVERY match, so their over-reach costs work rather than accuracy,
  while this one returns the FIRST. Closing it: resolve the receiver expression's own type and key
  the lookup on (type, name) rather than on name.
- **A local taken out of a local.** Mutation, in `(*SecretTree).RestoreSenderRatchets`: delete the
  `zeroizeSecret` in front of its assignment and write the assignment as `holder := r` followed by
  `holder.secret = ...` -- the same pointer, the same store, a live ratchet secret dropped. Not
  reported. Deleting the `zeroizeSecret` and leaving the assignment on `r` IS reported, at 14 sites,
  which is the site the widening in 84e7a04 was written for. Closing it: take the local reading to
  its fixpoint, so that a local bound out of a tracked local is tracked.

Both are now stated as holes in that file's own prose. Until this commit both were stated there as
properties the reading has, which is the defect this file exists for pointed at a test.

---

## Closed

- **`sealAndRecordLocked`'s own drop of the ciphertext.** All three callers write `return nil, err`
  over whatever it hands them, so rewriting its `return nil, err` as `return private, err` was
  invisible. Closed by `TestSealAndRecordDropsTheCiphertextWhoseGenerationItCouldNotRecord`, which
  calls the function rather than a door: under that rewrite the whole of `./mls/...` and
  `./message/...` comes back 7,488 passing and exactly one failing, and the one is that case.

// The PURITY oracle for EphKey, and the reason it is not the second expansion ephkey_test.go
// correctly refuses.
//
// READ THIS BEFORE DELETING THE FILE AS A DUPLICATE, because at a glance it is exactly the thing
// the known answers are written the way they are in order to avoid. ephkey_test.go says, and it is
// right: an expansion written IN THIS MODULE is worthless as a check of the FORMULA. It would run
// on this module's own understanding of the three things a derivation is most easily wrong about --
// the info's field order, each field's width, and the byte order of a u64 -- so it would agree with
// a transposed implementation as readily as with a correct one, and a gate that agrees with the bug
// is worse than no gate. That is why the twenty seven known answers next door are HEX STRINGS
// computed from RFC 5869 and MASTER section 8.1 outside this module, and not a second expansion.
//
// THAT ARGUMENT IS ABOUT THE FORMULA AND IT DOES NOT REACH PURITY. The two are different
// properties with different failure modes:
//
//	the FORMULA property   the octets are HKDF-Expand(root, "eph/v1" || u8(b) || be64(t), 32).
//	                       A shared misunderstanding between subject and oracle is FATAL: both
//	                       sides move together and the comparison is silent. Checked by the known
//	                       answers, and only by them. This file cannot check it and does not claim
//	                       to -- a disagreement here says the two sides DIFFER, never which of them
//	                       is right.
//	the PURITY property    the octets are a function of (eph_root, bucket, window) AND OF NOTHING
//	                       ELSE. A shared misunderstanding is IRRELEVANT: two implementations wrong
//	                       in the same way still agree at every point, and a clock stirred into one
//	                       of them moves THAT side of the comparison and not the other. The
//	                       disagreement a clock creates survives any amount of shared error about
//	                       the formula.
//
// So the objection that forbids a second expansion as a formula check costs nothing here, and this
// file says so in its own voice rather than leaving a later reader to delete this gate for the
// reason ephkey_test.go gives about a different thing. MEASURED rather than argued: the oracle
// below was mutated to write the window LITTLE endian and all 1,200 drawn points went red against
// correct committed bytes -- which is the demonstration that this comparison cannot tell a formula
// difference from a purity one, and therefore that the hex strings next door are still the only
// formula check in this directory.
//
// WHAT THIS BUYS THAT A TABLE CANNOT, and it is the finding the 2026-09-13 close-out review
// MEASURED rather than suggested. The known answers pin POINTS. Purity is a per input property, so
// a clock conditioned on a point no row carries is invisible to them however wide the table gets --
// two roots of 2^256, and ten windows of 2^64. An oracle pins no points. It compares the two sides
// at whatever input it is handed, so it covers a CONDITION in proportion to that condition's
// MEASURE under the distribution its inputs are DRAWN from. A firing set of probability p is found
// at least once in N draws with probability 1 - (1-p)^N; at N = 600 that is 99.8 percent for
// p = 0.01, 45 percent for p = 0.001, and zero for every run anybody will ever make at p = 2^-64.
// That arithmetic is the whole of what this file is worth, and it is why the draw is not a detail.
//
// IT DRAWS ITS INPUTS AND DOES NOT ENUMERATE THEM, and that is the mechanism rather than a
// stylistic choice: a fixed list of inputs written into this file is a table with extra steps, and
// it would inherit the exact defect seventeen rows clustered at window 0 had -- a condition on a
// point nobody wrote down walks past it. The seed is drawn from crypto/rand and LOGGED, so a
// failure reproduces for a reader who has the log and nothing else, and the points are still not in
// the source.
package messagegroup

import (
	"bytes"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	mathrand "math/rand/v2"
	"slices"
	"testing"

	"github.com/urnetwork/connect/message"
)

// How many points each distribution is compared at.
//
// The number is a cost and not a strength. W1's firing set has measure ~1 and W5's has measure 4/6
// -- measured, mean 399.7 of 600 over ten runs -- so both die in the first handful of draws, and
// six hundred is only what makes the PRINTED RATE steady enough to compare between runs.
//
// RAISING IT WOULD NOT REACH THE SHAPES THAT ESCAPE, AND FOR TWO DIFFERENT REASONS. A condition
// whose measure is merely SMALL -- W4's 2^-64 -- is not reached by six hundred million either. But
// S1 and S2 at the head of the test below escape the production shaped draw at measure EXACTLY
// ZERO, not merely small: that draw's clock band stops at 2040, so no number written here reaches
// a window a sender computes after it. For those two the fix would be a different SUPPORT and not
// more points, and the honest response to both is the measurement below rather than a larger
// number here.
const ephPurityDraws = 600

// The wall clock band the production shaped draw samples, as unix milliseconds, written out rather
// than read off a clock.
//
// 2020-01-01T00:00:00Z and 2040-01-01T00:00:00Z. A draw whose upper end was time.Now() would be a
// gate whose coverage changes with the day it runs on, and a measurement nobody can reproduce next
// year is a failure this corpus has already paid for in other forms.
const (
	ephPurityFirstMs int64 = 1577836800000
	ephPurityLastMs  int64 = 2208988800000
)

// The band plant W5 fires in, as two constants so that this file's reporting and ephkey_test.go's
// are measured against the same two numbers rather than against two copies of them. Open at both
// ends, exactly as the plant is written.
const (
	ephPurityBandLow  uint64 = 1000
	ephPurityBandHigh uint64 = 1000000
)

// The names ephPurityOracle must not reach, asserted against its parsed body by
// TestThePurityOracleSharesNoDeclarationWithTheSubject.
//
// An oracle that had drifted into answering whatever EphKey answers -- by calling it, by sharing
// its closure, by being edited into a copy of it during some later cleanup -- reports zero
// disagreements at every point and reads exactly like a property that holds. The runtime guards
// below catch an oracle that IGNORES an argument; they cannot catch that one, because an oracle
// that is EphKey is sensitive to all three arguments in precisely the right way. Only the source
// catches it.
var ephPuritySubjectNames = []string{"EphKey", "ephLabelledInfo", "keyScheduleExpand", "ephKeyInfo", "ephKeyBytes"}

// The file this gate reads itself out of. Read off disk at run time, because a claim about what a
// function's body does not name is a claim about the source and not about a comment.
const ephPuritySourcePath = "ephpurity_test.go"

// ephPurityOracle is HKDF-Expand written out, reading no declaration of this package.
//
// It spells the label as a literal rather than through ephKeyInfo, and takes its length from the
// hash rather than through ephKeyBytes: the point of a second side is that it is a second side, and
// a constant shared with the subject is one fewer thing that can disagree. That is a PURITY
// argument and not a claim that sharing less makes this a formula check -- see the head of the file.
//
// RFC 5869 section 2.3 with L = 32 = HashLen is exactly one block, so the loop the RFC writes
// collapses: T(0) is empty, T(1) = HMAC-Hash(PRK, T(0) || info || 0x01), and OKM is T(1).
func ephPurityOracle(root []byte, bucket uint8, window uint64) []byte {
	info := []byte("eph/v1")
	info = append(info, bucket)
	for shift := 56; 0 <= shift; shift -= 8 {
		info = append(info, byte(window>>uint(shift)))
	}
	mac := hmac.New(sha256.New, root)
	mac.Write(info)
	mac.Write([]byte{0x01})
	return mac.Sum(nil)
}

// The rungs, read off connect/message's ladder rather than written as "0 through 5", for the same
// reason every other class in this directory is derived: a rung added upstream has to arrive here
// as a changed number and not as silence.
func ephPurityLadder(t *testing.T) []uint8 {
	t.Helper()
	rungs := []uint8{}
	for candidate := 0; candidate <= 0xFF; candidate += 1 {
		if 0 <= message.EphBucketSeconds(uint8(candidate)) {
			rungs = append(rungs, uint8(candidate))
		}
	}
	if len(rungs) == 0 {
		t.Fatal("message.EphBucketSeconds named no rung at all, so every draw below would have had no bucket to make and this gate would have compared nothing")
	}
	return rungs
}

// A seeded source, with the seed printed so a failure reproduces from the log alone.
func ephPuritySource(t *testing.T) *mathrand.Rand {
	t.Helper()
	seed := [32]byte{}
	if _, err := rand.Read(seed[:]); err != nil {
		t.Fatalf("the draw could not be seeded, so this gate has no inputs at all: %v", err)
	}
	t.Logf("seed %s -- every point below is reproducible from it, and none of them is written in this file",
		hex.EncodeToString(seed[:]))
	return mathrand.New(mathrand.NewChaCha8(seed))
}

func ephPurityRoot(source *mathrand.Rand) []byte {
	root := make([]byte, EphRootBytes)
	for offset := 0; offset+8 <= len(root); offset += 8 {
		binary.BigEndian.PutUint64(root[offset:], source.Uint64())
	}
	return root
}

// The two distributions, and THE DIFFERENCE BETWEEN THEM IS THE FINDING. Both are drawn; only one
// of them reaches the band a real sender computes in.
var ephPurityDistributions = []struct {
	name   string
	why    string
	window func(source *mathrand.Rand, bucket uint8) uint64
}{
	{
		name: "uniform over 2^64",
		why:  "the window field's whole declared range, which is what says this comparison is not fitted to any one band",
		window: func(source *mathrand.Rand, bucket uint8) uint64 {
			return source.Uint64()
		},
	},
	{
		name: "production shaped",
		why:  "MASTER section 8's own formula over a wall clock instant drawn from 2020-01-01 to 2040-01-01, which is the only draw that reaches the windows a real sender computes -- and it reaches ONLY the ones inside that band, never a window a sender computes after 2040",
		window: func(source *mathrand.Rand, bucket uint8) uint64 {
			sentAtMs := ephPurityFirstMs + source.Int64N(ephPurityLastMs-ephPurityFirstMs)
			seconds := message.EphBucketSeconds(bucket)
			if seconds <= 0 {
				// bucket 0: MASTER section 8.1 says its window is 0 by definition and is
				// never computed. The divisor would be nought.
				return 0
			}
			return uint64(sentAtMs / (int64(seconds) * 1000))
		},
	},
}

// TestEphKeyIsAFunctionOfItsThreeArgumentsOverDrawnInputs is the purity half of P5, and it is the
// only gate in this directory whose coverage is a MEASURE rather than a list of points.
//
// -- CLASS: (eph_root, bucket, window) triples DRAWN from two distributions, not enumerated.
// -- SCOPE: EphKey against ephPurityOracle above, which shares no declaration with it and is
//
//	asserted to share none by TestThePurityOracleSharesNoDeclarationWithTheSubject.
//
// -- PROPERTY: the two agree at every drawn point. They agree because EphKey is a function of its
//
//	three arguments; anything else EphKey reads moves its side of the comparison and not the
//	oracle's, whatever else the two might be wrong about together.
//
// MEASURED ON THIS COMMIT. Each plant is a clock behind a fmt.Stringer in connect/mls/syntax reached
// from EphKey by fmt.Sprint -- the shape the reference graph gate cannot see, and that gate is GREEN
// for every row below -- with the firing condition the only difference between rows. The KAT column
// is the twenty seven known answers next door, run at the same time, and it is here because the two
// gates fail over DIFFERENT things and that is the whole reason to have both:
//
//	plant                                        KAT rows   uniform 2^64  production shaped   runs
//	committed bytes                              0 of 27     0 of 600      0 of 600          --
//	Pu  fired unconditionally                    27 of 27  600 of 600    600 of 600         3/3
//	    bucket == b, at each rung b              2/5/4/5/4/7, summing to all 27              --
//	P3  bucket == 3                              5 of 27    89..101       90..116           3/3
//	W1  every root but the two the table pins    0 of 27   600 of 600    600 of 600         3/3
//	W2  every window but the ten the table pins  0 of 27   600 of 600    499..516           3/3
//	W5  1000 < window < 1000000                  8 of 27     0 of 600    375..413          10/10
//	R1  ephRoot[0] == 0x00            (2^-8)     0 of 27     0..5          2..6            12/12
//	R2  ephRoot[0] == 0 && [1] < 0x10 (2^-12)    0 of 27     0..2          0               6/20
//	W4  bucket == 5 && window == 17              0 of 27     0 of 600      0 of 600         0/6
//	S1  bucket == 5 && 1000 < window < 1000000   0 of 27     0 of 600      0 of 600         0/6
//	S2  613607 < window < 1000000                0 of 27     0 of 600      0 of 600         0/6
//
// EVERY SIX HUNDRED IS A COUNT OF A RANDOM DRAW AND NOT A CONSTANT, which is why the columns above
// carry ranges and a RUNS KILLED column rather than one number each. W5's production shaped count
// read 375..413 over ten runs, mean 399.7, against Binomial(600, 4/6) = 400 -- four of the six
// rungs put a 2020-2040 window in the band, which the SUPPORT lines below print. R2 is the row that
// makes the point sharpest: a root condition of measure 2^-12 is found with probability
// 1-(1-2^-12)^1200 = 25% per run, and it was killed in 6 of 20 runs, so it PASSES most CI runs.
// Even the 600 of 600 cells are draws and not certainties: W1 agrees whenever a drawn root lands on
// one of the two pinned (first, last) octet pairs, expected 600 * 2 * 2^-16 = 0.018 times per run,
// so its complement is RARE and not EMPTY. It read 600 of 600 in the three runs measured here.
// A single figure in any of these cells would be a seed and not a fact.
//
// It is not noise around nothing -- under W5 the disagreement count and the IN BAND draw count
// printed on the next line are the SAME NUMBER, re-measured 5 of 5 runs here at 413/413, 413/413,
// 389/389, 386/386 and 387/387, which is the direct measurement that what this gate caught is
// exactly the band and not a coincidence that happens to be the right size.
//
//	query: plant the edit in EphKey, then
//	go test -count=1 -v -run TestEphKeyIsAFunctionOfItsThreeArgumentsOverDrawnInputs ./messagegroup/
//	go test -count=1 -v -run TestEphKeyIsMasterSection81sDerivationAndNotThisPackagesOpinionOfIt ./messagegroup/
//	and the negative for every zero above is an unfiltered go test -timeout 600s ./messagegroup/,
//	because -run filters subtests and can only ever support "this IS caught".
//
// WHAT THIS DOES NOT KILL, stated as plainly as what it does, because an unstated boundary is the
// next hole and this line has already published two sentences that were too wide:
//
//   - THE SHAPES NAMED UNDER "WHAT SURVIVED" BELOW, which are published as a MEASUREMENT and not
//     as a characterisation. Four successive attempts to describe this residue in one sentence
//     were each too strong and each was corrected by the next measurement; the most recent called
//     it ONE (bucket, window) pair of measure 2^-64, and two of the three shapes below are about
//     2^21 cheaper than that and are reachable by a real sender. An oracle NARROWS the unsampled
//     root and window classes in proportion to a condition's measure. It does not close them, and
//     nothing finite does.
//   - THE BINDING TIME CLASS, ledger item MG-3. A clock installed by an init in a package only the
//     production composition root links is NIL in the binary this test runs in, at every point of
//     the input space. No table of any width and no oracle of any width reaches it, because there
//     is nothing to reach: in this binary EphKey really is pure. MG-3 stays FILED; nothing here
//     rules it, and nothing here is evidence about it either way.
//   - WHICH SIDE IS RIGHT. A disagreement says the two differ. The formula is the known answers' to
//     hold, and this gate goes equally red when the ORACLE is the wrong one, which was measured by
//     transposing the oracle's window to little endian: 1,200 of 1,200 red against correct
//     committed bytes.
//   - W5 DIES TO THE PRODUCTION SHAPED DRAW AND NOT TO THE UNIFORM ONE, and that is the same lesson
//     the twenty seven rows carry from the other side. A uniform uint64 window exceeds 1000000 with
//     probability 1 - 2^-44, so the uniform draw enters the band about 3e-11 times in six hundred.
//     Distribution IS coverage, and a draw over a field's declared range is not a draw over the
//     values anything actually computes.
//
// WHAT SURVIVED, NAMED. Each of these was planted, run and found green on EVERYTHING in this tree:
// 0 of 27 known answer rows, 0 of 600 on BOTH draws in six runs each, the reference graph gate
// green, and an unfiltered go test ./messagegroup/ at 0 failures. A shape that survives cannot be
// shown value-changing by a gate going red, so each was PROBED directly: each moves the derived
// octets at its own firing point and at no other point probed.
//
//	bucket == 5 && window == 17
//	    NOT production reachable: bucket 5's window 17 is 1971-04-22.
//
//	bucket == 5 && 1000 < window < 1000000
//	    PRODUCTION REACHABLE FROM 2046-09-27, the instant bucket 5's window first exceeds
//	    1000: 1001 * 2419200 * 1000 = 2421619200000. From that date up to window 1000000
//	    (the year 78600), every 28-day window a real sender computes is inside it.
//	    ephkey_test.go discloses the gap and the rung SUPPORT lines print it; it is named
//	    HERE because this is where a reader looks for what survives.
//
//	613607 < window < 1000000
//	    PRODUCTION REACHABLE FROM 2040-01-01: the 386,392 hourly windows bucket 1 computes
//	    between 2040-01-01 and 2084-01-29. 2040 is where ephPurityLastMs stops, not where
//	    senders stop. The production shaped draw reaches 205,585 of this band's 998,999
//	    windows, so 79.4% of the band is outside BOTH draws and outside the table.
//
// THE BOUNDARY OF THIS SURVIVING CLASS IS NOT KNOWN TO BE TIGHT. These are the shapes that have
// been TRIED, not the shapes that EXIST. This corpus has not characterised the set of conditions
// this ensemble misses; each of the three was found by trying one more, and the two a real sender
// reaches were found only after a sentence had already called the residue a single point.
func TestEphKeyIsAFunctionOfItsThreeArgumentsOverDrawnInputs(t *testing.T) {
	if ephPurityDraws <= 0 {
		t.Fatal("this gate is configured to draw no point at all, so it would report the clean pass a holding property reports, having compared nothing")
	}
	if len(ephPurityDistributions) == 0 {
		t.Fatal("no distribution is configured, so nothing was drawn from anything")
	}
	rungs := ephPurityLadder(t)
	// TWO RUNGS, because the bucket sensitivity guard below compares the oracle against itself at
	// the NEXT rung and a one rung ladder has none. MEASURED with the ladder cut to a single rung:
	// that guard reported "the oracle answered something DIFFERENT at the next rung ... for only 0
	// of 600" and blamed the ORACLE for a ladder that cannot be walked. A gate that names the wrong
	// cause is a gate the next reader deletes, so the real cause is asserted here instead -- and it
	// is a Fatal rather than an `if` around the guard, because silently skipping a liveness check
	// is the vacuity this whole block exists to refuse.
	if len(rungs) < 2 {
		t.Fatalf("the ladder names %d rung(s) and the bucket sensitivity guard below needs two, because it compares the oracle with itself one rung along. With one rung that guard fires and blames the oracle; this Fatal names the ladder, which is what actually moved",
			len(rungs))
	}
	source := ephPuritySource(t)

	for _, distribution := range ephPurityDistributions {
		drawn, disagreeing := 0, 0
		windowLive, rootLive, bucketLive := 0, 0, 0
		inBand, outOfBand := 0, 0
		bandRungs, unbandRungs := []uint8{}, []uint8{}
		firstDisagreement := ""

		for draw := 0; draw < ephPurityDraws; draw += 1 {
			root := ephPurityRoot(source)
			bucket := rungs[source.IntN(len(rungs))]
			window := distribution.window(source, bucket)
			drawn += 1

			if ephPurityBandLow < window && window < ephPurityBandHigh {
				inBand += 1
				if !slices.Contains(bandRungs, bucket) {
					bandRungs = append(bandRungs, bucket)
				}
			} else {
				outOfBand += 1
				if !slices.Contains(unbandRungs, bucket) {
					unbandRungs = append(unbandRungs, bucket)
				}
			}

			subject := EphKey(root, bucket, window)
			answer := ephPurityOracle(root, bucket, window)
			if !bytes.Equal(subject, answer) {
				disagreeing += 1
				if firstDisagreement == "" {
					firstDisagreement = fmt.Sprintf("EphKey(root %s, bucket %d, window %d) = %s and RFC 5869's own expansion of those same three arguments is %s",
						hex.EncodeToString(root), bucket, window, hex.EncodeToString(subject), hex.EncodeToString(answer))
				}
			}

			// THE FAIL CLOSED HALF, and it is the reason a zero above means anything at all. An
			// oracle that ignores one of its three arguments agrees with EphKey on every draw
			// where that argument happens not to be what differs, and reads as a property that
			// holds. So the oracle is also asked for three points where it MUST answer something
			// else -- one window up, one bit of the root flipped, the next rung along -- and each
			// count is asserted as a NUMBER equal to the draws, not as non emptiness.
			if !bytes.Equal(answer, ephPurityOracle(root, bucket, window+1)) {
				windowLive += 1
			}
			other := append([]byte(nil), root...)
			other[0] ^= 0x01
			if !bytes.Equal(answer, ephPurityOracle(other, bucket, window)) {
				rootLive += 1
			}
			// unconditional, like the other two: the two rung Fatal above is what makes
			// nextRung != bucket true, rather than an `if` here that would silently skip.
			nextRung := rungs[(slices.Index(rungs, bucket)+1)%len(rungs)]
			if !bytes.Equal(answer, ephPurityOracle(root, nextRung, window)) {
				bucketLive += 1
			}
		}

		if drawn != ephPurityDraws {
			t.Errorf("%s: %d point(s) were drawn and this gate is configured for %d", distribution.name, drawn, ephPurityDraws)
		}
		if inBand+outOfBand != drawn {
			t.Errorf("%s: %d in band plus %d out of band is not the %d drawn, so this file's own arithmetic about its coverage is wrong",
				distribution.name, inBand, outOfBand, drawn)
		}
		slices.Sort(bandRungs)
		slices.Sort(unbandRungs)

		t.Logf("%s (%s): %d point(s), %d disagreeing; COMPLEMENT: %d agreeing",
			distribution.name, distribution.why, drawn, disagreeing, drawn-disagreeing)
		t.Logf("%s: %d of %d drawn window(s) land in the production band %d < t < %d, on rung(s) %v; COMPLEMENT: %d outside it, on rung(s) %v",
			distribution.name, inBand, drawn, ephPurityBandLow, ephPurityBandHigh, bandRungs, outOfBand, unbandRungs)

		if disagreeing != 0 {
			t.Errorf("%s: EphKey and an expansion that shares no declaration with it disagree at %d of %d drawn point(s). The two cannot disagree about the FORMULA in a way that matters here -- a shared misunderstanding moves both sides together -- so a disagreement is EphKey reading something that is not one of its three arguments. First: %s",
				distribution.name, disagreeing, drawn, firstDisagreement)
		}
		for _, live := range []struct {
			argument string
			count    int
			perturb  string
		}{
			{"window", windowLive, "the next window up"},
			{"eph_root", rootLive, "one bit of the eph_root flipped"},
			{"bucket", bucketLive, "the next rung of the ladder"},
		} {
			if live.count != drawn {
				t.Errorf("%s: the oracle answered something DIFFERENT at %s for only %d of %d drawn point(s), and it has to at all %d. An oracle that is not sensitive to its %s argument agrees with EphKey wherever that argument is what differs, and the %d disagreement(s) reported above would mean nothing",
					distribution.name, live.perturb, live.count, drawn, drawn, live.argument, disagreeing)
			}
		}
	}
}

// TestThePurityOracleSharesNoDeclarationWithTheSubject is the guard the runtime cannot be: an
// oracle that IS EphKey passes every sensitivity check above, because it is sensitive to all three
// arguments in exactly the right way, and reports zero disagreements for ever.
//
// -- CLASS: every identifier ephPurityOracle's parsed body names.
// -- SCOPE: this file, read off disk at run time rather than described in a comment.
// -- PROPERTY: none of ephPuritySubjectNames appears. The complement -- how many of the names it
//
//	reaches are banned -- is asserted as a number, and the count of names it reaches at all is
//	asserted non empty, so a walk that read an empty body fails closed instead of reporting that
//	the banned set was absent from nothing.
func TestThePurityOracleSharesNoDeclarationWithTheSubject(t *testing.T) {
	if len(ephPuritySubjectNames) == 0 {
		t.Fatal("the banned set is empty, so no name could have been found in it and this gate would pass over any oracle at all")
	}
	parsed, err := parser.ParseFile(token.NewFileSet(), ephPuritySourcePath, nil, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parse %s: %v -- this gate reads its subject off disk, so a file it cannot read is a gate with no input", ephPuritySourcePath, err)
	}
	var subject *ast.FuncDecl
	for _, declaration := range parsed.Decls {
		function, isFunction := declaration.(*ast.FuncDecl)
		if isFunction && function.Recv == nil && function.Name.Name == "ephPurityOracle" && function.Body != nil {
			subject = function
		}
	}
	if subject == nil {
		t.Fatalf("ephPurityOracle was not found in %s, so this gate read nothing and the absence it would otherwise report is the absence of a reading", ephPuritySourcePath)
	}

	named, banned := []string{}, []string{}
	ast.Inspect(subject.Body, func(node ast.Node) bool {
		identifier, isIdentifier := node.(*ast.Ident)
		if !isIdentifier {
			return true
		}
		if !slices.Contains(named, identifier.Name) {
			named = append(named, identifier.Name)
		}
		if slices.Contains(ephPuritySubjectNames, identifier.Name) && !slices.Contains(banned, identifier.Name) {
			banned = append(banned, identifier.Name)
		}
		return true
	})
	slices.Sort(named)
	t.Logf("class: %d banned name(s) %v; ephPurityOracle's body names %d identifier(s) %v; COMPLEMENT: %d of them banned %v",
		len(ephPuritySubjectNames), ephPuritySubjectNames, len(named), named, len(banned), banned)
	if len(named) == 0 {
		t.Fatal("ephPurityOracle's body names no identifier at all, so the walk above read an empty body and its clean answer is vacuous")
	}
	if len(banned) != 0 {
		t.Errorf("ephPurityOracle names %v. An oracle that reaches the subject it is comparing against agrees with it at every point and reports zero disagreements for ever, which is the one failure mode the drawn comparison above cannot see",
			banned)
	}
}

// TestTheProductionShapedDrawActuallyReachesTheBandTheKnownAnswersMissed is the claim this file's
// existence rests on, asserted rather than described, and independently of any draw.
//
// -- CLASS: the rungs of the ladder.
// -- SCOPE: the production shaped distribution above, at every rung, over its whole 2020-2040 band.
// -- PROPERTY: the set of rungs whose windows fall WHOLLY inside 1000 < t < 1000000 over that
//
//	period is non empty, and the COMPLEMENT -- the rungs a production shaped draw never puts in
//	the band -- is printed with its members and each member's window range, because that is the
//	part a reader would otherwise assume away.
//
// MEASURED, and this is the row that says why widening the table was never going to be enough on
// its own: bucket 0's window is 0 by definition and can never be in the band, and BUCKET 5's
// production window is 730 at 2026-01-01 and does not exceed 1000 until 2046-09-27. A clock
// conditioned on this band is invisible at bucket 5 for another twenty years -- to the twenty seven
// rows and to this file's draw alike.
func TestTheProductionShapedDrawActuallyReachesTheBandTheKnownAnswersMissed(t *testing.T) {
	rungs := ephPurityLadder(t)
	inBand, outOfBand := []uint8{}, []uint8{}
	reason := map[uint8]string{}
	for _, rung := range rungs {
		seconds := message.EphBucketSeconds(rung)
		low, high := uint64(0), uint64(0)
		if 0 < seconds {
			low = uint64(ephPurityFirstMs / (int64(seconds) * 1000))
			high = uint64((ephPurityLastMs - 1) / (int64(seconds) * 1000))
		}
		// set for EVERY rung, not only the out-of-band ones. Printing the range only when a
		// rung MISSES the band is what let "rung 1 stops at 613607" go unpublished, and 613607
		// is the left edge of a 386,392 window escape that is green on every gate in this tree.
		reason[rung] = fmt.Sprintf("its 2020-2040 windows run %d..%d", low, high)
		if ephPurityBandLow < low && high < ephPurityBandHigh {
			inBand = append(inBand, rung)
		} else {
			outOfBand = append(outOfBand, rung)
		}
	}
	t.Logf("class: %d rung(s) %v; a production shaped draw puts %d of them wholly inside %d < t < %d: %v",
		len(rungs), rungs, len(inBand), ephPurityBandLow, ephPurityBandHigh, inBand)
	// the draw's SUPPORT, printed for every rung. This is a Logf and asserts nothing; it is here
	// so the reachable set is something the run STATES rather than something a reader derives.
	// Over these six ranges the draw can produce 205,585 of the band's 998,999 windows, so 79.4%
	// of the band is outside it -- five contiguous gaps, the widest being (613607, 1000000) at
	// 386,392 windows. query: for each rung, low = ephPurityFirstMs/(seconds*1000) and
	// high = (ephPurityLastMs-1)/(seconds*1000); union the parts of [low,high] inside the band.
	for _, rung := range rungs {
		t.Logf("SUPPORT: rung %d -- %s", rung, reason[rung])
	}
	for _, rung := range outOfBand {
		t.Logf("COMPLEMENT member: rung %d is NOT reached by this band -- %s", rung, reason[rung])
	}
	t.Logf("COMPLEMENT: %d rung(s) %v", len(outOfBand), outOfBand)
	if len(inBand) == 0 {
		t.Errorf("no rung's production windows fall inside %d < t < %d, so the production shaped draw reaches the band plant W5 fires in at NO rung, and its disagreement count would be zero for a reason that has nothing to do with EphKey",
			ephPurityBandLow, ephPurityBandHigh)
	}
	if len(outOfBand) == 0 {
		t.Errorf("every rung's production windows fall inside the band, which contradicts what this file and ephkey_test.go both say about bucket 0 and bucket 5, and would make the complement printed above empty for a reason nobody measured")
	}
}

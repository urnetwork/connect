// connect's half of the shared eph_window known-answer table, and why one repository's gate is
// not a fastening.
//
// MASTER section 8's sender formula exists at more than one site and the duplication is FORCED
// rather than careless. Spec B section 2.2 allows the message-server module connect/message and
// connect/protocol and nothing else, so the harness that plays the sender over there cannot link
// this package's EphWindowAt, and section 12.1 hands it the divisor without the division. A test
// that called both and compared them is the test neither repository may write.
//
// THE TWO COPIES HAD ALREADY DRIFTED INTO A REAL DISAGREEMENT, which is why this file exists and
// not because a second table is tidy. The server's copy read `if seconds <= 0 { return 0 }`,
// collapsing "off-ladder bucket" and "bucket 0" into ONE answer -- the exact sentinel collision
// M1-27's second half was ruled to eliminate, reintroduced one repository over. Driven from a
// module outside both, the two answered differently on 9 of 32 probed (bucket, sent_at_ms) pairs:
// (0, -1) and every off-ladder pair. Nothing was looking: at that commit the harness package had
// no test file at all.
//
// WHAT CROSSES A FORBIDDEN IMPORT IS A VALUE. testdata/eph-window-kat.txt is 57 answers computed
// in python from section 8's sentence -- not read out of either implementation -- and confirmed
// against the shipped sender from a throwaway module outside both repositories, which is the one
// place section 2.2 permits the two to be imported together. msgrepo carries the same file, byte
// for byte, and drives its own two copies over it in harness/ephkat_test.go. This file is the
// other half, and landing it is what makes the fastening TWO GATES rather than one digest string
// compared by a person: an edit to the table in either repository turns the other red.
//
// THE FILE IS ONE TABLE CARRIED BY TWO REPOSITORIES AND NOT A RESTATEMENT, including its prose,
// and THE STALE PARAGRAPH IS NOW GONE FROM BOTH COPIES. Its header used to say "connect owes the
// other half ... until that lands", which this very file was the landing of; the sentence could not
// be corrected on either side alone, because the two copies are pinned by a digest over the WHOLE
// file, so a one sided comment edit is drift the other repository's gate goes red on. It was
// therefore made as one change in both repositories with both digests republished, which is the
// procedure SPEC-LEDGER.md item 193 carries, and 193 stays FILED rather than closed: what is
// fastened is a set of values, and no test in either repository can call the other's copy of the
// formula. What is owed here is byte-identity, and byte-identity is what is asserted.
//
// THE DIGEST MOVED ON 2026-09-13 and the old value is recorded rather than dropped, because a
// reader meeting a red gate needs to know whether the table was edited or smudged:
//
//	f6ef2ae645294a085ae88705209b756578f403029dcd0e0f5b2ef726e897712a  before, 57 rows
//	6cdbff6c52021bae040a79beda712b18afbe342036ea84cf3066dee24c1521b8  after, 57 rows, PROSE ONLY
//
// No answer changed. The edit removed the stale paragraph and added one naming sent_at_ms
// 1767225600000 as 2026-01-01T00:00:00Z, which is the instant ephkey_test.go's production shaped
// known answers are computed for -- so the two tables in this directory now state the same instant
// and one of them recomputes the other's windows from it.
package messagegroup

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/urnetwork/connect/message"
)

const ephWindowKatPath = "testdata/eph-window-kat.txt"

// The table's digest over its CANONICAL bytes -- CRLF folded to LF before hashing -- and it is
// THE SAME CONSTANT msgrepo's harness/ephkat_test.go pins. That is the whole mechanism: two
// repositories, one value, and no import between them.
//
// Canonical and not raw. core.autocrlf is true at system scope on the Windows boxes that build
// this repository, and .gitattributes pins this path to eol=lf so a checkout here writes the same
// octets msgrepo's does -- but connect's checkout rules are not msgrepo's to set and the reverse
// is equally true. A digest that changed with the checkout would say "the table drifted" on a
// clean clone, and the response to a gate that cries wolf is always to delete it.
//
// MEASURED on the committed file, not typed from memory:
//
//	sha256 of messagegroup/testdata/eph-window-kat.txt with \r\n -> \n
const ephWindowKatDigest = "6cdbff6c52021bae040a79beda712b18afbe342036ea84cf3066dee24c1521b8"

// The refusal names the table uses. They name the SENTINEL and not the message text, because the
// two repositories prefix their messages differently on purpose ("messagegroup: ..." against
// "harness: ...") and a table that compared prose would be a table that cannot be shared.
const (
	ephWindowKatOffLadder = "ERR_EPH_BUCKET_OFF_LADDER"
	ephWindowKatSentAt    = "ERR_EPH_WINDOW_SENT_AT"
)

// One row of the shared table.
type ephWindowKatRow struct {
	line     int
	bucket   uint8
	sentAtMs int64
	// exactly one of these is set: a window, or the name of a refusal
	window uint64
	refuse string
}

func (self ephWindowKatRow) answer() string {
	if self.refuse != "" {
		return self.refuse
	}
	return strconv.FormatUint(self.window, 10)
}

func (self ephWindowKatRow) String() string {
	return fmt.Sprintf("(bucket %d, sent_at_ms %d) -> %s [%s:%d]", self.bucket, self.sentAtMs, self.answer(), ephWindowKatPath, self.line)
}

// readEphWindowKat parses the table and FATALS rather than skipping on every way it can fail to
// read one.
//
// A missing, smudged or unparseable table must end the run. A gate whose input vanished reports
// the same clean pass a gate whose property holds reports, and that is the failure mode this
// corpus has already paid for: an overlay substitution of this very file came back green on the
// other side, because -overlay is a COMPILER substitution and os.ReadFile is a run time read.
// Anything that mutates this gate's INPUT has to edit the real file.
func readEphWindowKat(t *testing.T) []ephWindowKatRow {
	t.Helper()
	raw, err := os.ReadFile(filepath.FromSlash(ephWindowKatPath))
	if err != nil {
		t.Fatalf("the shared known-answer table could not be read, so this gate has no input at all: %v", err)
	}
	canonical := strings.ReplaceAll(string(raw), "\r\n", "\n")

	sum := sha256.Sum256([]byte(canonical))
	if measured := hex.EncodeToString(sum[:]); measured != ephWindowKatDigest {
		t.Fatalf("the shared known-answer table's digest is %s and this file pins %s; this table is one half of a pair the dependency rule will not let a test compare, so an edit to it HERE is an edit msgrepo cannot see. If the change is deliberate, make it in BOTH repositories and republish the digest in ledger item 193",
			measured, ephWindowKatDigest)
	}

	rows := []ephWindowKatRow{}
	for index, text := range strings.Split(canonical, "\n") {
		trimmed := strings.TrimSpace(text)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		fields := strings.Fields(trimmed)
		if len(fields) != 3 {
			t.Fatalf("%s:%d has %d fields and every row of this table has three: %q", ephWindowKatPath, index+1, len(fields), trimmed)
		}
		bucket, err := strconv.ParseUint(fields[0], 10, 8)
		if err != nil {
			t.Fatalf("%s:%d does not name a bucket byte: %v", ephWindowKatPath, index+1, err)
		}
		sentAtMs, err := strconv.ParseInt(fields[1], 10, 64)
		if err != nil {
			t.Fatalf("%s:%d does not name a sent_at_ms reading: %v", ephWindowKatPath, index+1, err)
		}
		row := ephWindowKatRow{line: index + 1, bucket: uint8(bucket), sentAtMs: sentAtMs}
		switch fields[2] {
		case ephWindowKatOffLadder, ephWindowKatSentAt:
			row.refuse = fields[2]
		default:
			window, err := strconv.ParseUint(fields[2], 10, 64)
			if err != nil {
				t.Fatalf("%s:%d's answer is neither a window nor one of %q / %q: %v", ephWindowKatPath, index+1, ephWindowKatOffLadder, ephWindowKatSentAt, err)
			}
			row.window = window
		}
		rows = append(rows, row)
	}
	if len(rows) == 0 {
		t.Fatalf("%s parsed to no rows at all, so this gate read nothing", ephWindowKatPath)
	}
	return rows
}

// ephWindowKatAnswerOf names what EphWindowAt answered, in the table's own vocabulary.
func ephWindowKatAnswerOf(window uint64, err error) string {
	switch {
	case errors.Is(err, ErrEphBucketOffLadder):
		return ephWindowKatOffLadder
	case errors.Is(err, ErrEphWindowSentAt):
		return ephWindowKatSentAt
	case err != nil:
		return "unclassified refusal: " + err.Error()
	}
	return strconv.FormatUint(window, 10)
}

// Every row of the shared table is the answer THIS copy of MASTER section 8's formula gives.
//
// ORDER OF REFUSAL IS PART OF THE ANSWER, and it is the half that a value table can hold and a
// prose paragraph cannot. Rows (6, -1), (16, -1), (21, -1) and (255, -1) are where the two guards
// are asked in the wrong order: a copy that tests the clock reading before the ladder answers
// ERR_EPH_WINDOW_SENT_AT there, and a copy that tests the ladder first answers
// ERR_EPH_BUCKET_OFF_LADDER. EphWindowAt tests the ladder first, by switching on
// message.EphBucketSeconds before it looks at the reading at all, and the table says so.
func TestEphWindowAtAnswersTheSharedKAT(t *testing.T) {
	rows := readEphWindowKat(t)
	for _, row := range rows {
		measured := ephWindowKatAnswerOf(EphWindowAt(row.bucket, row.sentAtMs))
		if measured != row.answer() {
			t.Errorf("%s, and this copy answered %s", row, measured)
		}
	}
	t.Logf("%d rows of %s answered by messagegroup.EphWindowAt", len(rows), ephWindowKatPath)
}

// The table is a SAMPLE of a domain it cannot enumerate, so here is what it leaves out, named and
// asserted rather than left for a reader to worry about. This is msgrepo's second gate written
// over connect's function, and it is what still bites when the digest is republished along with
// an edit -- which is the one way a table can change and the digest clause see nothing.
//
// The class is every bucket byte a uint8 can hold: 256 members, derived from the width and not
// typed. message.EphBucketSeconds is the one place that says which of them name a rung, so the
// partition is read off the ladder and a rung added upstream moves it here without an edit.
//
// WHAT THIS ASSERTS, in order: the table names every bucket that HAS a rung, because a rung the
// table never exercises is a divisor nothing pins; the complement -- the off-ladder bytes the
// table does NOT name -- is non-empty, is exactly 256 minus the rungs minus the off-ladder bytes
// the table does name, and every one of its members is refused with the same sentinel the named
// ones are, at both signs of the reading, which is where the ORDER of the two guards shows. The
// complement is printed with its members, because a complement whose size is asserted and whose
// membership is never shown is the shape that hid a five-of-seventeen in this corpus already.
//
// AND the three answer classes are each non-empty. A table that lost every refusal row would
// still name every rung and still have the right complement arithmetic, and it would pin the
// division while saying nothing at all about the two guards in front of it.
func TestTheSharedKatsComplementOverTheBucketClassIsNamedAndRefused(t *testing.T) {
	rows := readEphWindowKat(t)

	rungs := []uint8{}
	offLadder := []uint8{}
	for candidate := 0; candidate <= 0xFF; candidate += 1 {
		bucket := uint8(candidate)
		if 0 <= message.EphBucketSeconds(bucket) {
			rungs = append(rungs, bucket)
			continue
		}
		offLadder = append(offLadder, bucket)
	}
	if len(rungs) == 0 || len(offLadder) == 0 {
		t.Fatalf("the eph ladder partitions no bucket byte at all (%d rungs, %d off the ladder), so this gate read nothing", len(rungs), len(offLadder))
	}
	if total := len(rungs) + len(offLadder); total != 256 {
		t.Fatalf("the partition covers %d of the 256 bucket bytes a uint8 holds", total)
	}

	named := []uint8{}
	answers := map[string]int{}
	for _, row := range rows {
		if !slices.Contains(named, row.bucket) {
			named = append(named, row.bucket)
		}
		switch row.refuse {
		case "":
			answers["a window"] += 1
		default:
			answers[row.refuse] += 1
		}
	}
	slices.Sort(named)

	// half one: every rung is exercised
	missingRungs := []uint8{}
	for _, rung := range rungs {
		if !slices.Contains(named, rung) {
			missingRungs = append(missingRungs, rung)
		}
	}
	if 0 < len(missingRungs) {
		t.Errorf("%s exercises %d of the ladder's %d rungs and names none of %v; every rung's divisor has to be pinned by a value or it is pinned by nothing",
			ephWindowKatPath, len(rungs)-len(missingRungs), len(rungs), missingRungs)
	}

	// half two: the complement, named, counted, asserted, and failing closed when empty
	namedOffLadder := []uint8{}
	complement := []uint8{}
	for _, bucket := range offLadder {
		if slices.Contains(named, bucket) {
			namedOffLadder = append(namedOffLadder, bucket)
			continue
		}
		complement = append(complement, bucket)
	}
	if len(complement) == 0 {
		t.Fatalf("the table names every one of the %d off-ladder bucket bytes, so this gate's complement is empty and it asserts nothing; if the table really did grow to cover all 256, delete this half rather than leave it green over nothing", len(offLadder))
	}
	if want := 256 - len(rungs) - len(namedOffLadder); len(complement) != want {
		t.Errorf("the complement holds %d bucket bytes and the arithmetic says %d (256 - %d rungs - %d named off-ladder)", len(complement), want, len(rungs), len(namedOffLadder))
	}
	t.Logf("%s names %d of the %d off-ladder bucket bytes (%v); the COMPLEMENT is the remaining %d: %v",
		ephWindowKatPath, len(namedOffLadder), len(offLadder), namedOffLadder, len(complement), complement)

	for _, bucket := range complement {
		for _, sentAtMs := range []int64{-1, 0, 1767225600000} {
			if measured := ephWindowKatAnswerOf(EphWindowAt(bucket, sentAtMs)); measured != ephWindowKatOffLadder {
				t.Errorf("bucket %d is in the table's complement and names no rung, and (bucket %d, sent_at_ms %d) answered %s rather than %s",
					bucket, bucket, sentAtMs, measured, ephWindowKatOffLadder)
			}
		}
	}

	// half three: all three answers are exercised. The order-of-refusal property is carried by
	// the refusal rows alone, and a table that kept only its arithmetic would still pass halves
	// one and two.
	for _, wanted := range []string{"a window", ephWindowKatOffLadder, ephWindowKatSentAt} {
		if answers[wanted] == 0 {
			t.Errorf("no row of %s answers %s; EphWindowAt is three answers over message.EphBucketSeconds's three, and a table missing one of them pins two thirds of it",
				ephWindowKatPath, wanted)
		}
	}
	t.Logf("answers: %d rows -- %d a window, %d %s, %d %s", len(rows),
		answers["a window"], answers[ephWindowKatOffLadder], ephWindowKatOffLadder, answers[ephWindowKatSentAt], ephWindowKatSentAt)
}

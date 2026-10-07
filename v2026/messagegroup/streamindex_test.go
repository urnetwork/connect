// The stream index reservation's CONTRACT, held against a file backed reserver that lives here
// and ships nowhere.
//
// Why the implementation under test is a test fake and not production source, restated where a
// reader of the tests meets it: spec A section 8.2 assigns the durable store to sdk's
// MessageStore, method for method, and neither half of the record layer imports an I/O package
// at all. A second durable implementation here would be the second implementation of one thing
// and would make the client half a storage engine. What this file owes instead is that every
// property of the interface is EXECUTABLE now rather than deferred to a package that does not
// exist -- so the fake is the crash injection harness section 5.6's own named test needs, and
// every assertion below is an obligation the unwritten sdk store plan inherits.
//
// The fake is deliberately built in two layers. streamIndexFake is the protocol -- the
// allocation, the ordering of the flush against the return, the high water -- and
// streamIndexDurable is the medium under it. That seam is what makes "Reserve returns only after the write is
// durable" observable in a go test at all: a real file cannot demonstrate it in process, because
// an unsynced write is still visible to a reader on the same machine. A restart here is a fresh
// streamIndexFake over the same medium, holding nothing the medium did not persist, which is the
// same thing a process death is to a store.
package messagegroup

import (
	"encoding/hex"
	"errors"
	"fmt"
	"go/ast"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// The medium a reserver's state survives a process death in.
type streamIndexDurable interface {
	load() (map[string]uint64, error)
	save(image map[string]uint64) error
}

// A real file, written and fsync'd, which is what section 5.6's "fsync'd or equivalent" names.
type streamIndexFileStore struct {
	path  string
	saves int
}

func (self *streamIndexFileStore) load() (map[string]uint64, error) {
	image := map[string]uint64{}
	raw, err := os.ReadFile(self.path)
	if errors.Is(err, os.ErrNotExist) {
		return image, nil
	}
	if err != nil {
		return nil, err
	}
	for _, line := range strings.Split(string(raw), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		index, err := strconv.ParseUint(fields[1], 10, 64)
		if err != nil {
			return nil, err
		}
		image[fields[0]] = index
	}
	return image, nil
}

func (self *streamIndexFileStore) save(image map[string]uint64) error {
	self.saves += 1
	text := strings.Builder{}
	for _, key := range slices.Sorted(maps.Keys(image)) {
		fmt.Fprintf(&text, "%s %d\n", key, image[key])
	}
	file, err := os.OpenFile(self.path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := file.WriteString(text.String()); err != nil {
		file.Close()
		return err
	}
	// the flush the contract is about. Everything above this line is a write a crash
	// discards; everything below it survives one.
	if err := file.Sync(); err != nil {
		file.Close()
		return err
	}
	return file.Close()
}

// The same medium without the file, for the properties that need ten thousand restarts rather
// than ten. It is durable in exactly the sense that matters here -- a fresh reserver over it
// holds what was saved and nothing else -- and it costs no fsync, which is what makes section
// 5.6's own named test runnable at its stated size.
type streamIndexImageStore struct {
	image map[string]uint64
	saves int
}

func (self *streamIndexImageStore) load() (map[string]uint64, error) {
	copied := map[string]uint64{}
	for key, index := range self.image {
		copied[key] = index
	}
	return copied, nil
}

func (self *streamIndexImageStore) save(image map[string]uint64) error {
	self.saves += 1
	copied := map[string]uint64{}
	for key, index := range image {
		copied[key] = index
	}
	self.image = copied
	return nil
}

// streamIndexFake is the protocol half: the allocation, the durable flush ordered before the
// return, and the high water a ratchet resumes from.
type streamIndexFake struct {
	lock    sync.Mutex
	durable streamIndexDurable
	image   map[string]uint64
	// the indices this instance has told a caller it may use. A reload that came back
	// behind one of these is the rewind ErrStreamIndexRewound names.
	handedOut map[string]uint64
	reserves  int
}

// A medium whose flush fails, which is the only way a test can stand between the write and its
// durability.
//
// IT WRAPS THE MEDIUM AND IS NOT A FIELD ON THE FAKE, and that position is the whole of what it
// is for. This injection used to be a saveErr field consulted just BEFORE the medium's save, and
// a mutation moved the fake's own commit -- image and handedOut -- in front of the save and
// SURVIVED: the early return fired above the moved lines, so a store that committed its state
// and then failed to flush looked exactly like a correct one. Standing at the medium instead
// puts the failure after anything the protocol half does, which is where a lost flush actually
// happens.
type streamIndexFailingMedium struct {
	under streamIndexDurable
	err   error
}

func (self *streamIndexFailingMedium) load() (map[string]uint64, error) { return self.under.load() }

func (self *streamIndexFailingMedium) save(image map[string]uint64) error {
	if self.err != nil {
		return self.err
	}
	return self.under.save(image)
}

func openStreamIndexFake(durable streamIndexDurable) (*streamIndexFake, error) {
	image, err := durable.load()
	if err != nil {
		return nil, err
	}
	return &streamIndexFake{durable: durable, image: image, handedOut: map[string]uint64{}}, nil
}

// A reserver over a fresh in-memory medium, for the tests that need a working sink and do not
// care what it is made of.
func newStreamIndexMemory() *streamIndexFake {
	fake, err := openStreamIndexFake(&streamIndexImageStore{image: map[string]uint64{}})
	if err != nil {
		panic(err)
	}
	return fake
}

// Reserve is the ALLOCATION ruling A1 requires, and the shape of this body is the argument for
// it: the read of the high water, the choice of the next index and the durable write are one
// critical section under one lock, so there is no window in which two callers can be handed the
// same number. The assert shape this replaced could not have that property, because the caller's
// choice happened outside the store.
func (self *streamIndexFake) Reserve(stream StreamKey) (uint64, error) {
	self.lock.Lock()
	defer self.lock.Unlock()
	self.reserves += 1
	key := streamIndexRowKey(stream)
	held := self.image[key]
	if held == ^uint64(0) {
		// the stream has spent the last index a u64 holds, and there is no next one. It is
		// ErrStreamIndexConsumed because from the caller's side it is the one permanent
		// answer this interface has: the store cannot allocate and never will again.
		return 0, fmt.Errorf("%w: the high water is %d and there is no successor", ErrStreamIndexConsumed, held)
	}
	index := held + 1
	proposed := map[string]uint64{}
	for existing, at := range self.image {
		proposed[existing] = at
	}
	proposed[key] = index
	if err := self.durable.save(proposed); err != nil {
		return 0, err
	}
	// AFTER the flush. Everything below this line is state a caller may rely on, and moving
	// any of these lines above the save is the whole defect this fake exists to make
	// observable: an index answered before it is durable is an index a crash re-issues.
	self.image = proposed
	self.handedOut[key] = index
	return index, nil
}

func (self *streamIndexFake) HighWater(stream StreamKey) (uint64, error) {
	self.lock.Lock()
	defer self.lock.Unlock()
	// total over the key space: a stream never seen is 0 and not an error, so highWater + 1
	// is a well defined start.
	return self.image[streamIndexRowKey(stream)], nil
}

// streamIndexRowKey flattens a StreamKey into the row identity a store would use. It is the
// fake's own choice and not the interface's: what the interface fixes is which stream a
// reservation belongs to, and a store is free to hash, concatenate or index the fields however
// it likes as long as two distinct streams are two distinct rows -- which is what
// TestTheReserverIsTotalOverItsKeySpaceAndSeparatesGroups holds it to.
//
// It is derived off the type rather than written as a list, which is what keeps ruling A1
// checkable HERE as well as in the production key: a field added to StreamKey and forgotten here
// would silently merge two streams into one row, and a field REMOVED -- which is what A1 did to
// the retention byte -- would leave this line naming something that no longer compiles. Both
// failures are the same one, and reflection over the fields is what makes them impossible to
// have quietly.
func streamIndexRowKey(stream StreamKey) string {
	value := reflect.ValueOf(stream)
	parts := make([]string, 0, value.NumField())
	for i := range value.NumField() {
		field := value.Field(i)
		if field.Kind() == reflect.Array {
			octets := make([]byte, field.Len())
			for at := range field.Len() {
				octets[at] = byte(field.Index(at).Uint())
			}
			parts = append(parts, hex.EncodeToString(octets))
			continue
		}
		// %#v rather than a kind switch, so a scalar field of a kind nobody anticipated
		// still lands in the row identity instead of being dropped out of it.
		parts = append(parts, fmt.Sprintf("%#v", field.Interface()))
	}
	return strings.Join(parts, "/")
}

// reload re-reads the medium, and refuses if it came back behind an index this instance has
// already handed out.
func (self *streamIndexFake) reload() error {
	self.lock.Lock()
	defer self.lock.Unlock()
	image, err := self.durable.load()
	if err != nil {
		return err
	}
	for key, handed := range self.handedOut {
		if image[key] < handed {
			return fmt.Errorf("%w: %s persisted %d and %d was handed out", ErrStreamIndexRewound, key, image[key], handed)
		}
	}
	self.image = image
	return nil
}

// A reserver that refuses everything, for the ordering property one file over.
type streamIndexRefusing struct {
	err      error
	reserves int
}

func (self *streamIndexRefusing) Reserve(stream StreamKey) (uint64, error) {
	self.reserves += 1
	return 0, self.err
}

func (self *streamIndexRefusing) HighWater(stream StreamKey) (uint64, error) { return 0, nil }

// A reserver whose HighWater fails, so a constructor that ignored the read is visible.
type streamIndexUnreadable struct{ err error }

func (self *streamIndexUnreadable) Reserve(stream StreamKey) (uint64, error) { return 1, nil }

func (self *streamIndexUnreadable) HighWater(stream StreamKey) (uint64, error) { return 0, self.err }

// A reserver that allocates whatever it is told to, so a ladder can be driven onto an index its
// own counter would never have produced. It exists for the two answers ruling A1 makes a ladder
// have to survive -- a store that went backwards under it, and one that jumped further ahead
// than the walk bound -- neither of which any honest allocation reaches.
type streamIndexScripted struct {
	answers  []uint64
	at       int
	reserves int
}

func (self *streamIndexScripted) Reserve(stream StreamKey) (uint64, error) {
	self.reserves += 1
	if len(self.answers) <= self.at {
		return 0, fmt.Errorf("the script has %d answers and this is call %d", len(self.answers), self.at+1)
	}
	answer := self.answers[self.at]
	self.at += 1
	return answer, nil
}

func (self *streamIndexScripted) HighWater(stream StreamKey) (uint64, error) { return 0, nil }

var streamIndexGroup = streamKeyNamed("grp-1")

// streamKeyNamed is one distinct stream per name, so a case that wants two streams says so
// rather than assembling a struct literal each time.
func streamKeyNamed(name string) StreamKey {
	stream := StreamKey{}
	copy(stream.GroupId[:], name)
	copy(stream.SenderHandle[:], name)
	return stream
}

// Property 1: Reserve returns only after the reservation survives a process death.
//
// The mechanism is an injected failure point BETWEEN the write and the flush, which is the only
// thing that can tell "returned" from "durable" -- a sleep cannot, and neither can reading the
// file back in the same process. Three claims, and the third is the one a swallowed flush error
// fails: the error reaches the caller, the medium holds nothing, and the index is still free
// afterwards, so the seal that was refused did not silently consume a nonce.
func TestReserveReturnsOnlyAfterTheReservationIsDurable(t *testing.T) {
	store := &streamIndexFileStore{path: filepath.Join(t.TempDir(), "stream.index")}
	medium := &streamIndexFailingMedium{under: store}
	fake, err := openStreamIndexFake(medium)
	if err != nil {
		t.Fatalf("open the reserver: %v", err)
	}
	// the happy path first, so the assertion below is about durability and not about the
	// fake being broken
	if index, err := fake.Reserve(streamIndexGroup); err != nil || index != 1 {
		t.Fatalf("the first allocation answered %d (%v), want 1", index, err)
	}
	restarted, err := openStreamIndexFake(&streamIndexFileStore{path: store.path})
	if err != nil {
		t.Fatalf("restart the reserver: %v", err)
	}
	if got, err := restarted.HighWater(streamIndexGroup); err != nil || got != 1 {
		t.Fatalf("after a restart the high water is %d (%v), want 1; Reserve returned before the write reached the medium", got, err)
	}
	// and now the failure point between the write and the flush
	injected := errors.New("the disk is full")
	medium.err = injected
	if index, err := fake.Reserve(streamIndexGroup); !errors.Is(err, injected) || index != 0 {
		t.Errorf("Reserve answered index %d and %v when the flush failed, want no index and the flush's own error; a swallowed flush error is a reservation that is not one",
			index, err)
	}
	afterFailure, err := openStreamIndexFake(&streamIndexFileStore{path: store.path})
	if err != nil {
		t.Fatalf("restart the reserver: %v", err)
	}
	if got, _ := afterFailure.HighWater(streamIndexGroup); got != 1 {
		t.Errorf("the medium holds high water %d after a failed flush, want 1", got)
	}
	// the counter did not move, so the seal that was refused consumed no nonce: the next
	// allocation is still 2 and not 3
	medium.err = nil
	if index, err := fake.Reserve(streamIndexGroup); err != nil || index != 2 {
		t.Errorf("the allocation after a failed flush answered %d (%v), want 2: a failed reservation must burn no index", index, err)
	}
	if 2 <= store.saves && store.saves != 2 {
		// two successful saves and no more: the failed one never reached the medium
		t.Errorf("the medium was written %d times for two successful reservations", store.saves)
	}
}

// Property 2: TestStreamIndexNeverReused, named by spec A section 5.9 G5 and G11.
//
// Section 5.6 states its shape: ten thousand seal operations with an injected crash after Reserve
// and before the AEAD, the session restarted from the persisted state, and no index ever produced
// twice. SealRecord does not exist until task 11 and the property is not about SealRecord -- it
// is about the reserver plus the restart -- so the crash is injected exactly where the seal would
// have been, and task 11 extends this same test to the real one.
//
// The medium is the in-memory image rather than the file, which is what makes ten thousand
// restarts affordable; TestReserveReturnsOnlyAfterTheReservationIsDurable is where the file and
// its fsync are held.
func TestStreamIndexNeverReused(t *testing.T) {
	const seals = 10000
	store := &streamIndexImageStore{image: map[string]uint64{}}
	produced := map[uint64]int{}
	for seal := 0; seal < seals; seal += 1 {
		// a fresh reserver over the persisted state: this IS the restart
		fake, err := openStreamIndexFake(store)
		if err != nil {
			t.Fatalf("restart %d: %v", seal, err)
		}
		// under ruling A1 the caller no longer chooses the number, so this loop does not
		// compute one: it asks, and the whole property is about what the store answers
		// across ten thousand process deaths.
		index, err := fake.Reserve(streamIndexGroup)
		if err != nil {
			t.Fatalf("allocate at restart %d: %v", seal, err)
		}
		if earlier, isRepeat := produced[index]; isRepeat {
			t.Fatalf("index %d was produced at seal %d and again at seal %d; a reused stream index is a reused nonce under a reused record key",
				index, earlier, seal)
		}
		produced[index] = seal
		// and here the process dies, after the reservation and before the aead. The next
		// iteration is what comes back.
	}
	if len(produced) != seals {
		t.Errorf("%d distinct indices came out of %d seals", len(produced), seals)
	}
	if got, _ := (&streamIndexFake{durable: store, image: store.image}).HighWater(streamIndexGroup); got != seals {
		t.Errorf("the persisted high water is %d after %d seals", got, seals)
	}
}

// Property 3: HighWater never rewinds, and a medium that came back behind an index already
// handed out is a refusal rather than a fresh start.
func TestHighWaterNeverRewinds(t *testing.T) {
	store := &streamIndexImageStore{image: map[string]uint64{}}
	fake, err := openStreamIndexFake(store)
	if err != nil {
		t.Fatalf("open the reserver: %v", err)
	}
	previous := uint64(0)
	for round := uint64(1); round <= 64; round += 1 {
		index, err := fake.Reserve(streamIndexGroup)
		if err != nil {
			t.Fatalf("allocate at round %d: %v", round, err)
		}
		if index != round {
			t.Fatalf("round %d allocated index %d; the allocation of a stream with no gaps is its round number", round, index)
		}
		got, err := fake.HighWater(streamIndexGroup)
		if err != nil {
			t.Fatalf("high water: %v", err)
		}
		if got < previous {
			t.Fatalf("the high water went from %d to %d", previous, got)
		}
		previous = got
		// a restart in the middle must not move it either
		if err := fake.reload(); err != nil {
			t.Fatalf("reload at %d: %v", index, err)
		}
		if got, _ := fake.HighWater(streamIndexGroup); got != previous {
			t.Fatalf("a reload moved the high water from %d to %d", previous, got)
		}
	}
	// and a medium that lost a flush is ErrStreamIndexRewound, not a fresh start: every
	// index above what it now holds is a nonce this device may already have used
	store.image[streamIndexRowKey(streamIndexGroup)] = 7
	if err := fake.reload(); !errors.Is(err, ErrStreamIndexRewound) {
		t.Errorf("a medium that came back at 7 after 64 was handed out answered %v, want ErrStreamIndexRewound", err)
	}
}

// Property 3 of the contract, restated for ruling A1: no index is ever handed out twice, and the
// permanent refusal is the store's own -- a stream with no successor left.
//
// WHY THIS CASE IS NOT THE ONE IT REPLACES, said here because a reader comparing the two will
// otherwise read a weakening. Wave 1's clause 3 was "reserving a consumed index answers
// ErrStreamIndexConsumed", and the case that held it reserved index 2 after index 5 to see the
// refusal. Under A1 there is no call that says which index, so that case could not be written at
// all -- what it protected has moved into the store, and what is checked here is the property
// rather than the refusal: every answer across every restart is distinct and strictly greater,
// and the one thing that CAN still be permanently refused -- a counter with no successor -- is
// still a typed sentinel and not a bool.
func TestNoStreamIndexIsEverAllocatedTwiceAndExhaustionIsTyped(t *testing.T) {
	fake := newStreamIndexMemory()
	seen := map[uint64]bool{}
	last := uint64(0)
	for round := 1; round <= 64; round += 1 {
		index, err := fake.Reserve(streamIndexGroup)
		if err != nil {
			t.Fatalf("round %d: %v", round, err)
		}
		if seen[index] {
			t.Fatalf("round %d was handed index %d a second time; a reused stream index is a reused nonce under a reused record key", round, index)
		}
		if index <= last && round != 1 {
			t.Errorf("round %d answered %d after %d; the counter is monotone", round, index, last)
		}
		seen[index] = true
		last = index
	}
	if got, _ := fake.HighWater(streamIndexGroup); got != last {
		t.Errorf("the high water is %d after the last allocation answered %d", got, last)
	}
	// and the one permanent refusal a store can make. A stream parked at the last index a u64
	// holds has no successor, so the allocation is a typed fatal error rather than a wrap:
	// wrapping re-issues every record key and every nonce this sender has used.
	spent := streamKeyNamed("a stream at the end of the counter")
	fake.image[streamIndexRowKey(spent)] = ^uint64(0)
	if index, err := fake.Reserve(spent); !errors.Is(err, ErrStreamIndexConsumed) || index != 0 {
		t.Errorf("a stream with no successor answered index %d and %v, want no index and ErrStreamIndexConsumed", index, err)
	}
	if got, _ := fake.HighWater(streamIndexGroup); got != last {
		t.Errorf("the refusal on one stream moved another stream's high water to %d, want %d", got, last)
	}
}

// Property 5: the store is total over its key space, and two groups do not share a counter.
func TestTheReserverIsTotalOverItsKeySpaceAndSeparatesGroups(t *testing.T) {
	fake := newStreamIndexMemory()
	for _, unseen := range []StreamKey{{}, streamKeyNamed("x"), streamKeyNamed("a stream nothing has written to")} {
		got, err := fake.HighWater(unseen)
		if err != nil {
			t.Errorf("HighWater of an unseen group answered %v; a group never seen is 0 with no error, so highWater + 1 is a well defined start", err)
		}
		if got != 0 {
			t.Errorf("HighWater of an unseen group is %d, want 0", got)
		}
	}
	left := streamKeyNamed("group-left")
	right := streamKeyNamed("group-right")
	for round := 1; round <= 4; round += 1 {
		if _, err := fake.Reserve(left); err != nil {
			t.Fatalf("allocate round %d for the left group: %v", round, err)
		}
	}
	if got, _ := fake.HighWater(right); got != 0 {
		t.Errorf("the right group's high water is %d after four reservations against the left group; the counter is per group and a shared one burns indices in one and re-issues them in the other", got)
	}
	if index, err := fake.Reserve(right); err != nil || index != 1 {
		t.Errorf("the right group's first allocation answered %d (%v), want 1: a group that has never sent starts at 1 however far another group has run", index, err)
	}
	if got, _ := fake.HighWater(left); got != 4 {
		t.Errorf("the left group's high water is %d after an allocation against the right group, want 4", got)
	}
	// EVERY FIELD OF THE KEY SEPARATES A ROW, derived off the type rather than written as the
	// two cases this type happens to have today. Ruling A1 says the counter is keyed by
	// (group_id, sender_handle), which is two claims and not one: two groups do not share a
	// counter, AND two senders of one group do not either. A row key that dropped a field --
	// or a field added later and forgotten -- merges two streams into one, and the merged one
	// hands the second stream indices the first has already used.
	streamKeyType := reflect.TypeOf(StreamKey{})
	if streamKeyType.NumField() == 0 {
		t.Fatal("StreamKey has no fields, so this half read nothing")
	}
	for i := range streamKeyType.NumField() {
		field := streamKeyType.Field(i)
		base := StreamKey{}
		apart := StreamKey{}
		differing := reflect.ValueOf(&apart).Elem().Field(i)
		if differing.Kind() != reflect.Array {
			// a scalar field: one is enough to tell it from zero
			differing.SetUint(1)
		} else {
			differing.Index(0).SetUint(1)
		}
		if base == apart {
			t.Fatalf("StreamKey.%s could not be made to differ, so this case cannot judge it", field.Name)
		}
		separate := newStreamIndexMemory()
		for round := 1; round <= 3; round += 1 {
			if _, err := separate.Reserve(base); err != nil {
				t.Fatalf("StreamKey.%s: allocate round %d on the base stream: %v", field.Name, round, err)
			}
		}
		index, err := separate.Reserve(apart)
		if err != nil {
			t.Fatalf("StreamKey.%s: allocate on the differing stream: %v", field.Name, err)
		}
		if index != 1 {
			t.Errorf("two streams differing only in StreamKey.%s share a counter: the second one's first allocation is %d and not 1, so it is being handed indices the first stream has already used",
				field.Name, index)
		}
	}
}

// The layering refusal, asserted rather than stated: nothing in this package's PRODUCTION source
// implements StreamIndexReserver.
//
// The class is derived from the interface's own method set rather than from a list of file names:
// any production declaration -- a type with both methods, or a function returning something that
// has them -- is a durable store this package has grown, and section 8.2 says the durable store
// is sdk's. imports_test.go holds the other half, over the production import set, because a store
// needs an I/O package before it needs a method name.
func TestNoProductionDeclarationOfThisPackageImplementsTheReserver(t *testing.T) {
	_, sources := messagegroupProductionSources(t)
	// the method set is read off the interface's own declaration, so a method added to
	// StreamIndexReserver widens this gate with nobody remembering to
	wanted := streamIndexReserverMethodNames(t, sources)
	if len(wanted) == 0 {
		t.Fatal("StreamIndexReserver declares no method in this package's source, so this gate looked for nothing")
	}
	methods := map[string]map[string]bool{}
	for _, source := range sources {
		for _, declaration := range source.parsed.Decls {
			function, isFunction := declaration.(*ast.FuncDecl)
			if !isFunction || function.Recv == nil || len(function.Recv.List) == 0 {
				continue
			}
			receiver := recordKeyReceiverTypeName(function.Recv.List[0].Type)
			if receiver == "" {
				continue
			}
			if methods[receiver] == nil {
				methods[receiver] = map[string]bool{}
			}
			methods[receiver][function.Name.Name] = true
		}
	}
	for receiver, has := range methods {
		complete := true
		for _, method := range wanted {
			if !has[method] {
				complete = false
				break
			}
		}
		if complete {
			t.Errorf("%s implements StreamIndexReserver in this package's production source; the durable store is section 8.2's MessageStore and a second one here is the second implementation of one thing",
				receiver)
		}
	}
}

func streamIndexReserverMethodNames(t *testing.T, sources []messagegroupSource) []string {
	t.Helper()
	for _, source := range sources {
		for _, declaration := range source.parsed.Decls {
			general, isGeneral := declaration.(*ast.GenDecl)
			if !isGeneral {
				continue
			}
			for _, spec := range general.Specs {
				typed, isTyped := spec.(*ast.TypeSpec)
				if !isTyped || typed.Name.Name != "StreamIndexReserver" {
					continue
				}
				declared, isInterface := typed.Type.(*ast.InterfaceType)
				if !isInterface || declared.Methods == nil {
					t.Fatal("StreamIndexReserver is declared and is not an interface")
				}
				names := []string{}
				for _, method := range declared.Methods.List {
					for _, name := range method.Names {
						names = append(names, name.Name)
					}
				}
				slices.Sort(names)
				return names
			}
		}
	}
	t.Fatal("this package declares no StreamIndexReserver")
	return nil
}

// The name a receiver expression hangs off, so a pointer receiver and a value receiver are one
// type.
func recordKeyReceiverTypeName(expr ast.Expr) string {
	switch typed := expr.(type) {
	case *ast.Ident:
		return typed.Name
	case *ast.StarExpr:
		return recordKeyReceiverTypeName(typed.X)
	case *ast.IndexExpr:
		return recordKeyReceiverTypeName(typed.X)
	}
	return ""
}

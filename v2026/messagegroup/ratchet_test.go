// The two ratchets: the ordering the sender's key depends on, the erasure section 5.5 names, and
// the receiver's window with the eviction that keeps it bounded across a whole table.
package messagegroup

import (
	"errors"
	"fmt"
	"go/ast"
	"reflect"
	"slices"
	"sync"
	"testing"
)

func ratchetClassKey() []byte {
	mlsSecret, pqSecret := keyScheduleKatInputs()
	return DeriveClassKeys(StorageRoot(mlsSecret, pqSecret)).Durable
}

const ratchetLeaf uint32 = 3

var ratchetGroup = streamKeyNamed("grp-7")

// The ladder as an independent walk, so every test below can say WHICH rung it expected rather
// than only that two calls agreed.
func ratchetLadder(t *testing.T, rungs int) [][]byte {
	t.Helper()
	classKey := ratchetClassKey()
	ladder := [][]byte{RecordKeyZero(classKey, ratchetLeaf)}
	for position := 1; position < rungs; position += 1 {
		ladder = append(ladder, RecordKeyNext(ladder[position-1]))
	}
	return ladder
}

// ---------------------------------------------------------------------------
// task 7, the sender ratchet
// ---------------------------------------------------------------------------

// Property 1, the behavioural half: a failed reservation produces no index and no key.
//
// It is the mechanism that survives a refactor the syntax check below would not recognise, and
// it is the one that refuses "call Reserve, ignore the error, derive the key anyway" -- a body
// that is reachability-identical to the correct one.
func TestNextProducesNothingWhenTheReservationFails(t *testing.T) {
	injected := errors.New("the disk is full")
	refusing := &streamIndexRefusing{err: injected}
	ratchet, err := NewSenderRatchet(ratchetClassKey(), ratchetLeaf, ratchetGroup, refusing)
	if err != nil {
		t.Fatalf("build the ratchet: %v", err)
	}
	before := append([]byte(nil), ratchet.recordKey...)
	index, key, err := ratchet.Next()
	if !errors.Is(err, injected) {
		t.Errorf("Next answered %v when the reservation failed, want the reserver's own error", err)
	}
	if key != nil {
		t.Errorf("Next answered a %d octet key when the reservation failed; the key must not exist before the reservation is durable", len(key))
	}
	if index != 0 {
		t.Errorf("Next answered index %d when the reservation failed", index)
	}
	if refusing.reserves != 1 {
		t.Errorf("Next reserved %d times, want 1", refusing.reserves)
	}
	// and the ratchet did not move, so a full disk is a retry rather than a hole
	if string(ratchet.recordKey) != string(before) {
		t.Error("a failed reservation advanced the ladder; the same index must be offered to the next call")
	}
	if ratchet.Position() != 1 {
		t.Errorf("a failed reservation moved the position to %d, want 1", ratchet.Position())
	}
}

// Property 1, the syntax half: the reservation's error is BOUND and returned on before anything
// of the key schedule is reached.
//
// The CLASS is derived -- every production declaration of this package that calls Reserve -- and
// not the one name this task added, so a second call site is judged without anybody extending
// this test. It is fatal on an empty class, because a matcher that stopped finding the call
// would clear the ordering property having read nothing.
//
// The SCOPE (R3a) is this directory. The reachability half of this property -- "every path from
// SealRecord to a RecordAeadHead or RecordAeadBody call reaches Reserve" -- is NOT here: that
// class has zero members until task 11 declares SealRecord, and a walk over it now would either
// fatal on arrival or pass having followed nothing. Task 11 owes it.
func TestEveryReservationIsCheckedBeforeTheKeyScheduleIsReached(t *testing.T) {
	_, sources := messagegroupProductionSources(t)
	// "the key schedule" is DERIVED as everything that reaches this package's one expansion or
	// its one extraction, transitively, and not as the declarations of a file called
	// keyschedule.go. The first version of this gate did the second, which is rule 5's own
	// second half -- a gate that derives its class and then enumerates its SCOPE is not a
	// derived gate -- and it would have read a derivation added in any other file as not being
	// part of the key schedule at all.
	schedule := recordKeyKdfReachingFunctions(sources)
	if len(schedule) < 3 {
		t.Fatalf("only %d declarations of this package reach the kdf, so the ordering below had almost nothing to be an ordering against", len(schedule))
	}
	reserving := []string{}
	for _, source := range sources {
		for _, declaration := range source.parsed.Decls {
			function, isFunction := declaration.(*ast.FuncDecl)
			if !isFunction || function.Body == nil {
				continue
			}
			if !slices.Contains(keyScheduleCalleeNames(function.Body), "Reserve") {
				continue
			}
			reserving = append(reserving, function.Name.Name)
			ratchetCheckReservationOrder(t, function, schedule)
		}
	}
	slices.Sort(reserving)
	if len(reserving) == 0 {
		t.Fatal("no production declaration of this package reserves a stream index, so this gate cleared the ordering the whole record aead's nonce uniqueness rests on")
	}
	if !slices.Equal(reserving, []string{"Next"}) {
		t.Logf("the reservation is made in %v; every one of them was held to the ordering", reserving)
	}
}

// The ordering, decided on one function body: the statement holding the Reserve call binds EVERY
// result it answers, the guard immediately after it returns, and no statement at or before it
// reaches the key schedule.
//
// THIS READING MOVED WITH RULING A1 AND IT MOVED WIDER, which is recorded here because a reader
// comparing it against the wave 1 version will otherwise read a loosening. Wave 1's Reserve
// answered one value, so the gate required literally one spelling -- the call in an if's Init,
// with exactly one name on the left. A1 makes Reserve an ALLOCATION answering (uint64, error), so
// that spelling is no longer the one a correct body has; keeping it would have been a control
// tuned to a signature rather than to a property, and the property is what this file is for.
//
// So the reading is now the property itself and it is STRICTER than what it replaced. Every
// result is required to be bound to a real name, which under wave 1 meant only the error and now
// means the INDEX as well -- and that clause is exactly the A1 defect a reader would otherwise
// have to catch by eye: a Next that discarded the store's index and handed out its own next
// number compiles, seals, and is undecryptable by every peer. Both spellings of the guard are
// accepted -- the call in an if's Init, and the call in an assignment whose very next statement
// is the guard -- because both make the reservation durable before the key exists, which is the
// whole of what section 5.6 asks for.
func ratchetCheckReservationOrder(t *testing.T, function *ast.FuncDecl, schedule map[string]bool) {
	t.Helper()
	reserveAt, scheduleAt := -1, -1
	for position, statement := range function.Body.List {
		if reserveAt < 0 && slices.Contains(keyScheduleCalleeNames(statement), "Reserve") {
			reserveAt = position
		}
		if scheduleAt < 0 {
			for _, callee := range keyScheduleCalleeNames(statement) {
				if schedule[callee] {
					scheduleAt = position
					break
				}
			}
		}
	}
	if reserveAt < 0 {
		t.Errorf("%s calls Reserve somewhere other than its own statement list, and this reading cannot order it", function.Name.Name)
		return
	}
	if 0 <= scheduleAt && scheduleAt <= reserveAt {
		t.Errorf("%s reaches the key schedule at statement %d and reserves at statement %d; the reservation must be durable BEFORE the key exists",
			function.Name.Name, scheduleAt, reserveAt)
	}
	// the two spellings, resolved to the same two things: the assignment that binds Reserve's
	// results, and the if that refuses on them.
	bound, guard := (*ast.AssignStmt)(nil), (*ast.IfStmt)(nil)
	switch statement := function.Body.List[reserveAt].(type) {
	case *ast.IfStmt:
		guard = statement
		bound, _ = statement.Init.(*ast.AssignStmt)
	case *ast.AssignStmt:
		bound = statement
		if reserveAt+1 < len(function.Body.List) {
			// the guard has to be the VERY NEXT statement. Anything between the
			// allocation and its refusal is work done on the strength of an error
			// nobody has looked at yet.
			guard, _ = function.Body.List[reserveAt+1].(*ast.IfStmt)
		}
	}
	if bound == nil {
		t.Errorf("%s does not bind what Reserve answers; a discarded result is a reservation that did not happen", function.Name.Name)
		return
	}
	if guard == nil {
		t.Errorf("%s binds the reservation and does not refuse on it in the next statement; section 5.6 says the seal refuses to proceed on error", function.Name.Name)
		return
	}
	// EVERY result, not only the error. Under A1 the index is the store's answer and a body
	// that dropped it would hand out a rung of its own choosing under a number the store
	// allocated to something else.
	for at, target := range bound.Lhs {
		name, isName := target.(*ast.Ident)
		if !isName || name.Name == "_" {
			t.Errorf("%s discards result %d of the reservation; under ruling A1 the store answers the index as well as the error and both are load bearing",
				function.Name.Name, at)
			return
		}
	}
	returns := false
	ast.Inspect(guard.Body, func(node ast.Node) bool {
		if _, isReturn := node.(*ast.ReturnStmt); isReturn {
			returns = true
		}
		return true
	})
	if !returns {
		t.Errorf("%s checks the reservation's error and carries on; section 5.6 says the seal refuses to proceed on error", function.Name.Name)
	}
}

// Property 2: TestRatchetZeroizes, named by section 5.5.
//
// The witness is a second slice header over the ratchet's OWN array, taken before the call and
// never handed to it, which is the struct field section 5.5 calls "entirely preventable". The
// caller's copy is a different array and holds the rung, so a zeroize of the returned slice --
// the other way to get this wrong -- fails here too.
func TestRatchetZeroizes(t *testing.T) {
	ladder := ratchetLadder(t, 4)
	ratchet, err := NewSenderRatchet(ratchetClassKey(), ratchetLeaf, ratchetGroup, newStreamIndexMemory())
	if err != nil {
		t.Fatalf("build the ratchet: %v", err)
	}
	for step := 0; step < 3; step += 1 {
		retained := ratchet.recordKey
		witness := retained[:len(retained):len(retained)]
		nonZero := 0
		for _, octet := range witness {
			if octet != 0 {
				nonZero += 1
			}
		}
		if nonZero == 0 {
			t.Fatalf("the ratchet is holding thirty two zeros before step %d, so this reading would pass against a helper that did nothing", step)
		}
		index, key, err := ratchet.Next()
		if err != nil {
			t.Fatalf("step %d: %v", step, err)
		}
		for i, octet := range witness {
			if octet != 0 {
				t.Fatalf("after Next the ratchet's own copy of record_key[%d] holds %#02x at index %d; section 5.5 asks for it to be overwritten before the return",
					index, octet, i)
			}
		}
		if len(key) != recordKeyBytes {
			t.Fatalf("Next answered a %d octet key", len(key))
		}
		allZero := true
		for _, octet := range key {
			if octet != 0 {
				allZero = false
			}
		}
		if allZero {
			t.Fatal("Next answered thirty two zeros; the erasure landed on the slice it handed out rather than on the one it retained")
		}
		if want := ladder[index]; string(key) != string(want) {
			t.Fatalf("Next answered %x at index %d and the ladder's rung there is %x", key, index, want)
		}
		if len(ratchet.recordKey) != 0 && &ratchet.recordKey[0] == &witness[0] {
			t.Fatal("the ratchet is holding the same array it erased")
		}
	}
	// and Zeroize erases the rung it is parked on
	parked := ratchet.recordKey
	witness := parked[:len(parked):len(parked)]
	ratchet.Zeroize()
	for i, octet := range witness {
		if octet != 0 {
			t.Errorf("after Zeroize the parked rung holds %#02x at index %d", octet, i)
		}
	}
}

// Property 3: the indices are consecutive from highWater + 1, never repeat, and survive a
// restart -- and the rung handed out at index i is the ladder's rung at i.
//
// The last clause is the pin the file comment argues: the ladder position IS the stream index,
// so a restart that resumed the ladder at zero while the index carried on would re-issue every
// key. It is the property the whole design decision exists for, and a restart is the only thing
// that can see it.
func TestTheSenderResumesAtTheHighWaterPlusOneAndNeverRepeatsARung(t *testing.T) {
	ladder := ratchetLadder(t, 16)
	store := &streamIndexImageStore{image: map[string]uint64{}}
	classKey := ratchetClassKey()
	seen := map[uint64]string{}
	for restart := 0; restart < 5; restart += 1 {
		reserver, err := openStreamIndexFake(store)
		if err != nil {
			t.Fatalf("restart %d: %v", restart, err)
		}
		ratchet, err := NewSenderRatchet(classKey, ratchetLeaf, ratchetGroup, reserver)
		if err != nil {
			t.Fatalf("restart %d: %v", restart, err)
		}
		for step := 0; step < 3; step += 1 {
			index, key, err := ratchet.Next()
			if err != nil {
				t.Fatalf("restart %d step %d: %v", restart, step, err)
			}
			if earlier, isRepeat := seen[index]; isRepeat {
				t.Fatalf("index %d came out twice; the first time it carried %s", index, earlier)
			}
			seen[index] = fmt.Sprintf("%x", key)
			if want := uint64(restart*3 + step + 1); index != want {
				t.Fatalf("restart %d step %d answered index %d, want %d; the resume is highWater + 1", restart, step, index, want)
			}
			if want := ladder[index]; string(key) != string(want) {
				t.Fatalf("index %d carried %x and the ladder's rung at that position is %x; the ladder position and the stream index have come apart, which is how a restart re-issues a nonce",
					index, key, want)
			}
		}
	}
	// index 0 is never handed out, which is the consequence of resuming at highWater + 1 for
	// a store that has never seen the group. It is stated here rather than left to be
	// rediscovered: record_key[0] is the ladder's root and seals nothing.
	if _, wasUsed := seen[0]; wasUsed {
		t.Error("index 0 was handed out; the resume rule makes the first index 1")
	}
}

// Property 4: two concurrent calls never hand out one index, and never one rung.
func TestConcurrentNextCallsNeverHandOutOneIndex(t *testing.T) {
	const workers = 8
	const each = 32
	ratchet, err := NewSenderRatchet(ratchetClassKey(), ratchetLeaf, ratchetGroup, newStreamIndexMemory())
	if err != nil {
		t.Fatalf("build the ratchet: %v", err)
	}
	type produced struct {
		index uint64
		key   string
	}
	answers := make(chan produced, workers*each)
	waiting := sync.WaitGroup{}
	for worker := 0; worker < workers; worker += 1 {
		waiting.Add(1)
		go func() {
			defer waiting.Done()
			for range each {
				index, key, err := ratchet.Next()
				if err != nil {
					answers <- produced{index: 0, key: err.Error()}
					continue
				}
				answers <- produced{index: index, key: string(key)}
			}
		}()
	}
	waiting.Wait()
	close(answers)
	indices := map[uint64]bool{}
	keys := map[string]uint64{}
	for answer := range answers {
		if indices[answer.index] {
			t.Fatalf("index %d was handed out twice", answer.index)
		}
		indices[answer.index] = true
		if earlier, isRepeat := keys[answer.key]; isRepeat {
			t.Fatalf("the rung handed out at index %d was handed out at index %d as well", answer.index, earlier)
		}
		keys[answer.key] = answer.index
	}
	if len(indices) != workers*each {
		t.Errorf("%d distinct indices came out of %d calls", len(indices), workers*each)
	}
	for index := uint64(1); index <= workers*each; index += 1 {
		if !indices[index] {
			t.Errorf("index %d is missing from a run that made %d consecutive reservations", index, workers*each)
		}
	}
}

// Property 5: exhaustion is a refusal and not a wrap.
//
// The ratchet is positioned at the last index a u64 holds by writing the field, because the only
// other way there is 2^64 reservations. That is a test reaching into its own package and not a
// production seam: nothing exported can set the position.
//
// THE STORE IS MOVED WITH IT, and that is ruling A1 rather than a convenience. Under wave 1 the
// ratchet chose the index, so writing the field was the whole of getting it to the end of the
// counter; under A1 the index is the STORE'S answer, and a ladder parked at the last index over a
// store still at zero is not an exhausted stream at all -- it is a store that went backwards
// under a live ladder, which is a different refusal this file holds one case over. So the store
// is parked one below the end, its allocation is the last index, and the ladder is standing on
// exactly it.
func TestTheSenderRefusesRatherThanWrappingAtTheEndOfTheCounter(t *testing.T) {
	reserver := newStreamIndexMemory()
	reserver.image[streamIndexRowKey(ratchetGroup)] = ^uint64(0) - 1
	ratchet, err := NewSenderRatchet(ratchetClassKey(), ratchetLeaf, ratchetGroup, reserver)
	if err == nil {
		t.Fatal("a ratchet resuming at the last index walked 2^64 rungs rather than refusing; the resume walk is bounded")
	}
	if !errors.Is(err, ErrLadderWalkTooLong) {
		t.Fatalf("a resume at the end of the counter answered %v, want ErrLadderWalkTooLong", err)
	}
	// so the ladder is built at the bottom and then WRITTEN to the end, which is the same
	// reach into the package the wave 1 case made and for the same reason.
	ratchet, err = NewSenderRatchet(ratchetClassKey(), ratchetLeaf, ratchetGroup, newStreamIndexMemory())
	if err != nil {
		t.Fatalf("build the ratchet: %v", err)
	}
	ratchet.position = ^uint64(0)
	ratchet.reserver = &streamIndexScripted{answers: []uint64{^uint64(0)}}
	index, key, err := ratchet.Next()
	if err != nil {
		t.Fatalf("the last index was refused: %v", err)
	}
	if index != ^uint64(0) || len(key) != recordKeyBytes {
		t.Fatalf("the last index came out as %d with a %d octet key", index, len(key))
	}
	if ratchet.position == 0 {
		t.Fatal("the counter wrapped to zero; every record key and every nonce this sender has used would be re-issued under a class key that has not moved")
	}
	for attempt := 0; attempt < 3; attempt += 1 {
		index, key, err := ratchet.Next()
		if !errors.Is(err, ErrSenderRatchetExhausted) {
			t.Errorf("attempt %d past the end answered %v, want ErrSenderRatchetExhausted", attempt, err)
		}
		if key != nil || index != 0 {
			t.Errorf("attempt %d past the end answered index %d and a %d octet key", attempt, index, len(key))
		}
	}
	// AND THE SAME AT THE END OF A WALK, which is the shape ruling A1 makes reachable and the
	// one the case above cannot see. A ladder no longer stands where the next index will be, so
	// a ladder five rungs below the end handed the last index by its store has to finish
	// standing ON that index: a body that walked the gap and left the position behind would
	// report a stream that has not ended and refuse the next call naming a rung it never
	// reached.
	walked, err := NewSenderRatchet(ratchetClassKey(), ratchetLeaf, ratchetGroup, newStreamIndexMemory())
	if err != nil {
		t.Fatalf("build the walking ratchet: %v", err)
	}
	walked.position = ^uint64(0) - 5
	walked.reserver = &streamIndexScripted{answers: []uint64{^uint64(0)}}
	walkedIndex, walkedKey, err := walked.Next()
	if err != nil {
		t.Fatalf("the last index at the end of a walk was refused: %v", err)
	}
	if walkedIndex != ^uint64(0) || len(walkedKey) != recordKeyBytes {
		t.Fatalf("the walk answered index %d with a %d octet key", walkedIndex, len(walkedKey))
	}
	zeroize(walkedKey)
	if walked.Position() != ^uint64(0) {
		t.Errorf("after walking to the last index the ladder stands at %d, want %d; a ladder that walked the gap and left its position behind reports a stream that has not ended",
			walked.Position(), ^uint64(0))
	}
	if _, _, err := walked.Next(); !errors.Is(err, ErrSenderRatchetExhausted) {
		t.Errorf("the call after the last index at the end of a walk answered %v, want ErrSenderRatchetExhausted", err)
	}

	// and a store whose high water is already the last index is refused at construction
	exhausted := newStreamIndexMemory()
	exhausted.image[streamIndexRowKey(ratchetGroup)] = ^uint64(0)
	if _, err := NewSenderRatchet(ratchetClassKey(), ratchetLeaf, ratchetGroup, exhausted); !errors.Is(err, ErrSenderRatchetExhausted) {
		t.Errorf("a ratchet resumed past the end of the counter answered %v, want ErrSenderRatchetExhausted", err)
	}
}

// The constructor's two refusals: no sink, and a sink that cannot be read.
func TestTheSenderRatchetRefusesAMissingOrUnreadableReserver(t *testing.T) {
	if _, err := NewSenderRatchet(ratchetClassKey(), ratchetLeaf, ratchetGroup, nil); !errors.Is(err, ErrNilStreamIndexReserver) {
		t.Errorf("a ratchet built with no reserver answered %v, want ErrNilStreamIndexReserver", err)
	}
	injected := errors.New("the store is corrupt")
	if _, err := NewSenderRatchet(ratchetClassKey(), ratchetLeaf, ratchetGroup, &streamIndexUnreadable{err: injected}); !errors.Is(err, injected) {
		t.Errorf("a ratchet whose high water could not be read answered %v, want the store's own error", err)
	}
}

// A caller that reuses its buffer must not move which row a ratchet's reservations land in.
//
// This used to be a behavioural case over a []byte group id: build a ratchet, overwrite the
// caller's array, and require the reservation to land under the original. StreamKey removed the
// hazard rather than fixing it -- every field is an array or a byte, so there is no reference for
// a caller to write through -- and the case is stated that way now, DERIVED off the type rather
// than written as a list of its fields, so a field added later that IS a reference is a failure
// here rather than a silent return of the aliasing this replaced.
//
// It is recorded as a replacement and not as a repair: the old case can no longer fail, and a
// case that cannot fail is the thing this project's first rule is about.
func TestNoFieldOfAStreamKeyIsSomethingACallerCanWriteThrough(t *testing.T) {
	streamKeyType := reflect.TypeOf(StreamKey{})
	if streamKeyType.NumField() == 0 {
		t.Fatal("StreamKey has no fields at all, so this gate read nothing")
	}
	for i := range streamKeyType.NumField() {
		field := streamKeyType.Field(i)
		switch field.Type.Kind() {
		case reflect.Array, reflect.Bool, reflect.Uint8, reflect.Uint16, reflect.Uint32,
			reflect.Uint64, reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32,
			reflect.Int64, reflect.String:
		default:
			t.Errorf("StreamKey.%s is a %s, which a caller goes on holding: a stream key that aliased its caller's storage would reserve indices against one row and use them against another",
				field.Name, field.Type.Kind())
		}
	}
	// and the type is comparable, which is what lets a store use it as a map key without a
	// second encoding of it. It is asserted by USING it as one: go refuses a map key type that
	// is not comparable at compile time, so this line is the assertion and a reflective
	// Comparable() would be a weaker restatement of it.
	rows := map[StreamKey]bool{streamKeyNamed("a"): true}
	if !rows[streamKeyNamed("a")] {
		t.Error("two equal stream keys did not answer one map row")
	}
	// the behavioural half the old case had: two distinct streams do not share a counter.
	reserver := newStreamIndexMemory()
	first, err := NewSenderRatchet(ratchetClassKey(), ratchetLeaf, streamKeyNamed("one"), reserver)
	if err != nil {
		t.Fatalf("build the first ratchet: %v", err)
	}
	second, err := NewSenderRatchet(ratchetClassKey(), ratchetLeaf, streamKeyNamed("two"), reserver)
	if err != nil {
		t.Fatalf("build the second ratchet: %v", err)
	}
	firstIndex, firstKey, err := first.Next()
	if err != nil {
		t.Fatalf("the first ratchet could not reserve: %v", err)
	}
	secondIndex, secondKey, err := second.Next()
	if err != nil {
		t.Fatalf("the second ratchet could not reserve: %v", err)
	}
	if firstIndex != 1 || secondIndex != 1 {
		t.Errorf("two distinct streams answered %d and %d, want 1 and 1: they are sharing a counter", firstIndex, secondIndex)
	}
	zeroize(firstKey)
	zeroize(secondKey)
}

// ---------------------------------------------------------------------------
// task 8, the receiver ratchet and the skipped key window
// ---------------------------------------------------------------------------

func ratchetReceiver(t *testing.T, headIndex uint64, windowSize int) *ReceiverRatchet {
	t.Helper()
	receiver, err := NewReceiverRatchet(ratchetClassKey(), ratchetLeaf, headIndex, windowSize)
	if err != nil {
		t.Fatalf("build the receiver: %v", err)
	}
	return receiver
}

// Property 1: an in-order receipt costs one step and retains nothing.
func TestAnInOrderReceiptAllocatesNoWindow(t *testing.T) {
	ladder := ratchetLadder(t, 64)
	receiver := ratchetReceiver(t, 0, DefaultRecordWindowSize)
	for index := uint64(0); index < 32; index += 1 {
		key, err := receiver.KeyFor(index)
		if err != nil {
			t.Fatalf("index %d: %v", index, err)
		}
		if string(key) != string(ladder[index]) {
			t.Fatalf("index %d answered %x and the ladder's rung there is %x", index, key, ladder[index])
		}
		if receiver.window != nil {
			t.Fatalf("the window was allocated by an in-order receipt at index %d and holds %d entries", index, len(receiver.window))
		}
	}
}

// Property 2: a gap within the window is fillable, once.
//
// Every skipped rung between the head and the request is derived and retained; a second request
// for a retained index answers it and drops it; a third refuses. A window that hands the same key
// out twice is a window that survives a replay.
func TestAGapInsideTheWindowIsFillableExactlyOnce(t *testing.T) {
	ladder := ratchetLadder(t, 64)
	receiver := ratchetReceiver(t, 0, DefaultRecordWindowSize)
	key, err := receiver.KeyFor(10)
	if err != nil {
		t.Fatalf("the first receipt was out of order and was refused: %v", err)
	}
	if string(key) != string(ladder[10]) {
		t.Fatalf("index 10 answered %x, want %x", key, ladder[10])
	}
	if len(receiver.window) != 10 {
		t.Fatalf("the window holds %d rungs after a jump of ten, want 10", len(receiver.window))
	}
	for index := uint64(0); index < 10; index += 1 {
		retained, err := receiver.KeyFor(index)
		if err != nil {
			t.Fatalf("retained index %d: %v", index, err)
		}
		if string(retained) != string(ladder[index]) {
			t.Errorf("retained index %d answered %x, want %x", index, retained, ladder[index])
		}
		// and a second request for the same index is refused: the window handed it over
		// and no longer holds it
		if again, err := receiver.KeyFor(index); !errors.Is(err, ErrOutOfWindow) {
			t.Errorf("retained index %d answered a %d octet key a second time (%v); a window that answers twice survives a replay", index, len(again), err)
		}
	}
	if len(receiver.window) != 0 {
		t.Errorf("the window holds %d rungs after every one of them was answered", len(receiver.window))
	}
}

// Property 3: beyond the window is a refusal, and the head does NOT move.
//
// The contrast with connect/mls's peekFor is deliberate and is argued in ratchet.go: mls may
// advance on a refusal because the generation reached it through an AEAD open under
// sender_data_secret, and this layer has no such gate -- the index is in the record's cleartext
// header. A head that moved on a forged index would burn every rung between here and there.
func TestBeyondTheWindowIsARefusalThatDoesNotMoveTheHead(t *testing.T) {
	const window = 16
	receiver := ratchetReceiver(t, 0, window)
	head := receiver.head
	parked := append([]byte(nil), receiver.secret...)
	for _, index := range []uint64{window + 1, window + 2, 1 << 20, ^uint64(0)} {
		key, err := receiver.KeyFor(index)
		if !errors.Is(err, ErrOutOfWindow) {
			t.Errorf("index %d answered %v, want ErrOutOfWindow", index, err)
		}
		if key != nil {
			t.Errorf("index %d answered a %d octet key", index, len(key))
		}
		if receiver.head != head {
			t.Fatalf("a refusal moved the head from %d to %d", head, receiver.head)
		}
		if string(receiver.secret) != string(parked) {
			t.Fatal("a refusal advanced the ladder")
		}
		if len(receiver.window) != 0 {
			t.Fatalf("a refusal retained %d rungs", len(receiver.window))
		}
	}
	// the last index INSIDE the window is answered, so the bound is the window and not one
	// less than it
	if _, err := receiver.KeyFor(window); err != nil {
		t.Errorf("index %d, which is exactly the window ahead of the head, was refused: %v", window, err)
	}
	// and an index below the head that the window does not HOLD is refused with the same
	// sentinel, without moving anything. It takes a fresh ratchet: the jump above filled the
	// window, and a retained index is answered rather than refused, which is property 2.
	fresh := ratchetReceiver(t, 0, window)
	for index := uint64(0); index < 2; index += 1 {
		if _, err := fresh.KeyFor(index); err != nil {
			t.Fatalf("in-order index %d: %v", index, err)
		}
	}
	if len(fresh.window) != 0 {
		t.Fatalf("two in-order receipts retained %d rungs, so the reading below is about a retained index rather than a passed one", len(fresh.window))
	}
	head = fresh.head
	if key, err := fresh.KeyFor(0); !errors.Is(err, ErrOutOfWindow) {
		t.Errorf("an index below the head answered a %d octet key (%v), want ErrOutOfWindow", len(key), err)
	}
	if fresh.head != head {
		t.Errorf("a refusal below the head moved it from %d to %d", head, fresh.head)
	}
}

// Property 6: every rung that leaves the window without being handed to a caller is zeroized.
//
// Three sites, and the assertion is over the ACTUAL arrays: a witness is taken over each retained
// rung before the eviction and read afterwards. A bare delete leaves live record keys wherever
// the allocator puts them next, and nothing this ratchet still reaches can see the difference.
func TestEveryRungThatLeavesTheWindowUnansweredIsZeroized(t *testing.T) {
	const window = 4
	// The forward walk one call may make is bounded by the window, so the per ratchet bound
	// is reached over TWO calls and not one: the first retains four rungs, the second
	// retains four more and the prune takes the four oldest.
	receiver := ratchetReceiver(t, 0, window)
	if _, err := receiver.KeyFor(window); err != nil {
		t.Fatalf("index %d: %v", window, err)
	}
	if len(receiver.window) != window {
		t.Fatalf("the window holds %d rungs after the first jump, want %d", len(receiver.window), window)
	}
	// witnesses over the arrays that are about to be evicted, taken before the call that
	// evicts them and never handed to it
	witnesses := map[uint64][]byte{}
	for index, secret := range receiver.window {
		witnesses[index] = secret[:len(secret):len(secret)]
	}
	if _, err := receiver.KeyFor(receiver.head + uint64(window)); err != nil {
		t.Fatalf("the second jump: %v", err)
	}
	if len(receiver.window) != window {
		t.Fatalf("the window holds %d rungs after the second jump, want %d", len(receiver.window), window)
	}
	evicted := 0
	for index, witness := range witnesses {
		if _, isRetained := receiver.window[index]; isRetained {
			continue
		}
		evicted += 1
		for i, octet := range witness {
			if octet != 0 {
				t.Fatalf("the evicted rung at index %d holds %#02x at offset %d; a bare delete leaves a live record key behind", index, octet, i)
			}
		}
	}
	if evicted != window {
		t.Fatalf("%d rungs were evicted by the second jump, want %d", evicted, window)
	}
	// and an evicted index is gone rather than answerable
	if key, err := receiver.KeyFor(0); !errors.Is(err, ErrOutOfWindow) {
		t.Errorf("an evicted index answered a %d octet key (%v), want ErrOutOfWindow", len(key), err)
	}
	// and Zeroize erases the parked rung and every remaining window entry
	parked := receiver.secret[:len(receiver.secret):len(receiver.secret)]
	remaining := map[uint64][]byte{}
	for index, secret := range receiver.window {
		remaining[index] = secret[:len(secret):len(secret)]
	}
	if len(remaining) == 0 {
		t.Fatal("the window is empty before Zeroize, so this reading would pass against a Zeroize that did nothing")
	}
	receiver.Zeroize()
	for i, octet := range parked {
		if octet != 0 {
			t.Errorf("the parked rung holds %#02x at offset %d after Zeroize", octet, i)
		}
	}
	for index, witness := range remaining {
		for i, octet := range witness {
			if octet != 0 {
				t.Errorf("the retained rung at index %d holds %#02x at offset %d after Zeroize", index, octet, i)
			}
		}
	}
	if len(receiver.window) != 0 {
		t.Errorf("Zeroize left %d entries in the window", len(receiver.window))
	}
}

// The two ratchets agree: what the sender hands out at index i is what the receiver answers for
// index i, in order and out of it.
func TestTheSenderAndTheReceiverAgreeOnEveryRung(t *testing.T) {
	sender, err := NewSenderRatchet(ratchetClassKey(), ratchetLeaf, ratchetGroup, newStreamIndexMemory())
	if err != nil {
		t.Fatalf("build the sender: %v", err)
	}
	sent := map[uint64][]byte{}
	for step := 0; step < 24; step += 1 {
		index, key, err := sender.Next()
		if err != nil {
			t.Fatalf("step %d: %v", step, err)
		}
		sent[index] = key
	}
	// the receiver starts where the sender's first index is, which is the join the file
	// comment says nobody supplies today
	receiver := ratchetReceiver(t, 1, DefaultRecordWindowSize)
	order := []uint64{1, 5, 4, 2, 3, 24, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16, 17, 18, 19, 20, 21, 22, 23}
	for _, index := range order {
		got, err := receiver.KeyFor(index)
		if err != nil {
			t.Fatalf("receiving index %d: %v", index, err)
		}
		if string(got) != string(sent[index]) {
			t.Fatalf("the receiver answered %x for index %d and the sender handed out %x", got, index, sent[index])
		}
	}
	if len(receiver.window) != 0 {
		t.Errorf("the window holds %d rungs after every index was received", len(receiver.window))
	}
}

// The receiver's constructor refuses a window that is not a window.
func TestTheReceiverRefusesAWindowThatIsNotOne(t *testing.T) {
	for _, size := range []int{0, -1, -1024} {
		if _, err := NewReceiverRatchet(ratchetClassKey(), ratchetLeaf, 0, size); !errors.Is(err, ErrWindowSize) {
			t.Errorf("a window of %d answered %v, want ErrWindowSize", size, err)
		}
	}
	if _, err := NewReceiverRatchets(0); !errors.Is(err, ErrWindowSize) {
		t.Errorf("a table with a retained bound of zero answered %v, want ErrWindowSize", err)
	}
}

// Property 4: the global bound holds regardless of how many senders there are.
//
// The SCOPE question (R3a): the bound is asserted over the WHOLE table, computed from the
// structure rather than from one ratchet, because the number of senders is not the receiver's
// choice and a per-ratchet bound multiplied by a number somebody else picks is not a bound.
func TestTheRetainedBoundHoldsOverTheWholeTableWhateverTheSenderCount(t *testing.T) {
	const bound = 24
	const window = 16
	for _, senders := range []int{1, 2, 8, 32} {
		table, err := NewReceiverRatchets(bound)
		if err != nil {
			t.Fatalf("build the table: %v", err)
		}
		keys := []ReceiverRatchetKey{}
		for sender := 0; sender < senders; sender += 1 {
			key := ReceiverRatchetKey{RetentionWire: 0x01}
			key.SenderHandle[0] = byte(sender)
			key.SenderHandle[15] = byte(sender)
			keys = append(keys, key)
			ratchet, err := NewReceiverRatchet(ratchetClassKey(), uint32(sender), 0, window)
			if err != nil {
				t.Fatalf("build a ratchet: %v", err)
			}
			table.Track(key, ratchet)
		}
		// every sender skips as far ahead as its own window allows, which is the shape a
		// flood takes
		for round := 0; round < 4; round += 1 {
			for _, key := range keys {
				if _, err := table.KeyFor(key, uint64((round+1)*window)); err != nil {
					t.Fatalf("%d senders, round %d: %v", senders, round, err)
				}
				if retained := table.Retained(); bound < retained {
					t.Fatalf("%d senders retained %d rungs, and the bound is %d", senders, retained, bound)
				}
			}
		}
		if retained := table.Retained(); bound < retained {
			t.Errorf("%d senders retained %d rungs, and the bound is %d", senders, retained, bound)
		}
	}
}

// Property 5: the eviction takes from the FULLEST window, so a member holding a handful of
// skipped rungs never pays for a member holding a thousand.
//
// Section 5.5's own rule -- evict the oldest SENDER -- starves whoever went quiet, which is the
// member most likely to need the window. This is connect/mls's policy, adopted as open item
// M1-12's labelled recommendation.
func TestTheEvictionTakesFromTheFullestWindowAndNotTheQuietestSender(t *testing.T) {
	const bound = 12
	table, err := NewReceiverRatchets(bound)
	if err != nil {
		t.Fatalf("build the table: %v", err)
	}
	quiet := ReceiverRatchetKey{RetentionWire: 0x01}
	quiet.SenderHandle[0] = 0x01
	flooding := ReceiverRatchetKey{RetentionWire: 0x01}
	flooding.SenderHandle[0] = 0x02
	for _, row := range []struct {
		key  ReceiverRatchetKey
		leaf uint32
	}{{key: quiet, leaf: 1}, {key: flooding, leaf: 2}} {
		ratchet, err := NewReceiverRatchet(ratchetClassKey(), row.leaf, 0, 64)
		if err != nil {
			t.Fatalf("build a ratchet: %v", err)
		}
		table.Track(row.key, ratchet)
	}
	// the quiet member is holding two skipped rungs
	if _, err := table.KeyFor(quiet, 2); err != nil {
		t.Fatalf("the quiet member: %v", err)
	}
	if got := table.ratchets[quiet].Retained(); got != 2 {
		t.Fatalf("the quiet member retained %d rungs, want 2", got)
	}
	// and now the flooding member pushes the table over its bound, several times
	for round := 1; round <= 4; round += 1 {
		if _, err := table.KeyFor(flooding, uint64(round*20)); err != nil {
			t.Fatalf("the flooding member, round %d: %v", round, err)
		}
		if retained := table.Retained(); bound < retained {
			t.Fatalf("round %d retained %d rungs over a bound of %d", round, retained, bound)
		}
		if got := table.ratchets[quiet].Retained(); got != 2 {
			t.Fatalf("round %d took %d of the quiet member's two rungs; the eviction must land on the fullest window",
				round, 2-got)
		}
	}
	// and the quiet member's rungs are still answerable, which is the whole point of the
	// policy
	for _, index := range []uint64{0, 1} {
		if _, err := table.KeyFor(quiet, index); err != nil {
			t.Errorf("the quiet member's retained index %d was evicted by another sender's flood: %v", index, err)
		}
	}
}

// A sender the table has never been told about is a refusal and not an empty key, and the table's
// own erasure reaches the ratchets it drops.
func TestTheTableRefusesAnUntrackedSenderAndErasesWhatItReplaces(t *testing.T) {
	table, err := NewReceiverRatchets(DefaultRetainedRecordKeys)
	if err != nil {
		t.Fatalf("build the table: %v", err)
	}
	key := ReceiverRatchetKey{RetentionWire: 0x01}
	if _, err := table.KeyFor(key, 0); !errors.Is(err, ErrNoReceiverRatchet) {
		t.Errorf("an untracked sender answered %v, want ErrNoReceiverRatchet", err)
	}
	first := ratchetReceiver(t, 0, 8)
	table.Track(key, first)
	if _, err := table.KeyFor(key, 3); err != nil {
		t.Fatalf("a tracked sender: %v", err)
	}
	witnesses := [][]byte{first.secret[:len(first.secret):len(first.secret)]}
	for _, secret := range first.window {
		witnesses = append(witnesses, secret[:len(secret):len(secret)])
	}
	table.Track(key, ratchetReceiver(t, 0, 8))
	for which, witness := range witnesses {
		for i, octet := range witness {
			if octet != 0 {
				t.Errorf("the replaced ratchet's rung %d holds %#02x at offset %d; a ratchet replaced at an epoch change is holding the previous epoch's keys", which, octet, i)
			}
		}
	}
	// and the table's own Zeroize reaches what it holds
	second := table.ratchets[key]
	if _, err := table.KeyFor(key, 2); err != nil {
		t.Fatalf("the replacement: %v", err)
	}
	parked := second.secret[:len(second.secret):len(second.secret)]
	table.Zeroize()
	for i, octet := range parked {
		if octet != 0 {
			t.Errorf("the table's Zeroize left %#02x at offset %d", octet, i)
		}
	}
	if _, err := table.KeyFor(key, 5); !errors.Is(err, ErrNoReceiverRatchet) {
		t.Errorf("the table still answers after Zeroize: %v", err)
	}
}

// The forward walk one call may make is bounded by the window, which is what makes an
// unauthenticated index safe to read at all.
//
// A peer that picks an index out of the air can cost this receiver at most windowSize expansions
// and windowSize retained rungs before it is refused, and the refusal leaves the head where it
// was so the cost is not cumulative.
func TestOneCallFillsAtMostTheWindow(t *testing.T) {
	const window = 8
	receiver := ratchetReceiver(t, 0, window)
	if _, err := receiver.KeyFor(window); err != nil {
		t.Fatalf("index %d: %v", window, err)
	}
	if window < len(receiver.window) {
		t.Errorf("one call retained %d rungs over a window of %d", len(receiver.window), window)
	}
	if _, err := receiver.KeyFor(receiver.head + uint64(window) + 1); !errors.Is(err, ErrOutOfWindow) {
		t.Error("a jump of one more than the window was accepted")
	}
}

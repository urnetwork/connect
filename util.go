package connect

import (
	"context"
	"os"
	"os/signal"
	"slices"
	"sync"
	"syscall"
	"time"
	// "fmt"
	// "runtime/debug"
	// "strings"
	// "encoding/json"
	// "reflect"
	mathrand "math/rand"
)

type Monitor struct {
	mutex  sync.Mutex
	notify chan struct{}
}

func NewMonitor() *Monitor {
	return &Monitor{
		notify: make(chan struct{}),
	}
}

func (self *Monitor) NotifyChannel() chan struct{} {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	return self.notify
}

func (self *Monitor) NotifyAll() chan struct{} {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	close(self.notify)
	self.notify = make(chan struct{})
	return self.notify
}

// MonitorValue pairs a value with its change notification so the two halves of
// the monitor contract cannot be separated. A bare `Monitor` leaves both halves
// to the caller — every mutation must remember to notify, and every consumer
// must subscribe immediately before its read — and both are silent when missed:
// forgetting to notify parks the waiter forever, subscribing across a blocking
// wait re-wakes it on state it already read. `MonitorValue` makes neither
// representable. The value can only be read together with a channel armed at the
// instant of the read (`Get`), and can only be written through `Set`/`Update`,
// which notify inside the same critical section.
//
// `Set` notifies only when the value actually changes. That is what lets a
// consumer that both reads and writes the value — an election loop that picks
// the mode it also watches — converge instead of waking itself forever.
//
// Methods are safe for concurrent use. `Update`'s function runs under the lock,
// so it must not call back into this value.
type MonitorValue[T comparable] struct {
	stateLock sync.Mutex
	value     T
	notify    chan struct{}
}

func NewMonitorValue[T comparable](value T) *MonitorValue[T] {
	return &MonitorValue[T]{
		value:  value,
		notify: make(chan struct{}),
	}
}

// Get returns the current value and a channel that closes on the next change.
// The pair is taken atomically, so no change can slip between the read and the
// subscribe. Act on the value, then wait on the channel — never wait in between.
func (self *MonitorValue[T]) Get() (T, chan struct{}) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.value, self.notify
}

// Value returns the current value without subscribing. Use it only where no wait
// follows; anything that waits for a change must use `Get`.
func (self *MonitorValue[T]) Value() T {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.value
}

// Set stores the value, notifying the waiters when it changed. It reports
// whether it changed.
func (self *MonitorValue[T]) Set(value T) bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.setWithLock(value)
}

// Update applies f to the current value and stores the result, notifying the
// waiters when it changed. It reports whether it changed. f runs under the lock.
func (self *MonitorValue[T]) Update(f func(T) T) bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.setWithLock(f(self.value))
}

func (self *MonitorValue[T]) setWithLock(value T) bool {
	if self.value == value {
		return false
	}
	self.value = value
	close(self.notify)
	self.notify = make(chan struct{})
	return true
}

// memoryShedders are callbacks that drop recoverable caches (resolver caches, pooled
// connections, affinity maps) so a memory-constrained host can shed before it approaches
// its process memory limit.
var memoryShedders = NewCallbackList[func()]()

// AddMemoryShedder registers a callback invoked by ShedMemory. It returns an unregister
// closure; an owner must unregister its shedder when it closes.
func AddMemoryShedder(shed func()) func() {
	callbackId := memoryShedders.Add(shed)
	return func() {
		memoryShedders.Remove(callbackId)
	}
}

// ShedMemory invokes the registered shedders. The host calls this on its memory pressure
// signal (e.g. the iOS network extension pressure warning) to drop recoverable caches.
func ShedMemory() {
	for _, shed := range memoryShedders.Get() {
		HandleError(shed)
	}
}

// makes a copy of the list on update
type CallbackList[T any] struct {
	mutex sync.Mutex
	// `callbacks` and `callbackIds` are parallel arrays.
	// `callbackIds` is kept sorted ascending because `Add` always appends
	// `nextCallbackId` which is monotonically increasing. `Remove` relies on
	// this invariant to use `slices.BinarySearch`.
	callbacks      []T
	callbackIds    []int
	nextCallbackId int
}

func NewCallbackList[T any]() *CallbackList[T] {
	return &CallbackList[T]{
		callbacks:      []T{},
		callbackIds:    []int{},
		nextCallbackId: 0,
	}
}

func (self *CallbackList[T]) Get() []T {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	return self.callbacks
}

func (self *CallbackList[T]) Add(callback T) int {
	self.mutex.Lock()
	defer self.mutex.Unlock()

	callbackId := self.nextCallbackId
	self.nextCallbackId += 1

	nextCallbacks := slices.Clone(self.callbacks)
	nextCallbacks = append(nextCallbacks, callback)
	self.callbacks = nextCallbacks

	nextCallbackIds := slices.Clone(self.callbackIds)
	nextCallbackIds = append(nextCallbackIds, callbackId)
	self.callbackIds = nextCallbackIds

	return callbackId
}

func (self *CallbackList[T]) Remove(callbackId int) {
	self.mutex.Lock()
	defer self.mutex.Unlock()

	i, found := slices.BinarySearch(self.callbackIds, callbackId)
	if !found {
		// not present
		return
	}

	nextCallbacks := slices.Clone(self.callbacks)
	nextCallbacks = slices.Delete(nextCallbacks, i, i+1)
	self.callbacks = nextCallbacks

	nextCallbackIds := slices.Clone(self.callbackIds)
	nextCallbackIds = slices.Delete(nextCallbackIds, i, i+1)
	self.callbackIds = nextCallbackIds
}

// this coordinates and idle shutdown when the shutdown and adding to the work channel are on separate goroutines
type IdleCondition struct {
	mutex           *sync.Mutex
	condition       *sync.Cond
	modId           uint64
	updateOpenCount int
	closed          bool
}

func NewIdleCondition() *IdleCondition {
	mutex := &sync.Mutex{}
	condition := sync.NewCond(mutex)
	return &IdleCondition{
		mutex:           mutex,
		condition:       condition,
		modId:           0,
		updateOpenCount: 0,
		closed:          false,
	}
}

func (self *IdleCondition) Checkpoint() uint64 {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	return self.modId
}

func (self *IdleCondition) Close(checkpointId uint64) bool {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	if self.modId != checkpointId {
		return false
	}
	if 0 < self.updateOpenCount {
		return false
	}
	self.closed = true
	return true
}

func (self *IdleCondition) WaitForClose() bool {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	for 0 < self.updateOpenCount {
		self.condition.Wait()
	}
	self.closed = true
	return true
}

func (self *IdleCondition) UpdateOpen() bool {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	if self.closed {
		return false
	}
	self.modId += 1
	self.updateOpenCount += 1
	return true
}

func (self *IdleCondition) UpdateClose() {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	if self.updateOpenCount <= 0 {
		panic("IdleCondition.UpdateClose underflow: called more times than UpdateOpen")
	}
	self.updateOpenCount -= 1
	self.condition.Signal()
}

func MinTime(a time.Time, bs ...time.Time) time.Time {
	min := a
	for _, b := range bs {
		if b.Before(min) {
			min = b
		}
	}
	return min
}

type Event struct {
	ctx    context.Context
	cancel context.CancelFunc
}

func NewEvent() *Event {
	return NewEventWithContext(context.Background())
}

func NewEventWithContext(ctx context.Context) *Event {
	cancelCtx, cancel := context.WithCancel(ctx)
	return &Event{
		ctx:    cancelCtx,
		cancel: cancel,
	}
}

func (self *Event) Ctx() context.Context {
	return self.ctx
}

func (self *Event) Set() {
	self.cancel()
}

func (self *Event) IsSet() bool {
	select {
	case <-self.ctx.Done():
		return true
	default:
		return false
	}
}

func (self *Event) WaitForSet(timeout time.Duration) bool {
	select {
	case <-self.ctx.Done():
		return true
	case <-time.After(timeout):
		return false
	}
}

// SetOnSignals registers the Event to be Set when any of signalValues is
// received. The returned cleanup function unregisters the handler.
//
// Side effect: if the spawned watcher goroutine panics, it will call Set()
// as part of its panic recovery. Callers should be aware that any
// unexpected failure in the signal-watcher will fire the Event, since the
// safest assumption on panic is that we should treat the program as
// shutting down.
func (self *Event) SetOnSignals(signalValues ...syscall.Signal) func() {
	stopSignal := make(chan os.Signal, len(signalValues))
	for _, signalValue := range signalValues {
		signal.Notify(stopSignal, signalValue)
	}
	go HandleError(func() {
		for {
			select {
			case _, ok := <-stopSignal:
				if !ok {
					return
				}
				self.Set()
			}
		}
	}, func() {
		signal.Stop(stopSignal)
		close(stopSignal)
		self.Set()
	})
	return func() {
		signal.Stop(stopSignal)
		close(stopSignal)
	}
}

func WeightedShuffle[T comparable](values []T, weights map[T]float32) {
	WeightedShuffleWithEntropy[T](values, weights, float32(0))
}

func WeightedShuffleWithEntropy[T comparable](values []T, weights map[T]float32, entropy float32) {
	n := len(values)

	mathrand.Shuffle(n, func(i int, j int) {
		values[i], values[j] = values[j], values[i]
	})

	netRemaining := float32(0)
	for j := 0; j < n; j += 1 {
		netRemaining += weights[values[j]]
	}

	for i := 0; i < n-1; i += 1 {
		j := func() int {
			r := mathrand.Float32()
			rnet := r * netRemaining
			net := entropy * netRemaining
			for j := i; j < n; j += 1 {
				w := weights[values[j]]
				net += w
				if rnet < net {
					netRemaining -= w
					return j
				}
			}
			// zero weights, use the last value
			return n - 1
		}()
		values[i], values[j] = values[j], values[i]
	}
}

func WeightedShuffleFunc[T any](values []T, weight func(T) float32) {
	WeightedShuffleFuncWithEntropy[T](values, weight, float32(0))
}

func WeightedShuffleFuncWithEntropy[T any](values []T, weight func(T) float32, entropy float32) {
	n := len(values)

	mathrand.Shuffle(n, func(i int, j int) {
		values[i], values[j] = values[j], values[i]
	})

	netRemaining := float32(0)
	for j := 0; j < n; j += 1 {
		netRemaining += weight(values[j])
	}

	for i := 0; i < n-1; i += 1 {
		j := func() int {
			r := mathrand.Float32()
			rnet := r * netRemaining
			net := entropy * netRemaining
			for j := i; j < n; j += 1 {
				w := weight(values[j])
				net += w
				if rnet < net {
					netRemaining -= w
					return j
				}
			}
			// zero weights, use the last value
			return n - 1
		}()
		values[i], values[j] = values[j], values[i]
	}
}

func WeightedSelectFunc[T any](values []T, n int, weight func(T) float32) {
	WeightedSelectFuncWithEntropy[T](values, n, weight, float32(0))
}

// puts the result at the front of values
func WeightedSelectFuncWithEntropy[T any](values []T, n int, weight func(T) float32, entropy float32) {
	n = min(n, len(values))

	netRemaining := float32(0)
	for j := 0; j < len(values); j += 1 {
		netRemaining += weight(values[j])
	}

	for i := 0; i < n; i += 1 {
		j := func() int {
			r := mathrand.Float32()
			rnet := r * netRemaining
			net := entropy * netRemaining
			j := i + (mathrand.Intn(len(values)-i) % (len(values) - i))
			for c := 0; c < len(values)-i; c += 1 {
				w := weight(values[j])
				net += w
				if rnet < net {
					netRemaining -= w
					return j
				}
				// shuffle iteration
				j = i + (mathrand.Intn(len(values)-i) % (len(values) - i))
			}
			// zero weights, use the last value
			return j
		}()
		values[i], values[j] = values[j], values[i]
	}
}

type Reconnect struct {
	startTime  time.Time
	minTimeout time.Duration
}

func NewReconnect(minTimeout time.Duration) *Reconnect {
	return &Reconnect{
		startTime:  time.Now(),
		minTimeout: minTimeout,
	}
}

func (self *Reconnect) After() <-chan time.Time {
	timeout := self.minTimeout - time.Now().Sub(self.startTime)
	if timeout <= 0 {
		c := make(chan time.Time)
		close(c)
		return c
	} else {
		randomTimeout := time.Duration(mathrand.Int63n(int64(timeout)))
		return time.After(randomTimeout)
	}
}

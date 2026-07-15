# Go Style Guide

In the URnetwork code, the following Go style is used. A few conventions — notably the `self` receiver name and the verbose usage+type field naming — intentionally diverge from idiomatic Go; they are deliberate house rules, not oversights.

## Naming

- Our receiver name is `self`: `func (self *T) f(...)` (pointer receiver) or `func (self T) f(...)` (value receiver).
- Our canonical name for a `sync.Mutex` guarding state is `stateLock`.
- Field and variable names are slightly more verbose than standard Go, so usage and type can be inferred from the name. The scheme is usage + type:
  - Maps: `map[K]V` is `usageKVs` — e.g. `activeNetworkIdUsers` for a `map[Id]User` of active users. Note the plural `s`.
  - Slices: `[]T` is `usageTs` — e.g. `activeNetworkIds` for a `[]Id` of active networks. Note the plural `s`.
  - Variables: `T` is `usageT` — e.g. `activeNetworkId` for the active network `Id`.
  - Use the shortest type token that stays unambiguous: `Id`, not `Identifier`; but never abbreviate past recognition (`activeNetworkIds`, not `activeNetIds`).
  - When the value already implies its type, drop the type token: concrete objects are named by usage alone (`session`, `client`, `transferBalance`). Keep the type when dropping it would confuse an identifier with the object it names — an `Id` stays `clientId`, never `client`.
  - Short names like `t`, `s`, `ts` are fine in a small scope with few locals, where the type is obvious from nearby context.

## Identifiers

- Our `Id` type is a ULID. ULIDs put a millisecond creation timestamp in their high bits, so ids are **lexicographically ordered by creation time** — and we rely on this in several places: `MAX(id)` / `MAX(id::varchar)` selects the most-recently-created row of a group, and `ORDER BY id` stands in for `ORDER BY create_time`. The ordering survives the `uuid` column type and its `::varchar` canonical form (the timestamp is in the leading bytes, and hex sorts in value order), so it holds in SQL as well as in Go.
- The timestamp is **client-local**: an id carries the clock of whatever minted it, so id order is a dependable time order within a single source but is not a global clock across sources (two ids minted on different machines can be out of true order by the clock skew between them). When strict or cross-source ordering matters, order by an explicit server-set timestamp column instead.

## Package layering

**A package must never import its own subpackages.** Dependencies point one way:
a subpackage may import its parent or a peer, never a child. This is the same rule
the Go standard library follows.

The reason is that a parent importing a child inverts the dependency: the child is
supposed to be a detail of the parent, but the compiler now treats the parent as
depending on it, so the child's API is frozen by its own parent, cycles become easy
to create, and "what depends on what" stops being readable from the directory tree.

If a parent and a child need to share code, that code belongs in the **parent**, not
in a child the parent then imports. Do not reach for `internal/` to get around this
— `internal/` controls *visibility*, not *direction*, and a parent importing
`internal/x` is still a parent importing a child.

Shared code that several files in one package need is just a file in that package.
Name it for what it is, and prefix the files of a cohesive group so the grouping
survives the flattening:

```
proxy/
  relay.go            <- the shared bidirectional copy, used by http and socks
  http.go
  socks.go            <- the public SocksProxy
  socks5_server.go    <- the socks5 protocol implementation, one package,
  socks5_addr.go         grouped by filename prefix rather than by directory
  socks5_associate.go
  socks/main.go       <- a CHILD (package main) importing its parent: allowed
```

An identifier that no longer crosses a package boundary should be unexported. A
name is exported to cross a boundary, not as decoration; collapsing a subpackage
into its parent is a good moment to shrink the public surface back to what callers
actually use.

## Comments

- Prefer short inline comments (`// ...` inside a function), 1–2 lines per point unless more is truly needed.
- Put a doc comment at the top of each file, type, and function/method declaration, summarizing the architecture and edge cases. These can be as long as needed, but aim to be concise. Push information that applies throughout a type or file up to the type or file header instead of repeating it — in particular, concurrent goroutine safety.

## Concurrency and goroutine safety

- By default, package-level functions are assumed safe for concurrent use, and a type's methods are assumed NOT safe unless the type documents otherwise.
- Hold a lock across the smallest scope that needs it. The idiom is an immediately-invoked closure: `func() { self.stateLock.Lock(); defer self.stateLock.Unlock(); ... }()`.
- Every potentially infinite loop must take a context (for cancellation) and rate-limit itself (to avoid busy-spinning).
- Use `connect.Reconnect` for reconnect rate limiting.
- Prefer `connect.MonitorValue[T]` over a bare `Monitor` whenever the thing being watched is a value. It pairs the value with its notification so neither half of the contract below can be broken: `Get` hands back the value and a channel armed at the same instant, and `Set`/`Update` notify inside the same critical section (and only on an actual change, so a loop that elects the value it also watches converges instead of waking itself forever). Reach for a bare `Monitor` only when the state is not a single comparable value.
- A `Monitor` has two halves, and both fail silently. **Every mutation of the monitored state must notify**, and **every consumer must subscribe immediately before its read**. A mutation that forgets to notify parks its waiters forever — a monitor with no `NotifyAll` at all is dead on arrival and nothing will tell you.
- Notify from inside the same locked scope as the mutation, so the state change and its notification cannot be separated: `func() { self.stateLock.Lock(); defer self.stateLock.Unlock(); if self.x == x { return }; self.x = x; self.monitor.NotifyAll() }()`. Gate the notify on an actual change — an unconditional notify lets a loop that also writes the state wake itself in a cycle.
- `NotifyAll` closes the live channel and swaps in a fresh one, so a channel from `NotifyChannel` is a one-shot edge: once it fires it stays fired until you re-subscribe. A loop that waits on a channel it never re-subscribes to will hot spin the moment it takes a wake without exiting.
- Subscribe *immediately* before reading the monitored state — `update := monitor.NotifyChannel()` — then `select` on it while waiting for changes. When the state is lock-guarded, subscribe and read in the same locked scope.
- Nothing may block between the subscribe and the read. Both orderings fail silently, in opposite directions:
  - subscribing *after* the read loses an update: a change in between closes the channel that was already discarded, so the loop stalls until an unrelated change arrives.
  - subscribing well *before* the read duplicates work: a change in that gap is already carried by the read, and it also closed the channel that was subscribed, so the next iteration fires immediately and redoes the same work on identical state.
- The monitor delivers edges, but a read of the current state delivers a level. Any change that lands before the read is already in the result, so re-arming for it does not "avoid missing" it — it double counts it. Keep the gap between subscribe and read empty and the two agree.
- A coalescing emitter — at most one emit per epoch, carrying the complete state — waits for the change, sleeps the epoch, and only *then* subscribes and reads/emits. Subscribing before the epoch sleep is the duplicate case above: the burst's changes are already in the snapshot, and they leave the next round armed, so an identical emit fires one epoch later. See `DeviceLocal.watchNetworkPeers` in the sdk for the reference shape.

## Goroutine lifecycle

- Start a type's internal management goroutines in its constructor, so the returned object is already fully running. The lifecycle loop is conventionally `func (self *T) run()` — lowercase, internal, started by the constructor. When a type has a single internal lifecycle/maintenance loop (e.g. one goroutine that periodically evicts TTL state), name it `run`; give specific names only when a type has several distinct long-lived loops.
- Exception: when an external manager must clean up after the lifecycle, expose `func (self *T) Run()` (uppercase) instead. The manager calls `Run()` after construction and tears the object down when `Run()` returns. Casing carries the meaning: lowercase `run()` is internal and self-started; uppercase `Run()` is externally driven.
- Wait with `time.After` inside the run loop by default; we don't reach for `time.Timer` for the convenience of it.
- Exception — hot-path timer reuse: in a per-packet (or otherwise hot) loop, where profiling shows the per-iteration `time.After` allocation is a significant share of allocations, reuse a single `time.Timer` instead. Create it with `time.NewTimer(0)` before the loop, `defer timer.Stop()`, and `timer.Reset(d)` immediately before each blocking `select` that reads `timer.C`. This relies on go1.23+ timer semantics, where `Reset` guarantees no stale fire is delivered afterward, so the drain dance is unnecessary and the initial already-fired state is harmless. Reach for this only with a profile that justifies it, not preemptively.

## Logging

- Components log through a `Logger` (see `log.go`), not the global glog functions.
- Guard every verbose log statement that takes format arguments — `if self.log.V(2).Enabled() { self.log.Infof("[tag]...", a, b) }`, never a bare `self.log.V(2).Infof("[tag]...", a, b)`. The variadic arguments (and any `fmt` / `.String()` work among them) are boxed into an `[]any` at the call site *before* the level is checked: Go's variadic + interface-dispatch semantics defeat escape analysis, so a disabled level still heap-allocates on every call. The `Enabled()` guard skips all of it. This matters most on per-packet paths but is the rule everywhere, for consistency.
- Unconditional `Infof` / `Warningf` / `Errorf` (no `V(n)`) always emit, so they need no guard; a verbose call with no arguments boxes nothing and needs none either.

## Formatting and structure

- Format with `gofmt`.
- Inline single-use helper logic as a local closure at its call site (`f := func(...){ ... }; f()`) rather than pulling it out into a separate named function.

## Tests

- Each test is a top-level `func TestXxx(t *testing.T)`. Normal (positive) tests do not use `t.Run` subtests: if cases are logically separate, write separate top-level tests; if they are homogeneous variations of one thing, use a plain table loop (`for _, c := range cases { ... }`) reporting with `t.Errorf`/`t.Fatalf`.
- `t.Run` is appropriate only when the subtest boundary itself is the point — notably when a test deliberately runs a subtest that is expected to FAIL and asserts that failure. The subtest isolates and captures the failure so the parent can check it.

## Struct creation

- When initializing structs, always use explicit field names — `T{field: value}`, not `T{value}`.

## Capitalization

- In comments we do not use all upper case. If there is an important concept, use a concise name in lowercase for readability. Use plain words for names where possible e.g. "identity companion"

## Locking

- Functions that are expected to be called with one or more state locks should be named "*WithLock". Inversely, functions that do not have "*WithLock" should expect to be called with no state locks.
- Operations on locked state should be as tightly scoped as possible.
- Calls to external objects must not hold a state lock. This is generally an implication of the "WithLock" rule.
- Locks must always be acquired in consistent order. 
- Do not use re-entrant locks because they will mask locking issues.

## Message Pool

Pool buffers (`MessagePoolGet`) have a single owner that is responsible for returning them (`MessagePoolReturn`). Ownership moves by these rules:

1. **A successful send takes ownership.** When a buffer is handed to a sender and the send returns success, the sender now owns the buffer and is responsible for returning it.
2. **An unsuccessful send leaves ownership with the caller.** If the send returns not-success, the caller still owns the buffer and must return it (or reuse/retry it).
3. **A callback buffer is borrowed, valid only for the call.** A buffer passed to a callback is owned by the caller and is only valid for the duration of that call. To use it beyond the callback (e.g. to forward it on a channel or hand it to another goroutine), the callback must take a shared copy with `MessagePoolShareReadOnly` and pass that copy on; ownership of the copy then follows the send rules above.

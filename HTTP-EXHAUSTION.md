# Owned HTTP exhaustion causes

`HttpParallel`, `HttpSerial`, `WsDialContextWithDialer` and `H1DialContextWithDialer` previously returned an untyped `Timeout.` after their strategy budget ended, discarding the earlier physical read/connection errors. Callers could neither identify an expected outage nor detect a hard failure joined to it.

Each request/upgrade operation now owns a bounded cause collector. Immediate request/dial failures and deferred selected-body read failures are recorded at their actual boundaries; unselected bodies retain their existing cancellation/cleanup ownership. At exhaustion, `HttpRequestExhaustedError.Unwrap` exposes original observed leaves plus actual caller/strategy cancellation or the exhausted private deadline. Existing budgets, route selection, retry pacing, successful responses and WebSocket/H1 evaluation remain unchanged. Terminal H1 authorization refusals retain their existing immediate result and do not change route health; exhausted evaluations never fabricate a winning dialer.

Repeated transport classes retain one original representative each. Unknown/local hard leaves retain at most 32 originals and an explicit hard overflow sentinel. Repetition never grows an error journal, and overflow never resembles a pure transient timeout. Consumers still must classify every leaf; the new type does not itself grant retry authority. No diagnostic-text matching is needed.

The cumulative successor also bounds inspection itself to 256 nodes and 64
levels. Foreign `Unwrap`, `Timeout`, `Temporary`, and `Context.Err` methods run
outside the collector lock, so reentry cannot deadlock the owner. Iterative
inspection does not copy an unbounded joined vector; a cyclic/deep/wide or
truncated tree retains a hard ambiguity marker. Only classified copied leaves
enter the bounded storage mutation. These changes need separate normal/race
and original-body causal qualification; the previously retained 33-root scope
and its H1 descendants do not by themselves qualify this successor.

Qualification is pending. Five new deterministic roots cover actual HTTP body interruption, local request rebuild failure joined with EOF, cancellation, finite repeated-cause storage, and actual WS/H1 socket failure. They expire the operation context only after a real failed attempt is retained, without shortened read budgets. Existing serial/parallel body replay, cancellation/worker join, selected-body cleanup and HTTP limit and H1 terminal authorization roots remain affected qualification. Author checks are compile-only; no live API behavior has been changed.

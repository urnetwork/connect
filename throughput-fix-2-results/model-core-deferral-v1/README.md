# Explicit quality-change performance deferral

The user authorized deferring legitimate connection-quality-change tests until
`NetworkQualityChanged` is implemented and called at the modeled change.
The `model-core` mode preserves 27 of the historical 30 selected tests and
excludes exactly these three propagation-transition performance tests:

- `TestWindowPathAckTailRoundTripGrowthControl`
- `TestWindowPathServiceRoundTripChanges`
- `TestWindowPathServiceRoundTripGrowthBeyondOldRing`

The original `model` mode still selects all 30 tests. No Go test body or
numeric threshold changed. FIFO instrument, static-path, window-mismatch,
capacity-change/congestion and ownership coverage stay selected.

Terra medium verified shell syntax, the source selection, and the manifest's
recording of the deferral content and hash. This is a runner audit; no model
execution or passing performance result is claimed by this artifact.

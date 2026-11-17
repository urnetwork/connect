# Historical branch source, retained 2026-10-04

These deterministic gzip files contain the original patch series for each
patch-unique audited branch. `manifest.json` binds branch heads, commit lists,
compressed bytes and decompressed bytes. Inspect with `gzip -dc FILE.patch.gz`.
Binary payloads are not duplicated: their exact Git objects remain reachable
through the merge parents. These files are research/history, not compiled code,
new runtime activation, or a claim that an old experiment passed current gates.

The per-branch dispositions in `docs/integration/audited-branches-20261004.md`
identify current owners and distinct unresolved experiments. Do not apply an
archived patch wholesale to the current source tree.

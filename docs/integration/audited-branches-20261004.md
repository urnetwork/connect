# Audited branch integration, 2026-10-04

Base: origin/main ad6fc3a191a3d777b9a18aae63a03c4554c95dd6.
This integration retains every audited parent. Current wire, encryption, transfer,
WebRTC lifecycle, memory ownership and dependency policies remain authoritative.
Old experimental deltas that cannot safely replace these owners are retained as
explicit historical patches, with their superseding owner or unresolved boundary
listed below. They are not compiled or activated by this merge.

## Dispositions

- `fix/checkpoint-report-id-20261003` (`ded1ebd68f51`): every nonmerge patch is already equivalent on the base; retain current implementations and merge ancestry.
- `fix/memory-owner-ledger-20261003` (`15d6c8b9cbee`): every nonmerge patch is already equivalent on the base; retain current implementations and merge ancestry.
- `fix/memory-owner-ledger-emitter-20261003` (`6bca0432528d`): every nonmerge patch is already equivalent on the base; retain current implementations and merge ancestry.
- `fix/transfer-ack-owner-memory-20261003` (`d788cb4a8dfa`): every nonmerge patch is already equivalent on the base; retain current implementations and merge ancestry.
- `origin/codex/r46-exit-gap-root-20260926` (`e1fe2daf4841`): every nonmerge patch is already equivalent on the base; retain current implementations and merge ancestry.

- `origin/add-nix-flake-and-single-mod` (`2ac69d2ce392`): The single root module and in-tree protocol are already the current layout. Preserve current Go1.26/module graph; the old Go/Nix shell and wholesale old-layout formatting are historical, not a replacement build authority. Source: `research/historical-branches-20261004/add-nix-flake-and-single-mod.patch.gz`.

- `origin/add-peer-to-peer-connections` (`f7f897021be0`): Historical webrtc-conn/Pion-v3 API, five-second wrapper and deleted connect/ layout are retained. Current Pion-v4 transport_p2p_webrtc.go owns authenticated signaling, admission and lifecycle; the prototype is not selected. Exact historical source: `research/historical-branches-20261004/add-peer-to-peer-connections.patch.gz`.

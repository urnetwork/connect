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

- `origin/api-network-user` (`9067c85a7983`): Current API already contains both network-user routes with current GetNetworkUserResult and UpdateNetworkName schemas matching server handlers. Preserve those schemas over the obsolete field shapes. Exact historical source: `research/historical-branches-20261004/api-network-user.patch.gz`.

- `origin/codex/astra-r43-transport-recovery-20260924` (`3b7eca941491`): Current contractControlNeedsPlaintext, epoch-qualified optimistic delivery and contract-only writer pin preserve the repair. All three rekey, failed-rekey and optimistic-generation test files exactly match the branch. Keep newer transfer ownership. Exact historical source: `research/historical-branches-20261004/codex--astra-r43-transport-recovery-20260924.patch.gz`.

- `origin/codex/astra-receive-gap-ordering-20260925` (`3cc6ef2a89ca`): Receive-gap ordering and the rekey generation controls already exist. Preserve current sequence and lifecycle code; retain this older reviewed implementation in history. Exact historical source: `research/historical-branches-20261004/codex--astra-receive-gap-ordering-20260925.patch.gz`.

- `origin/diagrams` (`1a74f4b96021`): Original architecture diagrams retained as a dated historical document; their simplified ownership and old paths are not current runtime authority. Exact historical source: `research/historical-branches-20261004/diagrams.patch.gz`.

- `origin/extender-fixes` (`b8184cc9df66`): Old UDP extender source retained as research. Current net_extender_datagram/network/strategy/verification implementations own current cancellation and security. Deleted connect/ package files are not restored into the build. Exact historical source: `research/historical-branches-20261004/extender-fixes.patch.gz`.

- `origin/find-providers-tune1` (`4dd156de516d`): Old provider CLI/MIPS/mininit experiment retained. Current byte-count formatting/parsing and TLS configuration already exist with newer shared-certificate/session ownership. Removed provider CLI, generated IP lists, pool initialization and dependency versions are not resurrected. Exact historical source: `research/historical-branches-20261004/find-providers-tune1.patch.gz`.

- `origin/geode-api` (`bde5b739ece0`): Proposed earnings API retained as historical draft; it is not represented as current deployed endpoint behavior. Current API schema and generator workflow remain authoritative. Exact historical source: `research/historical-branches-20261004/geode-api.patch.gz`.

- `origin/gvisor-test` (`419aa07b516e`): 2024 source-map/netstack experiment retained. It does not replace current LocalUserNat or qualify an alternative to the missing current sibling gvisor fork. Current module pin and replacement are unchanged. Exact historical source: `research/historical-branches-20261004/gvisor-test.patch.gz`.

- `origin/inspect` (`26952d2f927a`): Draft inspection protocol retained with the exploratory analysis source. Current generated production wire types remain unchanged. Exact historical source: `research/historical-branches-20261004/inspect.patch.gz`.

- `origin/inspect-occurrence-data` (`06769d0068f8`): All31 distinct exploratory analysis commits are retained as source patches and merge parents. The study expects its old generated protocol tree and separate clustering dependencies; it is not activated. Original binary captures stay reachable in original Git objects, without duplication into active builds. Exact historical source: `research/historical-branches-20261004/inspect-occurrence-data.patch.gz`.

- `origin/merge-protocol` (`e748904c83fd`): Current root module already owns generated protocol and protocol/Makefile. Preserve current schemas, generated code, CI and dependencies instead of older import/generator rewrites. Exact historical source: `research/historical-branches-20261004/merge-protocol.patch.gz`.

- `origin/multi-key` (`ef3ec3a5d6fb`): Distinct destination-affinity experiment retained explicitly as research, not claimed equivalent. Key sharing, idle-owner changes and removed RST behavior require separate lifecycle qualification before runtime selection. Exact historical source: `research/historical-branches-20261004/multi-key.patch.gz`.

- `origin/net-fixes` (`ff6769d600d1`): Inspection draft and older HTTP changes retained. Current net_http transport/DNS/cancellation implementations supersede the deleted connect/net_http.go layout; no production wire change. Exact historical source: `research/historical-branches-20261004/net-fixes.patch.gz`.

- `origin/p2p-stream` (`c220330a05e5`): Historical P2P-only timeout design note retained. Current measured P2P settings and lifecycle bounds remain unchanged. Exact historical source: `research/historical-branches-20261004/p2p-stream.patch.gz`.

- `origin/privacytxt-01` (`935d89f39edc`): Privacytxt API draft and 2024 port rules retained as historical source. Current ip_security owners and regression controls remain authoritative; no destination or protocol rule is weakened. Exact historical source: `research/historical-branches-20261004/privacytxt-01.patch.gz`.

- `origin/provider-mips-fixes` (`896e5f639c42`): Later provider MIPS/debug variant retained with earlier tuning history. Current TLS, pool and parser implementations remain; removed provider executable and mininit behavior are not reinstated. Exact historical source: `research/historical-branches-20261004/provider-mips-fixes.patch.gz`.

- `origin/split` (`b85960c6a563`): Dated split-tunnel product design retained as historical documentation, without claiming every proposal is selected in current clients. Exact historical source: `research/historical-branches-20261004/split.patch.gz`.
## Equivalent ref aliases

These additional audited names are retained through the same merged source parents.
Each has no patch-unique commit relative to the integration base.

- `fix/transfer-owner-snapshot-fence-20261003` (`f29b679b7a88`): current patch-equivalent implementation retained; head is an ancestor of this integration.
- `origin/fix/transfer-ack-owner-memory-20261003` (`d788cb4a8dfa`): current patch-equivalent implementation retained; head is an ancestor of this integration.
- `origin/fp2-close-report-identity-emission-20261003` (`fd4388de8f8c`): current patch-equivalent implementation retained; head is an ancestor of this integration.
- `origin/fp2-close-report-owner-ledger-20261003` (`6bca0432528d`): current patch-equivalent implementation retained; head is an ancestor of this integration.
- `origin/fp2-close-report-wire-20261003` (`ded1ebd68f51`): current patch-equivalent implementation retained; head is an ancestor of this integration.
- `origin/fp2-transfer-owner-snapshot-fence-20261003` (`f29b679b7a88`): current patch-equivalent implementation retained; head is an ancestor of this integration.
- `origin/fp2-transport-owner-ledger-only-20261003` (`15d6c8b9cbee`): current patch-equivalent implementation retained; head is an ancestor of this integration.

## Validation and limits

All 31 audited ref heads are ancestors of this integration. The active source,
module files, and generated protocol bytes are identical to the fresh main base;
only integration documentation and historical source archives are added. The
focused close-report protocol race controls pass. A full current module build is
unqualified because the required sibling gVisor checkout is unavailable.
Historical feature drafts are preserved, not represented as enabled runtime features.

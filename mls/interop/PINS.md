<!-- connect/mls/interop/PINS.md -->
# Pinned external references

This is the one pin file for the whole slice. Bumping any line here is a pull request that must show
a green interop matrix and a green `vectors` job. Nothing in this file enters `go.mod`.

The commit lines are machine-readable and are grepped by other plans' tasks; keep the
`key=<40-char sha>` shape exactly.

```
mlswg=cfd450286d1bfd9cd2519b95c80f9771f94a5b1a
openmls=fd2e4891fdd7236f10c596c1f637c5e26c588e5c
hpke=b1f7cb0cdeab6906c61b3d6574e8bdfdbe1cd3fb
xwing=9b6ce9e614811dba8d46841052f3883cbc4c1a65
```

| What | Pin | Why |
|---|---|---|
| mlswg/mls-implementations | the `mlswg=` line above | the 16 vector families **and** the gRPC test runner, pinned together so the runner and the vectors never disagree |
| openmls/openmls | the `openmls=` line above | the differential oracle and the 9 fuzz targets; built out of process in CI, never linked |
| `ghcr.io/urnetwork/mls-peer-openmls` | digest `sha256:<...>` | interop peer |
| `ghcr.io/urnetwork/mls-peer-mlspp` | digest `sha256:<...>` | interop peer |
| `ghcr.io/urnetwork/mls-peer-mls-rs` | digest `sha256:<...>` | interop peer |
| `testdata/vectors/rfc/hpke-rfc9180-x25519.json` | the `hpke=` line above, filtered; sha256 `3cc5f951dea0b7dbe80419215e64c810498ee4dd76c376763bbe6860c346b11a` | the RFC 9180 base-mode known answers that hold the HPKE instantiation to the RFC rather than to itself; see the section below |
| `../../messagegroup/testdata/vectors/rfc/xwing-draft10.json` | the `xwing=` line above, whole; sha256 `409efe197550b22985b4a0419418a0c5f2c2b193426c55bd998399ec8d3e614d` | the three draft-connolly-cfrg-xwing-kem known answers that hold the X-Wing combiner, its label position and the seed expansion to the draft rather than to themselves; see the section below |

Peer images are prebuilt and pushed to GHCR by the weekly `peer-image-bump` job, which opens a
digest-bump PR. CI never compiles Rust or C++ on a per-commit path.

## testdata/vectors/rfc/hpke-rfc9180-x25519.json

| Field | Value |
|---|---|
| Upstream repository | `cfrg/draft-irtf-cfrg-hpke` |
| Upstream commit | `b1f7cb0cdeab6906c61b3d6574e8bdfdbe1cd3fb` |
| Upstream path | `test-vectors.json` |
| Upstream sha256 | `61fc662f01996cd06d713dacf5e133167bd309a1f329442d53f1e21a47b3ede6` |
| Vendored sha256 | `3cc5f951dea0b7dbe80419215e64c810498ee4dd76c376763bbe6860c346b11a` |

Fetched from
`https://raw.githubusercontent.com/cfrg/draft-irtf-cfrg-hpke/b1f7cb0cdeab6906c61b3d6574e8bdfdbe1cd3fb/test-vectors.json`.

Not an mlswg family, hence `rfc/`: `testdata/vectors/*.json` stays exactly the sixteen mlswg files
p8 Task 6 vendors and asserts over.

Filtered from the upstream file by the deterministic selection
`mode == 0 and kem_id == 32 and kdf_id == 1 and aead_id in (1, 3)`, re-serialized with
`json.dumps(out, indent=2, sort_keys=True)` and a trailing newline. That selects exactly the two
HPKE instantiations the two registered MLS ciphersuites use. Nothing is truncated: both entries
carry all 257 encryptions and all 3 exports.

The full upstream file is 5.9 MB and 128 entries, of which 126 are for algorithms this
implementation does not have and will never gain, so vendoring it whole would be 5.7 MB of
permanently dead weight in every clone.

Because the vendored file is a re-serialization rather than a copy, the byte-for-byte comparison
against upstream that every other row here gets is made against the upstream file's own digest
above, and the transform is held to it separately: the two vendored entries were asserted to
deep-equal the two upstream entries the predicate selects, and re-running the transform was
asserted to reproduce the vendored bytes exactly. Re-vendoring is therefore reproducible from the
upstream commit and this paragraph alone.

`core.autocrlf` is `true` at system scope on at least one machine that writes to this repository,
and the sixteen mlswg files were once vendored already smudged with a manifest computed over the
smudged bytes, so they verified against bytes upstream never published.
`testdata/vectors/rfc/.gitattributes` carries `* -text` and was committed before the file it
protects; `git ls-files --eol` reports `i/lf w/lf attr/-text`, and
`TestHpkeVectorFileWasNotSmudgedOnTheWayIn` refuses a carriage return in the file at all.

Every value above is pinned a second time in `mls/hpke_vectors_test.go` â€” the vendored digest as
`hpkeVectorSha256`, the repository, commit, path and upstream digest as the constants beside it â€”
and `TestHpkeVectorProvenanceIsRecordedInThePinFile` compares the two copies field by field: it
parses the fenced block and the table above rather than grepping the file, because the commit
appears three times and the vendored digest twice, and a `strings.Contains` over the whole file is
answered by whichever copy was not corrupted. It also holds the fetch url to the three fields it is
built out of, and the predicate, the serialization call and the stated depth to the constants the
tests loop over, so re-vendoring has to move both copies or fail. Before it existed, 33 of 35
corruptions of this section left all 82 tests in the package green.
`TestHpkeVectorDirectoryDisablesGitsTextConversion` reads `testdata/vectors/rfc/.gitattributes`
itself, so the rule going missing fails on the commit that removes it rather than on the next
person's fresh clone.

## messagegroup/testdata/vectors/rfc/xwing-draft10.json

| Field | Value |
|---|---|
| Upstream repository | `dconnolly/draft-connolly-cfrg-xwing-kem` |
| Upstream commit | `9b6ce9e614811dba8d46841052f3883cbc4c1a65` |
| Upstream path | `spec/test-vectors.json` |
| Upstream sha256 | `409efe197550b22985b4a0419418a0c5f2c2b193426c55bd998399ec8d3e614d` |
| Vendored sha256 | `409efe197550b22985b4a0419418a0c5f2c2b193426c55bd998399ec8d3e614d` |
| Vectors | `3` |

Fetched from
`https://raw.githubusercontent.com/dconnolly/draft-connolly-cfrg-xwing-kem/9b6ce9e614811dba8d46841052f3883cbc4c1a65/spec/test-vectors.json`.

Three KAT vectors, vendored whole and unmodified from the draft-10 reference implementation
(`spec/xwing.py` at the same commit), which is why the upstream and vendored digests above are the same
value rather than a digest and the digest of a transform. Fields per vector: `seed` (32 B), `sk` (32 B,
equal to the seed), `pk` (1216 B), `eseed` (64 B), `ct` (1120 B), `ss` (32 B). The file is 15,177 bytes.

X-Wing is an Internet-Draft with no IANA MLS code point, so this file moves when the draft moves. Bumping
the commit is a pull request that must show `TestXwingVector*` green; a changed combiner, a moved label or
a different seed expansion would show up as a decapsulation mismatch on all three vectors at once, which is
the failure mode we want rather than a silent divergence. Nothing else in this tree can see any of those
three defects: X-Wing round trips with itself under every one of them.

Not an mlswg family, hence `rfc/`, and it lives under `messagegroup/` rather than beside the HPKE corpus
because the KEM it holds is `connect/messagegroup`'s. It lived under `message/` until the 2026-09-06 split
moved X-Wing out of `connect/message` -- that package's import of `connect/mls` was the whole of what spec B
Â§2.2 forbids the message server from linking -- and the corpus follows the test that reads it, because both
paths are resolved relative to the package directory. `mls/testdata/vectors/*.json` stays exactly the sixteen
mlswg files p8 Task 6 vendors and asserts over, and that count is unaffected by this row.

`core.autocrlf` is `true` at system scope on at least one machine that writes to this repository, and a
smudged corpus verifies against bytes upstream never published.
`messagegroup/testdata/vectors/rfc/.gitattributes` carries `* -text`; `git ls-files --eol` reports
`i/none w/none attr/-text` -- `none` rather than the HPKE corpus's `lf` because this file is one line and a
trailing newline, so git sees no line ending to classify; what matters is `attr/-text`, which is what stops
the smudge. `TestXwingVectorFileWasNotSmudgedOnTheWayIn` refuses a carriage return in the file at
all, and `TestXwingVectorDirectoryDisablesGitsTextConversion` reads the attributes file itself so the rule
going missing fails on the commit that removes it rather than on the next person's fresh clone.

Every value above is pinned a second time in `messagegroup/xwing_vectors_test.go` — the vendored digest as
`xwingVectorSha256`, the repository, commit, path and vector count as the constants beside it — and
`TestXwingVectorProvenanceIsRecordedInThePinFile` compares the two copies field by field, parsing this
section's table rather than grepping the file, for the reason the section above records: the commit appears
in the fence, in the table and in the fetch url, so a `strings.Contains` over the whole file is answered by
whichever copy was not corrupted.

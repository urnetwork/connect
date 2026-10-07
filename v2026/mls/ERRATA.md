# RFC 9420 errata this package acts on

What this file is for: an implementation of a published RFC has to decide, one erratum at a
time, whether it follows the text as published or the text as corrected. That decision is not
visible from the code — a validator that applies a corrected rule looks exactly like one that
applies an invented rule — so each entry below records the erratum **verbatim**, the status it
carried when it was transcribed, the date it was read off the errata page, and what this package
does about it.

Two rules for anything added here.

- **Quote, do not summarise.** The transcription is what a later reader checks the code against.
  A paraphrase, or an entry that says "per the plan", is worth nothing: it cannot be diffed
  against <https://errata.rfc-editor.org/rfc9420> and it hides whichever detail the paraphraser
  did not think mattered.
- **Record the status, and record that the status can change.** An erratum in `Reported` has been
  submitted and nothing more. It has not been accepted by the responsible Area Director, it is
  not `Verified`, and it is not `Held for Document Update`. Implementing one is a decision to
  diverge from the published RFC, and the divergence has to be stated as such rather than as
  "the RFC says".

This file records only errata this package has **acted on**. An erratum absent from it has not
been assessed here.

---

## Erratum 8745 — Section 13.4, LeafNode capabilities in Update proposals and update paths

Retrieved from <https://errata.rfc-editor.org/eid8745> on **2026-08-28**.

| | |
|---|---|
| Errata ID | 8745 |
| RFC | 9420, "The Messaging Layer Security (MLS) Protocol", July 2023 |
| Source of RFC | mls (sec) |
| Section | 13.4 |
| Status | **Reported** |
| Type | Technical |
| Publication Format | TXT |
| Reported By | Jan Winkelmann |
| Date Reported | 2026-02-05 |

### Section 13.4 says

```
   *  A client adding a new member to a group MUST verify that the
      LeafNode for the new member is compatible with the group's
      extensions.  The capabilities field MUST indicate support for each
      extension in the GroupContext.
```

### It should say

```
   *  A client adding a new member to a group MUST verify that the
      LeafNode for the new member is compatible with the group's
      extensions.  The capabilities field MUST indicate support for each
      extension in the GroupContext.

   *  A client updating a leaf node in the group MUST verify that the
      new LeafNode is compatible with the group's extensions.  The
      capabilities field MUST indicate support for each extension in the
      GroupContext. This applies both to Update proposals and LeafNode
      objects in the update_path in a Commit.
```

### Notes

```
The RFC says on the topic of validating LeafNode capabilities:
> Note that the latter two requirements mean that all
> MLS GroupContext extensions are mandatory, in the
> sense that an extension in use by the group MUST be
> supported by all members of the group.
> --- https://www.rfc-editor.org/rfc/rfc9420.html#section-13.4-6
To that end, it requires that we check that the LeafNodes in KeyPackages
that are added support all extensions in the group context. However, it
doesn't seem to require that the same check is performed for LeafNodes in
Update proposals or update paths.
Also see this thread on the mailing list: https://mailarchive.ietf.org/arch/msg/mls/k18P4FP7dfS2cBmP0kL6Uh50-ok/
```

### What has been checked against the source, and what has not

Re-checked on **2026-08-28**. The result is split deliberately, because "checked" with no scope
on it is the failure mode this file exists to prevent: a record that looks checked and is not is
worse than an admitted gap.

**Verified against the errata page, <https://errata.rfc-editor.org/eid8745>.** The metadata table
above — errata ID, RFC, section, status `Reported`, type Technical, reporter, date reported — and
both quoted blocks. The status is still `Reported`. None of the transcription above is the plan's
reading of the erratum; it is the erratum.

**Verified against RFC 9420 itself: section 7.2's default extension type list**, quoted in full
further down this file — `0x0001` application_id, `0x0002` ratchet_tree, `0x0003`
required_capabilities, `0x0004` external_pub, `0x0005` external_senders. That check earned its
keep. `mls/extension.go` had declared `ExtensionTypeExternalSenders` at `0x0004`, which is
external_pub's code point, while this file quoted the correct assignment a few lines away, and
the enum pin in `extension_test.go` had been transcribed from the same misreading — so a green
suite defended the wrong value and this implementation would have disagreed with every peer about
the external_senders GroupContext extension. It is corrected, and
`TestEveryRfc9420DefaultExtensionTypeIsDeclaredAtTheCodePointItAssigns` now joins the package's
declared constants to the section 7.2 list BY NAME, so two transcriptions can no longer agree
wrongly.

**NOT re-verified: where in section 13.4 the quoted bullet sits.** An earlier version of this
paragraph said the erratum's quotation is "the fifth bullet" of section 13.4's list of things
"implementations MUST correctly handle", and that the Notes' paragraph "follows two bullets
later". Neither ordinal could be re-derived on 2026-08-28: the retrieval available here truncates
RFC 9420 well before section 13, and a summarising fetch of the same page returned a *different*
section 13.4 bullet — the one about `required_capabilities` — when asked for this one. Restating
the ordinals on that evidence would be exactly the thing the two rules at the top of this file
forbid, so they are withdrawn rather than repeated.

What is asserted is narrower and is what this package acts on: the erratum reproduces the
sentence pair quoted above, and its own Notes cite the "all MLS GroupContext extensions are
mandatory, in the sense that an extension in use by the group MUST be supported by all members of
the group" paragraph at `https://www.rfc-editor.org/rfc/rfc9420.html#section-13.4-6`, which is
inside section 13.4. Whether that bullet is the fifth of its list is not something this file
knows, and the inconsistency the erratum describes does not depend on it: the published text
imposes the group-extension compatibility check on the client *adding* a member, and on nobody
updating one, while asserting a conclusion about *all members of the group*.

Anyone revisiting this should re-read the erratum page rather than trust the table above, which
is a snapshot of one day, and should treat the paragraph withdrawn here as still open.

### What this package does

`(*LeafNode).Validate` in `leaf_node.go` applies the group-extension check to **all three**
leaf_node_source values — `key_package`, `update` and `commit` — and the loop that does it is
not conditioned on the source. `TestErrata8745` states both halves over the derived source class:
a leaf that does not list a non-default GroupContext extension is refused under every source, and
a leaf that does list it is accepted under every source. An implementation written from the
uncorrected text passes the `key_package` row and fails the other two.

The loop is not conditioned on the POSITION of the extension in the GroupContext's vector
either, and that half needed a case of its own. Every `GroupExtensions` value in the tree held
exactly one entry, so restricting the loop to its first element passed the entire suite — the
rule was observed at index zero and nowhere else. A real GroupContext carries several
extensions, and the ones it carries first are exactly the ones section 7.2 exempts, so a
first-element-only loop steps over the exempt entry, never reaches the non-default extension
behind it, and admits — or lets a member update into — a leaf that does not support an extension
the group is using. That is this erratum's own security consequence, arriving by a different
route. `TestLeafNodeValidateReadsEveryGroupContextExtensionAndNotOnlyTheFirst` walks the
offending extension across every index of a four-entry vector, under every source, behind both
kinds of entry the loop must pass over.

The refusal both tests assert is `errGroupContextExtensionNotListed`, a sentinel this rule now
owns. It used to answer only `errMissingRequiredCapability`, which four other rules also answer,
and an assertion that cannot tell which rule fired cannot state that this one fired at all —
which is how the gap above stayed invisible.

**This is stricter than the published RFC, deliberately, and the erratum is only `Reported`.**
The consequences are worth being explicit about, because a reader who finds this later should not
have to re-derive them:

- Security: without the correction, a member already in the group can send an Update proposal, or
  a Commit carrying an update_path, whose new LeafNode drops support for an extension the group
  is using. Nothing in the published text refuses it, and the group is then in the state section
  13.4 says cannot happen — an extension in use that not every member supports.
- Interoperability: a peer implementing the published text may send exactly such an Update. This
  package refuses it, and the refusal is `errMissingRequiredCapability`. That is a real
  divergence and not a bug in the peer.
- Status: `Reported` means submitted. If the erratum is later **Rejected**, the strictness above
  becomes a divergence with no standing behind it and this entry — and the loop in
  `leaf_node.go` — should be revisited. Re-read the status at the URL above rather than trusting
  this table, which is a snapshot of one day.

### Where this package does NOT follow the erratum's literal wording

The erratum says "The capabilities field MUST indicate support for each extension in the
GroupContext", repeating the published sentence. Taken with no exemption, that sentence refuses
every conforming leaf of any group whose GroupContext carries `required_capabilities` (0x0003) or
`external_senders`, because RFC 9420 section 7.2 says of the same `capabilities.extensions`
vector:

```
   The capabilities field indicates the protocol features that the
   client supports, including protocol versions, cipher suites,
   credential types, non-default proposal types, and non-default
   extension types.  The following proposal and extension types are
   considered "default" and MUST NOT be listed:
   ...
   *  Extension types:

      -  0x0001 - application_id

      -  0x0002 - ratchet_tree

      -  0x0003 - required_capabilities

      -  0x0004 - external_pub

      -  0x0005 - external_senders
```

Both sentences are MUSTs and they cannot both be obeyed for a default type. This package resolves
it the only way that leaves neither sentence violated: the five default types are exempt from the
section 13.4 check, because section 11.1 says they "are assumed to be implemented by all clients,
and need not be listed in RequiredCapabilities in order to be safely used" — that is, section
13.4's requirement is already discharged for them. `isDefaultExtensionType` in `leaf_node.go`
derives that class as the contiguous range 0x0001..0x0005, which is exactly RFC 9420's own
initial extension registry (section 17.3), and
`TestTheDefaultExtensionTypeClassIsExactlyTheFiveRfc9420Section72Names` holds the range against
the five code points over the whole uint16 space. A code point registered by a later document is
**not** default and must be listed.

---

## Erratum 8815 — Section 12.2, proposal references in a Commit are not validated

Retrieved from <https://errata.rfc-editor.org/eid8815> on **2026-09-02**.

| | |
|---|---|
| Errata ID | 8815 |
| RFC | 9420, "The Messaging Layer Security (MLS) Protocol", July 2023 |
| Section | 12.2 |
| Status | **Reported** |
| Type | Technical |
| Reported By | Ludovic Paillat |
| Date Reported | 2026-03-09 |

### Section 12.2 says

```
For a regular, i.e., not external, Commit, the list is invalid if any of the following occurs: It
contains an individual proposal that is invalid as specified in Section 12.1.
```

### It should say

```
For a regular, i.e., not external, Commit, the list is invalid if any of the following occurs: It
contains a reference to a proposal that was not previously received by the group member. It
contains an individual proposal that is invalid as specified in Section 12.1.
```

### Notes

**Not transcribed.** The retrieval available here returns the errata page through a summarising
fetch, and what it gave back for this field was a paraphrase in the third person ("The errata
submitter observes that ...") rather than the submitter's own words. This file's first rule is
quote, do not summarise, so the Notes are recorded as unread rather than as a quotation they are
not. Anyone revisiting this should read them at the URL above.

### What has been checked against the source, and what has not

**Verified against the errata page, <https://errata.rfc-editor.org/eid8815>, on 2026-09-02.** The
metadata table — errata ID, RFC, section, status `Reported`, type Technical, reporter, date
reported — and the two language blocks.

**NOT verified: the line breaking of the two quoted blocks.** The errata page renders them as
fixed-width blocks and the retrieval returned them as running text, so the wrapping above is this
file's and not the page's. The words are the page's; the line breaks are not. That distinction
matters here because RFC 9420 section 12.2's own list is a bulleted one and the quotation reads as
a run-on sentence, which is an artefact of the retrieval rather than of the erratum.

**Verified against RFC 9420 itself: section 12.2's invalidation list and section 12.4's
ProposalOrRef.** Section 12.2's list of what makes a proposal list invalid is quoted in full in
`mls_measure/mls-go/rfc9420.txt` and contains no clause about references at all, while section 12.4
says "Proposals sent by reference are specified by including the hash of the AuthenticatedContent
object in which the proposal was sent" and "A sender and a receiver of a Commit MUST verify that the
committed list of proposals is valid as specified in Section 12.2". The gap the erratum names is
real in the published text.

### What this package does

`CheckErrata8815` in `validate_commit.go` states the added clause over a commit's `ProposalOrRef`
vector: every entry of type `reference` names a proposal this member holds. It answers
`errProposalNotCached`, which is the value `(*ProposalCache).Resolve` already answers the same
question with — one rule, one value, two doors. A nil cache holds nothing, so a commit naming any
reference is refused under one; a commit carrying only by-value proposals names no reference and
passes, which is the correct answer for it.

`(*ProposalCache).Resolve` already refused an uncached reference before this entry existed, so the
erratum costs this implementation no behaviour change on the path that resolves a vector. What it
adds is the rule stated where a vector has NOT been resolved — a commit this client is assembling,
or one whose list arrived already bucketed — and a named target the negative test can drive.

**This is stricter than the published RFC, deliberately, and the erratum is only `Reported`.** The
consequences:

- Security: without the clause, section 12.2 states no rule about references, so a commit naming a
  reference no member holds is not invalid by the published text. Every receiver that cannot resolve
  it computes a different post-commit tree from the committer, and the disagreement is only caught
  by the confirmation tag — after the epoch has been derived, with nothing left to say which
  proposal was missing.
- Interoperability: none observed. A peer implementing the published text still has to resolve the
  reference in order to apply the commit at all, so a commit this rule refuses is one that peer
  cannot apply either. The divergence is in what is REPORTED, not in what is accepted.
- Status: `Reported` means submitted and nothing more. Re-read the status at the URL above rather
  than trusting this table, which is a snapshot of one day.

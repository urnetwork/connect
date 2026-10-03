# The one rule every gate in these three trees is held to, and the query that checks it

What this file is for: `mls`, `message` and `messagegroup` defend most of their properties with
**gates** — a test that derives a class of things and then asserts something over every member. A
gate is only ever as good as its class, and this project has now shipped the same class defect
**nine times at nine altitudes**, each one green, each one found by a reviewer a round later at
full cost.

**The line stops at nine, and it stops on evidence rather than on exhaustion.** The ninth is
recorded below and is NOT closed; [why the line stops here](#why-this-line-stops-here-and-the-criterion-that-replaces-the-slogan)
is the section that has to be read before anybody opens a tenth round, because the argument for
stopping is a measurement and not a mood.

The sixth, the seventh, the eighth and the ninth were all this file, each one inside the fix for
the one before it. It is worth stating plainly, because it is the reason the file no longer looks
the way it did — and because four rounds in a row is the argument for deciding a claim in a test
rather than agreeing with it in prose:

> The artifact written to stop the fifth instance contained the defect. Its table opened
> *"Every arity- or name-shaped narrowing over a reflected class"* and held **eight of the
> fifty-four that exist**. Its published query — offered as *the query that finds the next one* —
> grepped the literal symbol `method.Name`, so every narrowing whose receiver was spelled
> `writer`, `reported`, `of` or `verify` was invisible to it. A universal claim written by hand is
> a list wearing a quantifier, and a query keyed to a symbol is derived from the instance.
>
> And the fix for that contained the SEVENTH. The derivation it replaced the table with reads a
> **reflected method set**, under the sentence *"the two doors reflect offers onto a method set
> are `Method(i)` and `MethodByName(n)`"* — true of a method set, and the wrong class. Reflect
> opens onto a type's **fields** as well, and a name narrowing over a field set is this file's
> subject exactly as much as a name narrowing over a method set. **Seventy occurrences over
> sixty-eight keys stood one door over**, unindexed and unprinted, and a planted one passed all
> four gates below. Every one of the six findings that raised this file happened to be about a
> method, so the scope reproduced the shape of its instances.
>
> And the fix for THAT contained the EIGHTH, one altitude down. The doors were derived and the
> **readings off what a door hands back** were left as two literals: the selector `Name` and the
> selector `Type`. A member descriptor carries more — `reflect.Method` is a name, a package path,
> a type, a func and an index, and `reflect.StructField` adds a tag, an offset and an embedding
> flag — and every one of them is a fact about the member, so a predicate deciding by one narrows
> the same class. **That complement, six spellings, was stated nowhere and printed nowhere.** Four
> narrowings over it were planted and all five gates stayed green, the derived class stayed
> byte-identical, and the recall fraction did not move. Worse, the boundary paragraph offered
> `go/types` as the remedy for what this file cannot see, and **a go/types rebuild would have
> caught none of the four**: a struct tag is a spelling the parse tree already holds.
>
> And the fix for THAT contained the NINTH, one shape over. The doors were derived and the
> readings were derived, and the **container a member is carried through** was two literals, `X`
> and `[]X`, written into `gatesAnswers` as `TrimPrefix(spelling, "[]")` and into
> `gatesDoorSet.declares` as an exact match on `reflect.X` / `[]reflect.X`. On the toolchain in
> this checkout that clause removes **two spellings — `Methods` and `Fields` — over the four
> iterator doors Go 1.26 added to reflect's exported API**: `Type.Methods`, `Type.Fields`,
> `Value.Methods` and `Value.Fields`. And a second family sits beside it: `gatesGather` binds a member
> through four statement forms, so a member bound by a **type assertion** or a **type switch**
> binds nothing at all. Six narrowings across the two families were planted and all seven gates
> then in this file stayed green — **and, replanted after this round added two more, all nine do**.
> **That one is filed and not fixed**, and the section that says why is the point of this document
> now.

So the table below is no longer written. It is **derived**, from the parse tree, by
`gates_index_test.go`, and the two are required to be the same set in both directions. The query
below is no longer trusted. It is **run**, and its recall against that derivation is measured on
every test run and published here as a fraction this file is held to. The **doors** that derivation
reads a member through are no longer named either: they are derived from reflect's own source, by
the property that makes something a door. And the **readings** off what a door hands back are no
longer named either: the fields a member descriptor carries are found by the same sentence that
admits the descriptor, so a field Go adds to `reflect.Method` or `reflect.StructField` joins the
reading on the same day — and the ones this file does not read as a name or a type are printed on
every run.

**What this paragraph used to claim next was that "a door Go adds in a later release joins this
reading on the day the toolchain moves", and that sentence is measurably false on the toolchain in
this checkout.** Go 1.26 added four iterator doors to reflect and none of them joined, because the
derivation reads a descriptor bare or in a slice of them and an `iter.Seq[Method]` is neither. The
sentence has been deleted rather than softened. That is the ninth instance, and it is the reason
the next section exists.

---

## The property

> **A gate's class has two halves — the MEMBERS it admits and the SCOPE it reads them from — and
> every narrowing of either half must be stated in terms of the property being defended. A narrowing
> is derived from the INSTANCE when its justification names something that exists in the tree today:
> a symbol, a path, a count, or the construction a finding was first seen in.**

And the operational form, which is the part that is checkable:

> **Print the complement. A gate that narrows must name, at run time, every member it removed and
> the predicate it removed them by.**

An exclusion nobody prints is an exclusion nobody reads, and the two failure modes look identical
from outside a gate that does not print:

| what the complement prints | verdict |
|---|---|
| `[]` — empty | The narrowing was written for members that **do not exist**. It removes nothing today and will begin removing real ones on the commit that adds the first member of the shape it excludes. Delete it, or turn it into a **fail-closed refusal** (`t.Errorf`/`t.Fatalf`), never a `continue`. |
| exactly the identifier the narrowing's own comment names | Derived from the **instance**. Restate the exclusion from the property; if the restated sentence still needs that identifier, there is no exclusion, only a member you did not want to handle. |
| a set describable without naming any member of the tree | Derived from the **property**. Keep it, and keep printing it. |

The empty row is the one that matters most, because it is the one that reads as harmless. Four of
the first five instances had an empty or a one-name complement at the moment they were written, and
so did **both** of the rows this table carried as OPEN for a round.

**And the same reading applies to a QUERY and to a TABLE.** A query whose complement — the sites it
does not reach — is unprinted is the same defect as a gate whose complement is unprinted, and a
table claiming a class with no derivation behind it is a gate whose class is a list. That sentence
is the sixth instance, and the tests named at the bottom of this file are it, closed.

---

## The nine, and what each narrowed by

1. **A class dispositioned by a COUNT instead of a grep.** The number of members stood in for the
   members. A count cannot have a complement, which is why nothing could be read off it.
2. **A location query built from the NUMBERS a ruling changed** instead of from the **claims** it
   falsified. The scope half: the places to look were derived from an artefact of the ruling.
3. **A sweep for a missing AEAD nonce built from the CONSTRUCTION the defect was found in** (a KEM
   seal) rather than from the property it names (an AEAD key from a bare expand with no nonce beside
   it). It walked past a second instance in the same document.
4. **A gate whose class was a DIRECTORY** rather than "code that produces the record's keyed
   octets". Scope again. Closed: `crypto_forbidden_test.go`'s `forbiddenScanRoots` derives its
   roots from the module's own import graph and asserts the derivation, and a fourth cryptographic
   sibling joins by existing.
5. **`epochSliceAnsweringAccessors`, inside the fix for the fourth.** It derived its class off the
   type and then narrowed it with `if method.Type.NumIn() != 1 { continue }`, argued in its own
   comment from the one argument-taking method that exists: *"a method that takes an argument is not
   an accessor -- InstallWraps is this type's writer."* The property is "an exported method that can
   hand back this value's live octets", and a method that takes an argument can do exactly that.
   **Its complement was EMPTY** — `InstallWraps` answers only an error, so the result reading had
   already removed it and the arity line removed nothing at all. Two planted accessors,
   `PqSecretFor(purpose string) ([]byte, error)` and `WrapAt(index int) ([]byte, error)`, each
   handing back a copy of `pq_secret`, passed the entire suite.
6. **This file, inside the fix for the fifth.** Its table was a hand-written list under a universal
   quantifier, and its published query was keyed to one spelling of the receiver. Three narrowings
   over reflected method sets — `mlsEncodingEmitters`, `TestNoVectorRunnerCanSkip` and
   `trRecordLayerCodecMethods` — were invisible to both, and the first of them removed **four**
   methods of `*syntax.Writer` (`Bytes`, `Err`, `Len`, `MaxVectorLength`) without naming one of
   them. Measured over the whole class rather than over those three, the table understated itself
   by **forty-six rows of fifty-four**, and the published query reaches **33 of the 55** narrowing
   occurrences those rows cover.
7. **This file again, inside the fix for the sixth, and the SCOPE half.** The derivation that
   replaced the table was scoped to a reflected **method** set — two door names, justified by a
   sentence naming the two doors onto a method set. `Field`, `FieldByName`, `FieldByIndex`,
   `FieldByNameFunc` and `VisibleFields` are doors onto the same kind of class, and a name
   narrowing over a field set is the identical defect. The index claimed *"every narrowing over a
   reflected method set"* and the file said nowhere that a field set was outside it; the
   complement was **not empty and not printed**, and `framedContentArmFields` — one member of it
   — removed every field of `FramedContent` that appears in the cleartext header, plus one named
   by hand, without printing one of them. Closed by deriving the doors from reflect's own API
   rather than from the findings: see `gatesDeriveDoors`. The index went from 54 rows to **148**,
   and the published query's recall from **54/55** to **99/158**.
8. **This file a third time, inside the fix for the seventh, and the READING half.** The doors
   were derived; what a door HANDS BACK was still two spellings, `Name` and `Type`, written into
   two functions under no sentence at all. A member descriptor carries eight exported fields
   between the two of them, and the six the derivation did not read —
   `PkgPath`, `Func`, `Index`, `Tag`, `Offset`, `Anonymous` — are facts about the member exactly
   as its name is. Four planted narrowings over that complement (`field.Tag`, `field.PkgPath`,
   `field.Anonymous`, `len(member.Index)`) left **all five gates green**, the derived class
   byte-identical at 158 occurrences over 148 keys, and the recall fraction unmoved. **Three live
   sites were already in the tree**, each dropping a struct field carrying no `json` tag, two of
   them under a doc comment invoking the very rule they broke. And the sentence this file printed
   about what it could not see — *"it under-reaches in exactly three places, and all three want
   the same thing: a TYPE"* — was measurably wrong: none of the four wants a type, and the
   `go/types` rebuild it proposed would have found none of them. Closed by deriving the fields
   from the descriptor itself, in `gatesDescriptorFields`: see `gatesIsMemberAttribute`. The index
   went from 148 rows to **160**, and the published query's recall from **99/158** to **99/170**.
   The same round's self-check, run over its own diff, found one more of the shape one level down
   and closed it: the predicate class itself was narrowed by ARITY — "a function answering exactly
   one `bool`" — with an empty, unprinted complement.
9. **This file a fourth time, inside the fix for the eighth, and the CONTAINER half — AND THIS
   ONE IS FILED RATHER THAN CLOSED.** The doors were derived and the readings off a descriptor
   were derived; the **shapes a member is carried through** were two literals, `X` and `[]X`.
   `gatesAnswers` is `wanted[strings.TrimPrefix(spelling, "[]")]`, `gatesDoorSet.declares`
   exact-matches `reflect.X` and `[]reflect.X`, and the door sentence in this file said only
   *"or a slice of them"*. **Measured on the toolchain this gate runs on, by
   `TestTheContainerClauseOnTheDoorSentenceIsANarrowingAndItsSizeIsMeasured`**, that clause
   removes two spellings — `Methods` and `Fields` — over **six exported declarations**, four of
   which are doors on reflect's exported API and are the four iterator doors Go 1.26 added:
   `Type.Methods() iter.Seq[Method]`, `Type.Fields() iter.Seq[StructField]`,
   `Value.Methods() iter.Seq2[Method, Value]` and `Value.Fields() iter.Seq2[StructField, Value]`.
   (The other two are `*rtype.Methods` and `*rtype.Fields`, the unexported implementation of
   `Type`.) Both spellings sit in the 126-name `notDoors` list this file prints, which is the only
   place they appear and the one place nothing points at. The same two-entry list removes `*reflect.Method`,
   `map[K]reflect.Method`, `[4]reflect.StructField`, `...reflect.Method` and a defined slice type
   over one. **A second family sits one altitude over**: `gatesGather` binds a member through four
   statement forms — `AssignStmt`, `RangeStmt`, `ValueSpec`, `Field` — so a member bound
   by a **type assertion** or a **type switch** binds nothing, and the `RangeStmt` case reads only
   the value and never the key. Six narrowings across the two families were planted in a real
   `mls` `_test.go` and **all seven gates then in this file stayed green**, the derived class
   byte-identical at 170 occurrences over 160 keys, while a `[]reflect.Method` control planted
   beside them turned the index gate red immediately. **Replanted after this round, all NINE stay
   green** — which is the measurement that says what the two gates added here did and did not do:
   they make the narrowing visible, and they do not close it. **None of the nine wants a type**, so the `go/types` rebuild this
   document has twice offered as the remedy for what it cannot see would find none of them —
   the third round running that sentence has been wrong about the next instance.

   All nine shapes are now DRIVEN through the control in
   `TestTheGatesDerivationSeesANarrowingHoweverItsReceiverIsSpelled` and asserted **not found**,
   beside the three under-reaches that do want a type. That is what filing it means here: the
   boundary is a measurement that goes red on the day somebody widens the class, not a paragraph.
   `OPEN` is red for a **row of the index** below, and this is not one of those — it is the
   derivation's own boundary, stated and exercised.

---

## Why this line stops here, and the criterion that replaces the slogan

**Nine rounds have been spent on one defect class**, and the class has not changed once: *a
narrowing whose complement is unprinted, or whose justification names something that exists in the
tree today.* Every instance was green when it shipped. Every one was found by the next reviewer,
one round later, at full cost. And **the last five were each found inside the fix for the one
before**:

| # | the narrowing | half | found inside |
|---|---|---|---|
| 1 | a class dispositioned by a **count** rather than a grep | members | — |
| 2 | a location query built from the **numbers** a ruling changed | scope | — |
| 3 | a sweep built from the **construction** a finding was raised in | members | — |
| 4 | a gate whose class was a **directory** | scope | — |
| 5 | an accessor class narrowed by **arity** | members | the fix for the fourth |
| 6 | this file's hand-written table, and a query keyed to `method.Name` | members | the fix for the fifth |
| 7 | the derivation's **doors**, scoped to a reflected METHOD set | scope | the fix for the sixth |
| 8 | the **readings** off a descriptor, left as `Name` and `Type` | members | the fix for the seventh |
| 9 | the **container shapes** a member is carried through, `X` and `[]X` | members | the fix for the eighth |

The obvious reading of that table is that it should be run a tenth time. **It should not, and the
reason is in round eight's own commit.**

### The two-list proof

Round eight (`edb68e6`) closed the reading half and, in the same commit, added a second derivation
one library over so the overlap with `go/types` could be measured: `gatesAnswersObject`, modelled
on `gatesAnswers` and described in that commit's own text as *the same sentence one shape over*.

```go
// gatesAnswers   -- round seven, untouched by that commit
if wanted[strings.TrimPrefix(spelling, "[]")] {

// gatesAnswersObject -- added by that commit
bare := strings.TrimPrefix(strings.TrimPrefix(spelling, "[]"), "*")
```

**Two containers, two different literal lists, in one commit, written by one agent in one sitting** —
and the widening was not carried back. So the failure is not inattention and not inexperience; the
same hand wrote both halves within minutes of each other, and neither half said it was narrowing
anything.

### What that actually demonstrates

**Every derivation bottoms out in some literal.** There is no altitude at which the literal
disappears; there is only a choice about where to put it, and each round of this line moved it
one level down:

- the table was a list of narrowings → replaced by a derivation over the parse tree, which
  bottoms out in the literal door names `Method` and `MethodByName`;
- the doors were a list → replaced by a derivation over reflect's own source, which bottoms out
  in the literal field names `Name` and `Type`;
- the readings were a list → replaced by a derivation over the descriptor, which bottoms out in
  the literal containers `X` and `[]X`, and in the literal statement forms `AssignStmt`,
  `RangeStmt`, `ValueSpec` and `Field`;
- and the sentence that admits a descriptor bottoms out in `"Name"`, `"Type"` and `"string"` —
  a literal this file already carries, already prints, and already fails closed on.

Nine rounds is enough evidence to say that "keep going until nothing is a list" does not terminate.
It is the wrong stopping condition, because the thing being chased is not a mistake anybody made.

### So "derive from the property" is retired here

That slogan was in this project's standing rules for **four of the first five failures and caught
none of them**, and the reason is now clear: every one of those narrowings was written by somebody
who believed they had derived from the property — and had, right down to the literal at the
bottom. The slogan is not checkable. It cannot be applied to a diff and it cannot be applied to a
gate, because it gives no test that a reader can fail.

**What replaces it is two questions, and both are answerable by looking at the code:**

> ### 1. Is the literal at a level where being wrong is VISIBLE?
> ### 2. Does it FAIL CLOSED, and does it PRINT ITS COMPLEMENT?

That is a bounded engineering criterion. It does not promise that no literal is wrong — it
promises that a wrong one announces itself. Worked against this file's own literals:

| the literal | visible? | fails closed? | prints its complement? | verdict |
|---|---|---|---|---|
| `"Name"` / `"Type"` / `"string"` in `gatesDescriptorFields` | yes — if reflect renames either field the sentence admits **no** descriptor | yes — `gatesDeriveDoors` returns an error and `gatesDoorsOf` fatals | yes — the exported structs it removed, on every run | **keep** |
| the exported-field narrowing in `gatesDescriptorFields` | yes | **now yes** — an unexported field in a descriptor refuses rather than `continue`-ing | yes, and empty today, which is why the refusal was added | **keep** |
| `"bool"` in `gatesRecordDecided` | yes | no, but it over-reports rather than under-reports at the class boundary | **now yes** — 43 result positions in these trees read a member and are not spelled `bool`, named on every run | **keep** |
| `"[]"` in `gatesAnswers` — the DOOR reading | **was no**, the two spellings it removed read as ordinary non-doors inside a 126-name list; **now yes**, `TestTheContainerClauseOnTheDoorSentenceIsANarrowingAndItsSizeIsMeasured` asks the sentence both ways and holds the `gates-doors` pair | **no** — a container it does not know is simply not a door, silently | **was no; now yes** — two spellings over six declarations, named on every run | **the ninth, made visible** |
| `reflect.X` / `[]reflect.X` in `declares` — the PARAMETER reading | **no** — a helper taking `*reflect.Method` binds nothing and nothing says so | **no** | **no**, and unlike the row above there is no enumerable universe of parameter spellings to print it against, which is why making this half visible is a round rather than a gate | **the ninth, still silent** |
| the four statement forms in `gatesGather` | **no** | **no** | **no** | **the ninth, still silent** |

The last three rows are exactly why the ninth is real and exactly why it is not urgent: it is the
same class at the level this line has now reached, and the criterion says what to do about it
without another round of the arms race. **What this round did is the first row's answer and not
the other two's**: one literal was moved to where being wrong is visible, and the two that could
not be were written down instead. **A tenth round would find a tenth literal. It always will. The
question that decides whether that literal matters is in the three rows above.**

### What a future pass should do instead of round ten

1. **Do not open a round to make a list one entry longer.** Carrying the `*` from
   `gatesAnswersObject` back into `gatesAnswers` is the tempting example, and it is measured:
   package reflect declares **no exported symbol answering `*Method` or `*StructField`**, so that
   widening changes the door set by nothing at all. It would turn a two-entry list into a
   three-entry list, leave the iterator, the map, the array, the variadic and the named slice type
   outside it, and make the sentence read **more** complete than it is. That is the empty-complement
   row of the table above, which this file calls the dangerous one.
2. **If the ninth is closed, close it as a class.** The sentence to write is "a member is read
   through whatever container carries it, and every container this reading does not know is
   printed" — which needs a complement to exist before the first line of it is written. The
   nine control shapes in `TestTheGatesDerivationSeesANarrowingHoweverItsReceiverIsSpelled` are
   the acceptance test, and they will go **red** when it works: move them to the seen half, do not
   delete them.
3. **Judge any new gate by the two questions, not by whether its class is derived.** A derived
   class whose literal is invisible and silent is worth less than an enumerated one that refuses
   and prints. That inversion is the whole of what nine rounds bought.

---

## The query

Three forms, because a class is built out of reflection, out of source, or out of prose, and the
reading differs.

### Q1 — reflection-derived classes

**Q1 is a test, not a grep.** `gates_index_test.go` parses every `_test.go` file under the scan
roots and derives, without naming any symbol of the tree:

> a **predicate** that reads either the **NAME** of a member drawn from a reflected **member set**,
> or the **type or signature shape** of that member.

**"MEMBER SET", and the doors onto one are derived rather than named.** A door is any exported
function or interface method of `reflect` that answers a **member descriptor** — a struct reflect
exports that names one member of a type and carries that member's type — **bare or in a slice of
them** — plus, in reflect's Value half, an exported method that answers a `Value` for the same
arguments such a door takes. That sentence names no symbol of this tree and no symbol of reflect.
Naming the two method doors instead was the seventh instance.

**AND "BARE OR IN A SLICE OF THEM" IS A NARROWING, WHICH IS THE NINTH INSTANCE — this paragraph
did not carry that clause at all until this round, so the document stated a rule the code did not
implement and stated it as though it were the whole class.** The clause is two literals, `X` and
`[]X`; it is implemented as `strings.TrimPrefix(spelling, "[]")` in `gatesAnswers` and as an exact
match on `reflect.X` / `[]reflect.X` in `gatesDoorSet.declares`; and unlike every other narrowing
this file performs, **it had no complement of its own** — the door half has one now and the
parameter half still does not. So the count has to be read as a count over a narrowed class rather
than as a class:

- **run against Go 1.26 the sentence WITH the container clause admits ten spellings**, two of which
  it over-reports and says so, and the whole set is printed on every run beside the exported
  structs the descriptor sentence removed;
- **the same sentence WITHOUT the clause admits twelve** — the two further spellings being
  `Methods` and `Fields`, over six exported declarations, of which four are doors on reflect's
  exported API and are the four iterator doors Go 1.26 added: `Type.Methods() iter.Seq[Method]`,
  `Type.Fields() iter.Seq[StructField]`, `Value.Methods() iter.Seq2[Method, Value]` and
  `Value.Fields() iter.Seq2[StructField, Value]`;
- and **until this round the two spellings the clause removes were printed only inside the
  126-name `notDoors` list**, where they read as ordinary non-doors. That is the whole of why this
  one was invisible for a round: the complement of the DOOR SENTENCE printed them, and the
  complement of the CONTAINER CLAUSE — which is the narrowing that actually removed them — did not
  exist. It exists now, and it is the line below.

Ten is therefore the size of what this reading admits, not the size of what the sentence above
describes, and **both halves of that are now a measurement rather than a claim**:

**gates-doors: 10 / 12**

`TestTheContainerClauseOnTheDoorSentenceIsANarrowingAndItsSizeIsMeasured` derives both readings
off reflect's own source on every run, names every spelling the clause removes, and holds this
pair to what it measured — so on the toolchain that adds the next container the document goes red
with the new list already in hand. **That makes the ninth VISIBLE. It does not make it CLOSED**: a
narrowing spelled `for member := range subject.Methods()` is still invisible to every gate here,
and printing a complement is not the same as removing a narrowing. The gap is filed rather than
closed: see
[why this line stops here](#why-this-line-stops-here-and-the-criterion-that-replaces-the-slogan).

**AND THE READINGS OFF WHAT A DOOR HANDS BACK ARE DERIVED TOO, which was the eighth instance.**
A member descriptor does not carry two facts about a member, it carries eight; the field that NAMES
the member is read as a name narrowing, the field that carries its TYPE as a shape narrowing, and
**every other exported field of a descriptor** is read as a narrowing over a fact the descriptor
carries. Those fields are found by the same sentence that admits the descriptor rather than spelled
here, the six that are neither the name nor the type are printed on every run, and a field Go adds
to either descriptor joins the reading on the day the toolchain moves — which is true of a FIELD
and, as the ninth instance records, was not true of a DOOR. Reading only the first two
left `field.Tag`, `field.PkgPath`, `field.Anonymous` and `member.Index` invisible to every gate in
this file.

The receiver can be spelled anything at all; a member reached through a door directly, through a
`[]reflect.Method` or a `[]reflect.StructField` a helper was handed, or through a `reflect.Value`
whose signature comes back from `Type()`, all reach the same reading. **The STATEMENT a member is
bound by is a four-entry list, and that is the ninth instance's second family**: an assignment, a
range, a value specification and a field declaration bind a member, so a member bound by a **type
assertion** (`member := carried.(reflect.Method)`) or by a **type switch** is bound by nothing at
all, and the range case reads only the value, never the key. Both spellings name the descriptor
outright, exactly as a parameter does; neither wants a type. Driven through the control and
asserted not found, and filed rather than closed. **And a predicate is a
predicate however it is spelled**: the condition of an `if`, `for`, `switch` or `case`; an
identifier bound to a member reading above the line that decides by it (`drop :=
strings.HasPrefix(member.Name, "Gamma")` then `if drop`, or `key, _, _ :=
strings.Cut(field.Tag.Get("json"), ",")` then `if key == ""`); or the result of a function or
literal that answers one `bool`. That is the half a grep cannot do, and it is why the eight-row table was
wrong for a round.

The greps below are the **first cut**, kept because a reader wants something to run:

<!-- gates-query:begin -->
```sh
grep -rnE '\.NumMethod\(\)|\.MethodByName\(|\[\]reflect\.Method|\.NumField\(\)|\.FieldByName\(|\[\]reflect\.StructField' --include=*_test.go mls message messagegroup
grep -rnE '(if|for|case) .*\.Name|\.Type\.(In|Out|Kind|Elem|NumIn|NumOut|IsVariadic)\(|\.Type\(\)\.(In|Out|Elem|Kind|NumIn|NumOut)\(|\.(NumIn|NumOut)\(\)|\.(In|Out)\([0-9a-zA-Z_]+\) *(==|!=)|\.Field\([0-9a-zA-Z_]+\)|\.Kind\(\) *(==|!=)' --include=*_test.go mls message messagegroup
```
<!-- gates-query:end -->

Their recall against the derivation is measured on every run and stated here, and the sites they do
not reach are named in the test log:

**gates-recall: 104/178**

**That fraction is a MEASUREMENT and not a claim.** These patterns were fitted against the tree as
it stands, which is instance-derived by construction — which is exactly why the number is published
rather than the completeness. When a narrowing appears that they do not reach, this fraction goes
wrong and `TestTheQueryGatesPublishesIsMeasuredAgainstTheDerivationRatherThanTrusted` turns red.
**The correct response is to update the fraction, not to chase the pattern.** The derivation is the
query; the greps are a convenience whose honesty is enforced.

It is **not** 170/170, and the gap is wide rather than narrow, which is the mechanism working
rather than a defect. The **71 occurrences these patterns miss sit in 45 functions**, and they miss
them for four reasons a pattern cannot fix: a predicate that hands a member's type straight to a
function (`typeReachesByteStorageThrough(field.Type(), entered)`) carries none of the spellings a
grep can key on; a predicate bound to a name decides on a line that mentions no member at all (`if
!driven`); a member reached through a helper's parameter is only a member because of a
declaration in another file; and a fact cut out of a member's descriptor is decided two lines
later, by an identifier the patterns cannot tell from any other string (`key == ""`). Every one of
them is named in the test log on every run. That is what
printing a query's complement looks like, and it is also the argument for why the derivation and
not the grep is the query.

For each narrowing, make it append the rejected member to a slice and `t.Logf` that slice beside the
class. Then read the printed complement against the table above. Two shapes are always suspect:

- an **arity** test (`NumIn`, `NumOut`) on a class whose property is about RESULTS. Arity is a
  driver problem, not a membership problem: drive an argument-taking member with the zero value of
  each argument and let the reader fail closed, or refuse it out loud — do not `continue`.
- a **name** test (`HasPrefix`, `Name ==`) on a class the compiler can describe by shape.

### Q2 — source-scanned and scope-shaped classes

```sh
grep -rnE 'ReadDir|filepath\.Walk|ScanRoots|\.\./' --include=*_test.go mls message messagegroup
```

A gate that reads a set of directories, files or packages must print the ones it did **not** read,
and must derive that set from a property (`forbiddenScanRoots` derives "the cryptographic packages
this one is connected to" and asserts the derivation). A hand-written root list, a file exempted by
base name, and a `go list -f '{{range .Imports}}'` reading that only sees the runner's GOOS are all
the scope half of this defect.

**Q1 has TWO scope halves and both are derived, which is the seventh instance stated as a rule.**
The first is the set of files read: `forbiddenScanRoots`, **aliased and not restated**, so a fourth
package joins this file's index on the commit that joins that gate's roots. The second is the set
of **doors** a member is read through, and naming that one was the seventh instance: a derivation
can be perfect over the class it reads and still be scoped to a third of it. When a gate derives a
class, ask what it derives it *through*, and derive that too.

### Q3 — prose classes: plans, findings, rulings

A sweep raised by a finding must be built from the **property the finding names**, never from the
**construction it was found in**. The test is a sentence: write the class with no identifier, no
path and no number in it. If you cannot, you do not have the class yet — you have one member of it.
The same applies to a table claiming to hold "every rule of X": name the derivation that produced
the rows, or the row you forgot is invisible. **That last sentence was in this file while its own
table was a list of eight**, which is the whole argument for deciding a claim like it in a test
rather than agreeing with it in prose.

---

## The index

Every narrowing over a reflected **member** set in these three trees — through every door reflect
offers onto one, methods and fields alike; by every fact a member's descriptor carries, its name
and its type and the six besides; and whichever of the three ways a predicate is spelled. It is not
written — it is **derived**, and `TestTheGatesTableIsTheDerivedClassAndNotAListOfIt`
holds the two equal in both directions: a site in the tree and not here fails, and a row here whose
narrowing no longer exists fails too.

The key is the **file, the function and the rendered condition** — never the line number, because a
document keyed to line numbers rots on the first edit above it, and a gate that goes red for a
reason nobody caused is a gate that gets bypassed. Keyed this way it goes red on exactly one event:
a narrowing changed, which is the event that needs its complement read again.

The derivation is deliberately **over-broad** in three respects, stated here and DRIVEN through
the control rather than discovered later: it binds an identifier to a member for the whole of the
function it is bound in, not for the block Go scopes it to; it treats anything reached from a
member's signature as a reading of that signature; and in reflect's Value half it admits two
element readings that take the same arguments a field door takes. Over-reporting is the safe
direction; a site it reports that is not really a narrowing gets a row saying so, and four rows
below say exactly that.

**AND WHAT IT UNDER-REACHES IS STATED HERE AS A LIST OF WHAT IS DRIVEN, NOT AS A COUNT OF WHAT
EXISTS — which is the repair of a sentence that was measurably wrong.** This paragraph read *"it
under-reaches in exactly three places, and all three want the same thing: a TYPE… closing any of
them needs `go/types` and a full type-check"*, and that sentence was the eighth instance's cover:
four narrowings over the fields a member descriptor carries besides its name and its type were
invisible to this derivation, **none of them wanted a type**, and the `go/types` rebuild this
paragraph proposed would have found none of them — a struct tag is a selector the parse tree
already holds. A count of what a derivation cannot see is a claim about the unseen, and this file
has now been wrong about it twice.

**AND ONE NARROWING IN THAT DERIVATION NOW REFUSES RATHER THAN CONTINUING.**
`gatesDescriptorFields` reads only the EXPORTED fields of a member descriptor, because a predicate
in these trees cannot spell an unexported one. Measured on Go 1.26 that removes **nothing** —
`reflect.Method` and `reflect.StructField` are exported through and through — and a narrowing that
removes nothing, disposed of with a `continue`, is the row the table above calls the dangerous one,
sitting inside the file that publishes the table. `gatesDeriveDoors` now returns an error naming
the fields instead, so the day reflect adds an unexported field to a descriptor this derivation
says so and stops rather than quietly reading one fact fewer about every member. The complement is
still printed on every run, and it is still `[]`.

So what is written here is what is **driven through the control and asserted NOT found**, and
nothing more. **Twelve shapes, in three groups**, every one of them written into
`gatesControlSource` and asserted absent from the derivation's reading on every run:

**Three that want a TYPE**, and `go/types` with a full type-check would close each of them:
a member reached through a parameter declared `reflect.Value`; a member held in a struct FIELD
whose type is declared in another declaration; and a predicate answering a DEFINED type whose
underlying type is `bool`.

**Six CONTAINER shapes — the ninth instance, filed and not fixed.** A member is read bare or in a
slice of descriptors and in no other container, so all six of these bind nothing: a member set
reached through an ITERATOR door (`for member := range subject.Methods()`, which on Go 1.26 is a
real door of the reflect this gate parses) and the field twin of it; a member declared
`*reflect.Method`; one held in a `map[K]reflect.Method`; one held in a DEFINED slice type over a
descriptor; and one taken variadically.

**Three STATEMENT forms — the ninth instance's other family.** A member bound by a type assertion,
by a comma-ok type assertion, or by a type switch is bound by nothing, because the four statement
forms that do bind one are an assignment, a range, a value specification and a field declaration.

**None of the nine in the last two groups wants a type.** Each spells the descriptor outright — in
a parameter, in an assertion, or in a case clause — so the `go/types` rebuild this paragraph
proposed as the remedy for what it cannot see would find none of them either. **That is the third
round running that the remedy named here would have missed the next instance**, and it is the
reason the criterion this file is now held to is about literals and complements rather than about
which library a derivation is built on.

**And what is NOT here is as important as what is.** This list is what is asserted not found; it
is **not** a proof that nothing else is missing, and this document no longer implies otherwise.
Two things about it can be said honestly and are worth saying:

- **it grows by measurement, not by inspiration.** Every entry arrived because somebody planted a
  narrowing and watched the gates stay green. Nothing here was reasoned into existence, and a
  future entry will arrive the same way;
- **the entries a plant CAN reach are exactly the ones written down; the ones it cannot are not.**
  A shape nobody thought to plant is invisible to this list in precisely the way a member nobody
  thought to enumerate was invisible to the table this file replaced. The reader who wants the
  next instance should look where this file has three times found it: at what the derivation
  reads its class THROUGH — the doors in the seventh round, the descriptor's own fields in the
  eighth, the container and the binding statement in the ninth — rather than at the class itself.

Each row carries one verdict:

| verdict | what it means |
|---|---|
| `NARROWING/complement` | A name- or arity-shaped test that removes members from the class, and the gate **names every removed member at run time**. |
| `NARROWING/refusal` | A name- or arity-shaped test that removes members and **reports each one** (`t.Errorf`/`t.Fatalf`). Nothing leaves the class silently, so there is no complement to print — the failure is the print. |
| `CLASS/results` | Membership is decided by a reading of the member's own **result, argument or field TYPES**. That is the property itself rather than a narrowing of it, so there is no separate exclusion predicate to hold a complement. (The word was "result or argument" while the class was method-shaped; a field's own type is the same reading one door over.) |
| `DRIVER` | Walks a member's own arguments, results or fields, calls the member, recurses through it, or IS the assertion. It removes nobody: every member still reaches every rule. |
| `NOT-A-MEMBER` | The derivation over-reports here, and the row says why. **One row carries it**, added with the eighth instance: `sameRegistryVectors` reads `lf.Index(j)`, which is reflect.Value's ELEMENT reading and not the descriptor field spelled the same way — two of this derivation's stated over-reports meeting on one line. The other over-reporting sites share a (file, function, condition) key with an occurrence that IS a member, so they carry `DRIVER` and say so in the reading. |
| `OPEN` | Unmeasured. **This verdict is RED.** Recording a narrowing as open is not closing it: the two rows this table carried as OPEN at `81b97ca` were still open when the next reviewer arrived. |

<!-- gates-index:begin -->
| site | shape | narrowing | reading |
|---|---|---|---|
| `message/aad_test.go` `TestBodyBindingIsAStrictProjectionOfTheHeader` | shape | `headerField.Type != field.Type` | DRIVER — the assertion itself. Every field of BodyBinding is looked up on RecordHeader and one whose type differs is reported; it removes no field. |
| `message/attachment_test.go` `attachmentBodyFields` | shape | `declared.Type.Kind() != reflect.Pointer` | CLASS/results — membership is "the arms of a ServerAttachment", read off the field's own type: an arm is a pointer. An empty class is fatal below. |
| `message/attachment_test.go` `attachmentWidthFields` | shape | `field.Type.Kind() != reflect.Slice \|\| field.Type.Elem().Kind() != reflect.Uint8` | CLASS/results — membership is "a field of the attachment body that carries octets", read off the field's own type. That is the property rather than a narrowing of it. |
| `message/codec_agreement_test.go` `lpFieldValue` | name | `field.Name == name` | DRIVER — a lookup of the one field the caller named, over the whole structure. It decides no class. |
| `message/codec_agreement_test.go` `lpFieldValue` | shape | `field.Type.Kind() == reflect.Struct` | DRIVER — the recursion, so a field nested inside a struct is reached rather than dropped. It widens the walk. |
| `message/writeauth_test.go` `writeAuthCoveredNames` | shape | `field.Type == headerType` | DRIVER — decides whether a field contributes its own name or the twelve the header expands to; every field contributes. |
| `messagegroup/engineroles_test.go` `liveTreeOf` | shape | `field.Type != reflect.TypeOf((*mls.RatchetTree)(nil))` | NARROWING/refusal — fatal: the fixture that builds the identity-swap commit writes through mls.Group's unexported tree field, found by name, and a field of any other type is reported with a sentence rather than written through. It decides no class; it guards one write. |
| `messagegroup/engineroles_test.go` `liveTreeOf` | attribute | `tree == nil where tree = *(**mls.RatchetTree)(unsafe.Add(unsafe.Pointer(adapter.group), field.Offset))` | NARROWING/refusal — fatal: the live group holds no tree, which no fixture of this package produces, and the swap would otherwise dereference nil inside the test rather than name it. |
| `messagegroup/epoch_test.go` `TestAProvisionalEpochDeclaresNoFieldAbleToHoldACachedEnvKey` | name | `!slices.Contains(declared, name)` | NARROWING/refusal — the other direction: a row naming a field the type no longer declares is reported. |
| `messagegroup/epoch_test.go` `TestAProvisionalEpochDeclaresNoFieldAbleToHoldACachedEnvKey` | name | `isRowed where isRowed = epochProvisionalFields[name]` | NARROWING/refusal — a field with no row is reported on the else arm; nothing leaves silently. |
| `messagegroup/epoch_test.go` `TestEveryAccessorOfAProvisionalEpochAnswersTheValueItWasBuiltFrom` | name | `!isRowed where isRowed = epochAccessorAnswers[method.Name]` | NARROWING/refusal — an accessor with no row saying which of section 5.12 step 1's values it answers is reported. |
| `messagegroup/epoch_test.go` `TestEveryAccessorOfAProvisionalEpochRefusesOnceItHasBeenDestroyed` | shape | `!answersError where answersError = epochMethodResults(method.Type)` | CLASS/results — membership is "answers an error", read off the member's results. The members it does not admit are exactly the ones TestNoExportedAccessorOfAProvisionalEpochAnswersStateWithoutARefusal reports, so the complement is judged elsewhere rather than dropped. |
| `messagegroup/epoch_test.go` `TestEveryAccessorOfAProvisionalEpochRefusesOnceItHasBeenDestroyed` | shape | `j < bound.Type().NumIn()` | DRIVER — walks the member's own arguments to build the zero row it is driven with. |
| `messagegroup/epoch_test.go` `TestNoExportedAccessorOfAProvisionalEpochAnswersStateWithoutARefusal` | shape | `answersState && !answersError where answersError = epochMethodResults(method.Type) where answersState = epochMethodResults(method.Type)` | NARROWING/refusal — the complement of the row above, and it is reported rather than skipped: a member answering state with no error has no way to refuse once the destructor has run. |
| `messagegroup/epoch_test.go` `epochIsTheDestroyedFlag` | name | `!one.MethodByName(method.Name).Call(nil)[0].Bool()` | DRIVER — the same call over the destroyed set. |
| `messagegroup/epoch_test.go` `epochIsTheDestroyedFlag` | shape | `method.Type.NumIn() != 1 \|\| method.Type.NumOut() != 1 \|\| method.Type.Out(0).Kind() != reflect.Bool` | CLASS/results — RE-READ THIS ROUND, and the row was wrong before it: membership in the destroyed-flag exemption is decided by the member’s own signature, taking nothing and answering one bool, which is the property rather than a narrowing of it. It carried NARROWING/refusal, and it reports nothing at all — it answers false, and a member denied the exemption stays under every octet rule, so failing it widens what holds. `TestEveryRowClaimingAPrintedComplementHasOne` is what found that: the verdict claimed a refusal and the function calls no reporter. |
| `messagegroup/epoch_test.go` `epochIsTheDestroyedFlag` | name | `one.MethodByName(method.Name).Call(nil)[0].Bool()` | DRIVER — calls the member being classified, over the live set. |
| `messagegroup/epoch_test.go` `epochOctetAnsweringMethodsIn` | shape | `epochTypeCarriesOctets(method.Type.Out(i), map[reflect.Type]bool{})` | CLASS/results — membership is "some result of this member can carry an octet"; `epochSliceAnsweringAccessors` prints what it removed. The fifth instance, closed. |
| `messagegroup/epoch_test.go` `epochOctetAnsweringMethodsIn` | shape | `i < method.Type.NumOut()` | DRIVER — walks the member's own results. |
| `messagegroup/epoch_test.go` `epochOctetAnsweringMethodsIn` | shape | `method.Type.Out(i) == errorType` | DRIVER — skips one RESULT, the error, and never a member. |
| `messagegroup/epoch_test.go` `epochTypeCarriesOctets` | shape | `epochTypeCarriesOctets(carrier.Field(i).Type, seen)` | DRIVER — the reachability walk descends into every field and removes none. |
| `messagegroup/noncerebind_test.go` `noncerebindCopyOctetsInto` | shape | `field.Kind() != reflect.Slice \|\| field.Type().Elem().Kind() != reflect.Uint8 \|\| field.IsNil()` | DRIVER — the snapshot builder for the re-auth's one-field property, and it removes nobody: it walks the record's OWN fields, copies the ones that carry octets so the snapshot is octets rather than a window onto the record, and recurses into a struct field so the header's slices are reached too. Every field of message.Record still reaches `noncerebindMovedFields`, and the class that property is stated over is `noncerebindRecordFields`, which admits all of them. The nil arm is why a field that moved between nil and empty is still reported. |
| `messagegroup/noncerebind_test.go` `noncerebindMovedFields` | shape | `j < inner.NumField()` | DRIVER — walks the header's own fields so a difference inside it is reported as `Header.BodyHash` rather than as `Header`; it removes nobody, and a header that differs in no named member is still reported, as `Header` itself, on the `named == 0` arm. |
| `messagegroup/ratchet_test.go` `TestNoFieldOfAStreamKeyIsSomethingACallerCanWriteThrough` | shape | `field.Type.Kind()` | NARROWING/refusal — every field's kind is judged and the default arm reports, so a shape nobody wrote a case for fails rather than passing. |
| `messagegroup/seal_test.go` `TestBodyHashIsTheHashOfTheSealedBodyAndIsNotInTheBodyAad` | name | `strings.Contains(strings.ToLower(field.Name), "hash")` | NARROWING/refusal — a name-shaped predicate that REPORTS the members it selects. Nothing is removed from any rule by it. |
| `mls/caller_arrays_test.go` `groupAnswerDeclaresStorage` | shape | `len(byteStoragePathsOf(method.Type.Out(at), name)) != 0` | DRIVER — walks the member's own results. |
| `mls/caller_arrays_test.go` `groupInjectedObjects` | shape | `field.IsExported() && field.Type.Kind() == reflect.Interface` | CLASS/results — membership is "a field a caller supplies an object through", read off the field's own type and its exportedness; an empty class is fatal. |
| `mls/caller_arrays_test.go` `typeReachesByteStorageThrough` | shape | `typeReachesByteStorageThrough(field.Type(), entered)` | DRIVER — the reachability walk into every exported field. |
| `mls/commit_vector_join_test.go` `TestEveryExportedMethodOfAProposalCacheRefusesANilCacheRatherThanPanicking` | shape | `!known where known = arguments[parameter]` | NARROWING/refusal — an argument type this sweep has no value for is fatal, rather than driven with a zero value that a nil receiver would survive for the wrong reason. |
| `mls/commit_vector_join_test.go` `TestEveryExportedMethodOfAProposalCacheRefusesANilCacheRatherThanPanicking` | shape | `at < method.Type.NumIn()` | DRIVER — walks the member's own arguments. |
| `mls/commit_vector_join_test.go` `TestEveryExportedMethodOfAProposalCacheRefusesANilCacheRatherThanPanicking` | shape | `method.Type.Out(at) != reflect.TypeFor[error]()` | DRIVER — finds the error among the member's results. |
| `mls/commit_vector_join_test.go` `TestEveryExportedMethodOfAProposalCacheRefusesANilCacheRatherThanPanicking` | shape | `method.Type.Out(at).Kind()` | DRIVER — reads one result's kind to decide how to read it back. |
| `mls/crypto_labels_test.go` `TestEveryPublishedFieldOfTheKeyScheduleCorpusIsDecodedAndRead` | name | `!slices.Contains(readings, name)` | NARROWING/refusal — a published field nothing reads off a corpus epoch is reported. |
| `mls/crypto_labels_test.go` `TestEveryPublishedFieldOfTheKeyScheduleCorpusIsDecodedAndRead` | attribute | `!tagged where tagged = field.Tag.Lookup("json")` | NARROWING/refusal — the eighth instance's own shape, and the one site in these trees that already had it right: a field of `labelKatEpoch` carrying no json tag is FATAL, so nothing leaves the decoded class in silence. |
| `mls/crypto_labels_test.go` `theEpochsFieldOf` | shape | `field.Type.Kind() == reflect.Slice && field.Type.Elem() == epoch` | CLASS/results — the field is found by its own type, and a type declaring no such field is fatal. |
| `mls/crypto_test.go` `providerStructByteFields` | shape | `(field.Kind() == reflect.Slice \|\| field.Kind() == reflect.Array) && field.Type().Elem().Kind() == reflect.Uint8` | CLASS/results — membership is "this field carries octets", read off the field's own type. The array arm is in the same case as the slice because reading only the slice made a fixed width field render as no bytes at all. |
| `mls/crypto_test.go` `providerStructByteFields` | shape | `field.Kind() == reflect.Map && field.Type().Elem().Kind() == reflect.Slice && field.Type().Elem().Elem().Kind() == reflect.Uint8` | CLASS/results — the map arm of the same "carries octets" reading. |
| `mls/crypto_test.go` `providerStructByteFields` | shape | `field.Kind() == reflect.Slice && field.Type().Elem().Kind() == reflect.Struct` | DRIVER — descends into every entry of a vector, in order; it removes nobody. |
| `mls/epoch_advance_test.go` `TestEveryWriterOfTheProposalCacheBindingIsClassifiedHere` | shape | `at < method.Type.NumIn()` | DRIVER — walks the member's own arguments. |
| `mls/epoch_advance_test.go` `TestEveryWriterOfTheProposalCacheBindingIsClassifiedHere` | shape | `reflectTypeReaches(method.Type.In(at), []reflect.Type{reflect.TypeOf(&VerifiedGroupContext{})})` | CLASS/results — membership is what the member's argument types reach. |
| `mls/epoch_advance_test.go` `TestEveryWriterOfTheProposalCacheBindingIsClassifiedHere` | shape | `reflectTypeReaches(method.Type.In(at), reachGroupContextTargets)` | CLASS/results — the same reading against the derived target set. |
| `mls/epoch_advance_test.go` `epochCachesHeldBy` | shape | `extensionTypeSelectionNamedAs(field.Type(), cacheType)` | CLASS/results — membership is read off the field's own type. |
| `mls/extension_test.go` `TestSupportsEnforcesEveryEntryOfEveryRequirementVector` | shape | `mandatoryToImplement[carveOutKey(registry, code)]` | DRIVER — `registry` is a string read off a field's element type, and the derivation carries "anything reached from a member's signature" forward — its stated over-report. The condition chooses which of two assertions a dropped code point gets and removes no field. |
| `mls/extension_test.go` `capabilityPredicates` | shape | `method.Type.NumIn() != 2 \|\| method.Type.In(1) != registry` | NARROWING/complement — CLOSED this round. The `!strings.HasPrefix(method.Name, "Supports")` that used to open this loop is gone: measured, its complement was EMPTY, because MarshalMLS, UnmarshalMLS and Supports are all removed by these two shape clauses already. The removed members are now printed with their signatures. |
| `mls/extension_test.go` `capabilityPredicates` | shape | `method.Type.NumOut() != 1 \|\| method.Type.Out(0).Kind() != reflect.Bool` | NARROWING/complement — the other half of the same shape; complement printed. |
| `mls/extension_test.go` `capabilityPredicates` | name | `selected[method.Name]` | DRIVER — the complement printer itself, walking the method set to name what no field paired with. |
| `mls/extension_test.go` `generatedRegistryStructs` | shape | `got != typeName` | NARROWING/refusal — a field whose element type is not the name given is fatal. |
| `mls/extension_test.go` `requiredCapabilityFields` | shape | `capabilities.Field(j).Type == field.Type` | CLASS/results — the pairing is decided by the two members' own types, and a pairing that is not exactly one is fatal. |
| `mls/extension_test.go` `sameRegistryVectors` | attribute | `lf.Index(j).Uint() != rf.Index(j).Uint()` | NOT-A-MEMBER — `Index` here is reflect.Value's ELEMENT reading and not the descriptor field of that spelling: `lf` is `l.Field(i)`, a Value, and `lf.Index(j)` is its j'th element. Two stated over-reports meet on one line — the Value-half door admitted because it takes a field door's arguments, and the selector admitted because a descriptor carries a field spelled that way. The first row to carry this verdict. |
| `mls/external_provenance_test.go` `TestNoKeyScheduleAnswersAVerifiedGroupContext` | shape | `method.Type.Out(at) != verified` | DRIVER — walks results looking for the forbidden one; every member is looked at. |
| `mls/external_provenance_test.go` `TestTheOnlyExportedDoorOntoAVerifiedGroupContextIsAVerifiedGroupInfo` | shape | `door.Type.In(at) == tree` | DRIVER — walks one named door's own arguments. |
| `mls/external_provenance_test.go` `externalDoorsOntoAVerifiedGroupContext` | shape | `!isSignature where isSignature = method.Type().(*types.Signature)` | DRIVER — three occurrences share this key. Two read a *types.Object out of a package scope and are not members at all — the derivation binds an identifier for the whole function rather than the block it is scoped to, which is its stated over-report and why the predicate text here is carried from the third. The third guards a type assertion over a go/types method set whose complement is empty by construction, a *types.Func's type being always a signature, so it removes nobody. |
| `mls/external_provenance_test.go` `externalShadowHasThisTypesShape` | name+shape | `mine.Name() != theirs.Name() \|\| mine.Embedded() != theirs.Embedded() \|\| shadowStruct.Tag(at) != realStruct.Tag(at) \|\| !types.Identical(mine.Type(), theirs.Type())` | DRIVER — the assertion itself, walking both structs position by position; a difference is reported and no field is removed. |
| `mls/framing_guard_test.go` `comparesOctets` | shape | `comparesOctets(spelled.Field(index).Type(), seen)` | DRIVER — the walk descends into every field of a struct. |
| `mls/framing_protect_test.go` `TestEveryRegisteredContentTypeEncodesToThePrivateMessageContentLayoutSection631Writes` | name | `name == "ContentType" \|\| slices.Contains(arms, name)` | NARROWING/complement — CLOSED THIS ROUND: the removed set is the derived arm class plus the selector, and it is now printed beside the complement it builds. The arm class is itself asserted equal to the layout table three lines above, and an empty complement is fatal. |
| `mls/framing_protect_test.go` `TestTheSenderDataAadCoversExactlyTheHeaderFieldsItsParameterListNames` | name | `slices.Contains(covered, strings.ToLower(name[:1])+name[1:])` | DRIVER — every field of PrivateMessage is rewritten and observed above; this decides only which side of an equality the field lands on, and the two sides are compared, so a field left out of `want` fails here if it turns out to be covered. |
| `mls/framing_protect_test.go` `framedContentArmFields` | name | `!elsewhere[name]` | NARROWING/complement — CLOSED THIS ROUND, and it is the live member the SEVENTH instance was found by: a name narrowing over a reflected FIELD set, invisible to this file for six rounds because the derivation read method sets only. The removed members are now printed beside the class. One seed is an identifier of the tree — `"Sender"` is put out by hand because section 6.3.2 carries the sender in the ENCRYPTED SENDER DATA rather than in the content, and no type in this package holds that placement as a field the join could read; the rest of the removed set is derived from PrivateMessage's own fields. |
| `mls/framing_protect_test.go` `framingPreimageStructTypes` | shape | `field.Kind() == reflect.Struct` | DRIVER — queues a nested structure so the sweep reaches it; every field is still emitted. |
| `mls/framing_protect_test.go` `perturbFramedContentField` | shape | `field.Type() == reflect.TypeOf(Sender{})` | NARROWING/refusal — the other arm of the same switch. |
| `mls/framing_protect_test.go` `perturbFramedContentField` | shape | `field.Type() == reflect.TypeOf([]byte(nil))` | NARROWING/refusal — one arm of a switch whose default is fatal, so a field shape nobody wrote a move for fails rather than going unmoved. |
| `mls/framing_test.go` `TestTheAuthDataCodecWritesEveryFieldItsContentTypeCarriesAndNoOther` | name | `!carried && changed where carried = slices.Contains(authDataVariantPaths[contentType], name)` | NARROWING/refusal — the other direction of the same assertion. |
| `mls/framing_test.go` `TestTheAuthDataCodecWritesEveryFieldItsContentTypeCarriesAndNoOther` | name | `!observed[name]` | NARROWING/refusal — a field no declared content type's encoding depends on is reported. |
| `mls/framing_test.go` `TestTheAuthDataCodecWritesEveryFieldItsContentTypeCarriesAndNoOther` | name | `carried && !changed where carried = slices.Contains(authDataVariantPaths[contentType], name)` | NARROWING/refusal — an assertion, reported. |
| `mls/framing_test.go` `TestTheSenderCodecWritesEveryFieldItsArmCarriesAndNoOther` | name | `!carried && changed where carried = slices.Contains(senderVariantPaths[senderType], name)` | NARROWING/refusal — the other direction of the same assertion. |
| `mls/framing_test.go` `TestTheSenderCodecWritesEveryFieldItsArmCarriesAndNoOther` | name | `!observed[name]` | NARROWING/refusal — a field no declared sender type's encoding depends on is reported. |
| `mls/framing_test.go` `TestTheSenderCodecWritesEveryFieldItsArmCarriesAndNoOther` | name | `carried && !changed where carried = slices.Contains(senderVariantPaths[senderType], name)` | NARROWING/refusal — an assertion, reported. |
| `mls/framing_test.go` `TestTheSenderCodecWritesEveryFieldItsArmCarriesAndNoOther` | name | `name == senderDiscriminantField` | NARROWING/complement — CLOSED THIS ROUND: the removed member is now printed beside the swept class. It is one member, named by a constant of this file, and the discriminant is covered by the golden assertion at the end of this test rather than by the sweep — which the test states and then checks rather than assuming. |
| `mls/framing_test.go` `TestTheSenderVariantTableCoversTheTypeAndTheRegistry` | name | `claimed[name]` | NARROWING/refusal — every field is judged by how many arms carry it, and both 0 and more than 1 are reported. |
| `mls/framing_test.go` `TestTheSenderVariantTableCoversTheTypeAndTheRegistry` | name | `claimed[name] != 0` | NARROWING/refusal — reported. |
| `mls/framing_test.go` `TestTheSenderVariantTableCoversTheTypeAndTheRegistry` | name | `name == senderDiscriminantField` | NARROWING/refusal — the discriminant is not skipped here: it is asserted to be carried by no arm and then deleted from the table, so the member is judged rather than removed. |
| `mls/framing_test.go` `decodedFormOfAuthData` | name | `slices.Contains(carried, name)` | DRIVER — decides whether a field is zeroed or kept while building the decoded form; every field is written one way or the other. |
| `mls/framing_test.go` `mlsMessageArmFields` | shape | `structure.Field(at).Type.Kind() == reflect.Pointer` | CLASS/results — an arm is a pointer field, read off the type; an empty class is fatal. |
| `mls/group_context_test.go` `TestGroupContextCloneIsDeepAtEveryWritableLocation` | name | `strings.HasPrefix(path, "GroupContext."+name)` | NARROWING/refusal — a field no exercised writable location sits under is reported. |
| `mls/group_context_verified_test.go` `TestEveryConstructionOfAVerifiedGroupContextIsClassifiedHere` | name | `field.Name != verifiedGroupContextFieldName` | NARROWING/refusal — fatal, because the derivation downstream reads that spelling and a rename would empty its class. |
| `mls/group_context_verified_test.go` `TestEveryConstructionOfAVerifiedGroupContextIsClassifiedHere` | shape | `field.Type != reflect.TypeOf((*GroupContext)(nil))` | NARROWING/refusal — fatal: a value there rather than a pointer would make the zero value read as the empty group at epoch 0. |
| `mls/group_context_verified_test.go` `TestNoMethodOfAVerifiedGroupContextHandsOutTheStorageItVouchesFor` | shape | `method.Type.NumIn() != 1` | NARROWING/refusal — an argument-taking member is reported with `t.Errorf`, not skipped. |
| `mls/group_context_verified_test.go` `TestNoMethodOfAVerifiedGroupContextHandsOutTheStorageItVouchesFor` | shape | `reflectTypeReaches(method.Type.Out(at), reachGroupContextTargets)` | DRIVER — walks the member's own results. |
| `mls/group_test.go` `framedContentCarrier` | shape | `structure.Field(i).Type == declared` | CLASS/results — the carrier is found by the field's own type, and a second field of that type answers the empty string, which the reading's own gate refuses. |
| `mls/key_package_test.go` `mlsEncodingEmitters` | shape | `method.Type.NumIn() > 1` | DRIVER — the independent shape reading that proves the row above's complement; it decides no membership on its own. |
| `mls/key_package_test.go` `mlsEncodingEmitters` | name | `strings.HasPrefix(method.Name, "Write")` | NARROWING/complement — CLOSED this round, and the first of the three the receiver-keyed query missed. Its complement is NOT empty: `Bytes`, `Err`, `Len` and `MaxVectorLength`, four members removed in silence for a round. They are now printed, and the sentence that puts them out — "they take nothing and answer the writer's accumulated state" — is read off the type as a SHAPE and required to name the same set. |
| `mls/key_schedule_kat_test.go` `TestNoVectorRunnerCanSkip` | name | `strings.HasPrefix(name, "Skip")` | NARROWING/complement — the second site the receiver-keyed query missed. The name IS the property here: `*testing.T` offers no shape that tells `Skip` from `Log`, and the reading is anchored in both directions (`Skip`, `Skipf`, `SkipNow` must be in; `Fatal`, `Fatalf` must be out). The members it removes are now printed. |
| `mls/key_schedule_roundtrip_test.go` `seedMoveFieldAt` | name | `seedMoveFieldAt(value.Field(index), at+"."+field.Name, want)` | DRIVER — the recursive search over every exported field. |
| `mls/key_schedule_roundtrip_test.go` `seedValuesAgree` | name | `!seedValuesAgree(t, path+"."+field.Name, left.Field(index), right.Field(index))` | DRIVER — the recursive comparison over every exported field. |
| `mls/key_schedule_test.go` `TestAnErasedScheduleRefusesRatherThanAnsweringFromZeros` | shape | `method.Type.NumOut() == 0` | NARROWING/complement — the erasers, which answer nothing over a live epoch and so have no refusal to observe. They are now named at run time; the gate previously counted only the class it kept. |
| `mls/key_schedule_test.go` `TestEveryAccessorAnsweringAPointerAnswersIntoTheSchedulesOwnStorage` | shape | `method.Type.NumIn() != 1` | NARROWING/refusal — an argument-taking member is refused out loud rather than skipped. Closed with the fifth instance. |
| `mls/key_schedule_test.go` `TestEveryAccessorAnsweringAPointerAnswersIntoTheSchedulesOwnStorage` | shape | `method.Type.Out(at).Kind() == reflect.Pointer` | DRIVER — walks the member's own results collecting the pointer positions; every result is a row. |
| `mls/key_schedule_test.go` `TestNoExportedMethodOfThisPackageCanReachTheEpochSecret` | shape | `!driven && method.Type.NumIn() != 1` | NARROWING/refusal — an exemption resting on a sweep that drives nothing is reported. |
| `mls/key_schedule_test.go` `bytesTheGroupHandsOut` | name | `!driven where driven = groupMethodArgumentRows[method.Name]` | NARROWING/refusal — an argument-taking method with no rows is fatal unless an excuse is written down for it. |
| `mls/key_schedule_test.go` `bytesTheGroupHandsOut` | name | `!excused where excused = groupMethodsTakingArguments[method.Name]` | NARROWING/refusal — the excuse itself; without one the method is fatal. |
| `mls/key_schedule_test.go` `bytesTheGroupHandsOut` | shape | `!value.Type().AssignableTo(want)` | DRIVER — the same check over *Group's rows. |
| `mls/key_schedule_test.go` `bytesTheGroupHandsOut` | name | `driven where driven = groupMethodArgumentRows[method.Name]` | NARROWING/refusal — the other direction: rows for a method that takes no arguments are reported. |
| `mls/key_schedule_test.go` `bytesTheGroupHandsOut` | shape | `len(row)+1 != method.Type.NumIn()` | DRIVER — row width against the member's arity. |
| `mls/key_schedule_test.go` `bytesTheGroupHandsOut` | shape | `method.Type.NumIn() != 1` | DRIVER — chooses the argument rows. |
| `mls/key_schedule_test.go` `bytesTheGroupHandsOut` | shape | `method.Type.NumIn() == 1 && answersOnlyErrors(method.Type)` | NARROWING/complement — the removed members are now printed as well as counted; the non-empty check said the exclusion fired but never said what it removed. |
| `mls/key_schedule_test.go` `bytesTheScheduleHandsOut` | name | `!driven where driven = keyScheduleMethodArgumentRows[method.Name]` | NARROWING/refusal — an argument-taking method with no rows is fatal unless an excuse is written down for it. |
| `mls/key_schedule_test.go` `bytesTheScheduleHandsOut` | name | `!excused where excused = keyScheduleMethodsTakingArguments[method.Name]` | NARROWING/refusal — the excuse itself; without one the method is fatal. |
| `mls/key_schedule_test.go` `bytesTheScheduleHandsOut` | shape | `!value.Type().AssignableTo(want)` | DRIVER — checks one argument-row value against the member's declared argument type; it removes no member. Found only after the shape reading below stopped being a list of seven names. |
| `mls/key_schedule_test.go` `bytesTheScheduleHandsOut` | name | `driven where driven = keyScheduleMethodArgumentRows[method.Name]` | NARROWING/refusal — the other direction: rows for a method that takes no arguments are reported. |
| `mls/key_schedule_test.go` `bytesTheScheduleHandsOut` | shape | `len(row)+1 != method.Type.NumIn()` | DRIVER — checks a row's width against the member's arity before calling, so a mismatch names the method rather than panicking inside reflect. |
| `mls/key_schedule_test.go` `bytesTheScheduleHandsOut` | shape | `method.Type.NumIn() != 1` | DRIVER — chooses the argument rows; an argument-taking member with no rows and no written excuse is fatal. |
| `mls/key_schedule_test.go` `bytesTheScheduleHandsOut` | shape | `method.Type.NumIn() == 1 && method.Type.NumOut() == 0` | NARROWING/complement — an eraser answers nothing and so hands nothing out; the removed members are printed as `notCalled`. |
| `mls/key_schedule_test.go` `bytesTheScheduleKeeps` | name | `!read where read = scheduleStorageReaders[name]` | NARROWING/refusal — fatal: a kept []byte field with no reader falls outside every comparison this gate makes. |
| `mls/key_schedule_test.go` `bytesTheScheduleKeeps` | name | `!slices.Contains(fields, name)` | NARROWING/refusal — the other direction: a reader for a field the type does not declare is reported. |
| `mls/key_schedule_test.go` `bytesTheScheduleKeeps` | shape | `answer.result >= method.Type.NumOut()` | NARROWING/refusal — an excuse for a result position the method does not have is reported, because an excuse that can never fire leaves the table looking complete. |
| `mls/key_schedule_test.go` `bytesTheScheduleKeeps` | shape | `valueType.Field(i).Type != byteSlice` | CLASS/results — membership is "a []byte field of the schedule", read off the field's own type; a member of it with no reader is fatal on the next line. |
| `mls/key_schedule_test.go` `bytesTheStagedCommitHandsOut` | shape | `method.Type.NumIn() != 1` | DRIVER — chooses the argument rows, as bytesTheGroupHandsOut does; until 2026-09-21 this was the fatal itself, and LeafIdentityAfter is the first accessor of the type to take a parameter. |
| `mls/key_schedule_test.go` `bytesTheStagedCommitHandsOut` | name | `!driven where driven = stagedCommitMethodArgumentRows[method.Name]` | NARROWING/refusal — an argument-taking method with no rows is fatal; there is no excuse table for this type, so a parameter costs a row. |
| `mls/key_schedule_test.go` `bytesTheStagedCommitHandsOut` | shape | `len(row)+1 != method.Type.NumIn()` | DRIVER — row width against the member's arity. |
| `mls/key_schedule_test.go` `bytesTheStagedCommitHandsOut` | shape | `!value.Type().AssignableTo(want)` | DRIVER — the same check over *StagedCommit's rows. |
| `mls/key_schedule_test.go` `bytesTheStagedCommitHandsOut` | name | `driven where driven = stagedCommitMethodArgumentRows[method.Name]` | NARROWING/refusal — the other direction: rows for a method that takes no arguments are reported. |
| `mls/key_schedule_test.go` `epochSecretsByField` | shape | `field.Type() != byteSlice` | NARROWING/refusal — fatal: a secret held in anything but a []byte would fall outside every sweep over the derived secrets, so it is reported rather than skipped. |
| `mls/key_schedule_test.go` `mutatedGroupContexts` | shape | `target.Kind() == reflect.Slice && target.Type().Elem() == extensionType` | NARROWING/refusal — the other arm of the same switch. |
| `mls/key_schedule_test.go` `mutatedGroupContexts` | shape | `target.Kind() == reflect.Slice && target.Type().Elem().Kind() == reflect.Uint8` | NARROWING/refusal — one arm of a switch whose default is fatal, so a field this gate cannot move fails rather than going unjudged. |
| `mls/key_schedule_test.go` `scheduleMethodResults` | shape | `method.Type.NumIn() != 1` | DRIVER — chooses the argument rows; undriven is fatal. |
| `mls/key_schedule_test.go` `tagVerifierPairs` | name | `!strings.HasPrefix(verify.Name, "Verify")` | NARROWING/refusal — a bool-answering method not named `Verify<something>` is reported, not skipped, so guardrail 7 cannot be left by a rename. |
| `mls/key_schedule_test.go` `tagVerifierPairs` | shape | `compute.Type.NumIn() != 2 \|\| compute.Type.In(1) != byteSlice \|\| compute.Type.NumOut() != 1 \|\| compute.Type.Out(0) != byteSlice` | NARROWING/refusal — reported. |
| `mls/key_schedule_test.go` `tagVerifierPairs` | shape | `verify.Type.NumIn() != 3 \|\| verify.Type.In(1) != byteSlice \|\| verify.Type.In(2) != byteSlice` | NARROWING/refusal — reported. |
| `mls/key_schedule_test.go` `tagVerifierPairs` | shape | `verify.Type.NumOut() != 1 \|\| verify.Type.Out(0).Kind() != reflect.Bool` | CLASS/results — the class is "answers exactly one bool", read off the results; the members that do are logged beside the pairs. |
| `mls/leaf_node_test.go` `TestLeafNodeValidateEnforcesEveryRequiredCapabilitiesVector` | name | `!written where written = byField[name]` | NARROWING/refusal — a field of RequiredCapabilities with no row is reported. |
| `mls/leaf_node_test.go` `leafNodeFieldPathsOf` | shape | `field.Type.Kind() == reflect.Struct && !leafNodeFieldIsDelegated(field.Type)` | DRIVER — decides whether a field is descended into or emitted as a leaf path; every field yields at least one path. |
| `mls/message_protection_kat_test.go` `TestMessageProtectionVectorDecodesEveryColumnTheCorpusPublishes` | attribute | `mineKey != key where key = strings.Cut(field.Tag.Get("json"), ",") where mineKey = strings.Cut(mine.Tag.Get("json"), ",")` | NARROWING/refusal — two structs decoding one corpus row are held key against key, and a disagreement is reported rather than skipped. |
| `mls/message_protection_kat_test.go` `messageProtectionColumnTags` | attribute | `key == "" where key = strings.Cut(shape.Field(index).Tag.Get("json"), ",")` | NARROWING/refusal — RESTATED THIS ROUND. It read `if key != "" { tags = append(tags, key) }`: a field carrying no json tag left the column table in silence and its complement was EMPTY, which the table above calls the dangerous case. `encoding/json` decodes an untagged field under its GO NAME, so the column exists and this table, whose whole claim is "every json key this struct decodes", would not name it. Now fatal. |
| `mls/message_protection_kat_test.go` `theGroupContextParameters` | shape | `isSignature where isSignature = method.Type().(*types.Signature)` | DRIVER — the same shape as externalDoorsOntoAVerifiedGroupContext's row: two occurrences, one over a scope object that is not a member and one over a go/types method set whose complement is empty by construction. The predicate text is carried across blocks by the derivation's stated function-wide binding. |
| `mls/messages_kat_test.go` `TestMessagesCodecsReadTheColumnTheyAreNamedAfter` | attribute | `key == "" where key = strings.Cut(shape.Field(index).Tag.Get("json"), ",")` | NARROWING/refusal — RESTATED THIS ROUND, for `messagesColumnTags`' reason: an untagged field was skipped and left empty in the marker, and every codec reading it would then answer the empty string — which reads here as a codec naming the wrong column. Now fatal. |
| `mls/messages_kat_test.go` `messagesColumnTags` | attribute | `key == "" where key = strings.Cut(shape.Field(index).Tag.Get("json"), ",")` | NARROWING/refusal — RESTATED THIS ROUND. The same shape and the same empty complement as `messageProtectionColumnTags`; an untagged field is now fatal rather than skipped. |
| `mls/proposal_ceiling_test.go` `TestTheCachesAccountingIsAlwaysAViewOfTheEntriesItHolds` | name | `exempt \|\| compared[name] where exempt = entries[name]` | NARROWING/refusal — a field of the cache that neither table accounts for is reported, and the loop below reports a table key with no field, so both directions are judged. |
| `mls/proposal_ceiling_test.go` `testArmNamesALeaf` | shape | `arm.Field(i).Type() == names` | CLASS/results — "the arm names a leaf" is read off the field's own type. |
| `mls/proposal_ceiling_test.go` `testArmReplacesItsSendersLeaf` | shape | `arm.Field(i).Type() == replaces` | CLASS/results — "the arm replaces its sender's leaf" is read off the field's own type. |
| `mls/proposal_list_derivation_test.go` `TestAProposalListKeepsItsProposalsInExactlyOnePlace` | shape | `held.Type != want` | NARROWING/refusal — reported: anything but a []CachedProposal has lost the commit order. |
| `mls/proposal_list_derivation_test.go` `TestAProposalListKeepsItsProposalsInExactlyOnePlace` | name | `unicode.IsUpper([]rune(held.Name)[0])` | NARROWING/refusal — an exported storage field is reported. |
| `mls/proposal_list_derivation_test.go` `TestEveryPerTypeViewOfAProposalListIsItsCommitOrderFiltered` | name | `!joined where joined = carriedBy[method.Name]` | NARROWING/refusal — a view nothing names the filtered type of is reported. |
| `mls/proposal_list_derivation_test.go` `TestEveryPerTypeViewOfAProposalListIsItsCommitOrderFiltered` | name | `entry.Proposal.ProposalType != carries where carries = carriedBy[method.Name]` | DRIVER — IS the assertion: a view answering a proposal of another type is reported, and no member is removed. |
| `mls/proposal_list_derivation_test.go` `TestEveryPerTypeViewOfAProposalListIsItsCommitOrderFiltered` | name | `entry.Proposal.ProposalType == carries where carries = carriedBy[method.Name]` | DRIVER — builds the sequence the view is COMPARED against, filtering the commit order rather than the member set. Every view still reaches every rule. |
| `mls/proposal_list_derivation_test.go` `proposalListStorageFields` | shape | `carries(held.Field(i).Type, entered)` | DRIVER — the reachability walk into every field. |
| `mls/proposal_list_derivation_test.go` `proposalListStorageFields` | shape | `carries(structure.Field(i).Type, map[reflect.Type]bool{})` | CLASS/results — membership is "this field can carry a cached proposal", read off the field's own type. |
| `mls/proposal_list_test.go` `proposalListViewAnswer` | shape | `at < bound.Type().NumIn()` | DRIVER — walks the member's own arguments to build the zero row. An argument-taking view is DRIVEN here rather than dropped, which is what closing the row above required. |
| `mls/proposal_list_test.go` `proposalListViewMethods` | shape | `at < signature.NumOut()` | DRIVER — walks the member's own results. |
| `mls/proposal_list_test.go` `proposalListViewMethods` | name | `method.Name == commitOrder` | NARROWING/complement — one member, `All`, removed by name because it answers the commit order rather than a view of it. Proved non-empty by a fatal if the type stops declaring it, and now named in the printed complement. |
| `mls/proposal_list_test.go` `proposalListViewMethods` | shape | `signature.Out(at) == entries` | CLASS/results — CLOSED this round. Membership is "some result of this member is a `[]CachedProposal`", in any position and whatever sits beside it. The `NumIn() != 1 \|\| NumOut() != 1` that used to open it had an EMPTY complement — every non-view is removed by this type test — and would have silently dropped a view answering `([]CachedProposal, error)` or taking a filter. |
| `mls/proposal_wire_test.go` `proposalArmFields` | shape | `field.Type.Kind()` | CLASS/results — an arm is a pointer or a slice, read off the field's own type. The discriminant falls out by that same reading rather than by name, so a discriminant renamed does not fall in and an eighth arm added does; fewer than two arms is fatal. |
| `mls/proposal_wire_test.go` `proposalOrRefArmFields` | shape | `field.Type.Kind()` | CLASS/results — the same reading over ProposalOrRef. |
| `mls/provider_methods_test.go` `reachesByteSliceType` | shape | `reachesByteSliceType(under.Field(i).Type(), visiting)` | DRIVER — the reachability walk into every field. |
| `mls/psk_test.go` `providerPskInputBytePaths` | shape | `field.Type.Kind() == reflect.Slice && field.Type.Elem().Kind() == reflect.Uint8` | CLASS/results — membership is "a field carrying octets", read off the field's own type. |
| `mls/psk_test.go` `providerPskInputBytePaths` | shape | `field.Type.Kind() == reflect.Struct` | DRIVER — descends into a nested structure so its octet fields join the same class. |
| `mls/secret_tree_test.go` `TestEveryExportedSecretTreeMethodRefusesAfterZeroize` | shape | `at < method.Type.NumIn()` | DRIVER — walks the member's own arguments. |
| `mls/secret_tree_test.go` `TestSecretTreeCachedGeometryIsDerivedFromTheLeafCount` | name | `!onTheType[name]` | NARROWING/refusal — a table naming a field the type does not declare is reported. |
| `mls/secret_tree_test.go` `TestSecretTreeCachedGeometryIsDerivedFromTheLeafCount` | name | `geometry == state where geometry = secretTreeGeometryFields[name] where state = secretTreeStateFields[name]` | NARROWING/refusal — a field of `SecretTree` judged by both tables or by neither is reported; nothing leaves the comparison silently. |
| `mls/secret_tree_test.go` `TestSecretTreeCachedGeometryIsDerivedFromTheLeafCount` | name | `got != want where want = secretTreeGeometryFields[name](t, n)` | DRIVER — IS the assertion. The `name` it reads is the geometry table's own key and not a member's name at all; the derivation binds an identifier for the whole function it is bound in, which is its first stated over-report. |
| `mls/secret_tree_test.go` `stMethodsAnsweringBytes` | shape | `method.Type.Out(result) == byteSlice` | CLASS/results — membership is "some result of this member is a byte slice". |
| `mls/secret_tree_test.go` `stMethodsAnsweringBytes` | shape | `result < method.Type.NumOut()` | DRIVER — walks the member's own results. |
| `mls/transcript_test.go` `trRecordLayerCodecMethods` | name | `strings.HasSuffix(name, "LP")` | NARROWING/complement — the third site the receiver-keyed query missed. The suffix is the naming rule `encode.go` states (LP is the master design's notation for a fixed 32-bit big-endian length), and the twenty-odd methods it removes are now named rather than counted. |
| `mls/tree_sync_test.go` `TestEveryFactBothContextsCarryIsReconciled` | shape | `carried[pinned.Field(i).Type]` | CLASS/results — the class is "a group context field whose type the tree context also carries", read off both types. It is logged at run time and an empty class is fatal. |
| `mls/treekem_test.go` `fallibleProviderMethods` | shape | `at < method.Type.NumOut()` | DRIVER — walks the member's own results. |
| `mls/treekem_test.go` `fallibleProviderMethods` | shape | `method.Type.Out(at) == failure` | CLASS/results — membership is "some result of this member is an error". |
| `mls/type_reach_test.go` `TestTheCompiledTypeReachWalkEntersEveryConstructorItClaims` | shape | `reflectTypeReaches(field.Type, targets)` | CLASS/results — membership is what the field's own type reaches, and the answer is compared against the control's declared set in both directions. |
| `mls/type_reach_test.go` `reflectTypeReachesThrough` | shape | `reflectTypeReachesThrough(field.Type, targets, entered)` | DRIVER — the reflect half of the same walk, into every exported field. |
| `mls/type_reach_test.go` `reflectTypeReachesThrough` | shape | `reflectTypeReachesThrough(found.Method(at).Type, targets, entered)` | DRIVER — the reachability walk descends into every member of an interface's method set and removes none. This row exists because the derivation stopped enumerating the readings that count as a signature test: `Method(at).Type` handed to a function matches none of the seven names the first draft listed. |
| `mls/type_reach_test.go` `typeReachesNamedThrough` | shape | `!isSignature where isSignature = shape.Method(at).Type().(*types.Signature)` | DRIVER — a type assertion guard over a go/types method set; a *types.Func's type is always a signature, so its complement is empty by construction and it removes nobody. |
| `mls/type_reach_test.go` `typeReachesNamedThrough` | shape | `typeReachesNamedThrough(field.Type(), name, entered)` | DRIVER — the walk into every exported field. |
| `mls/validate_commit_test.go` `TestTheSectionTwelveTwoInputThisFileBuildsIsThisCommitsOwnFields` | name | `!written where written = expected[name]` | NARROWING/refusal — a field of the section 12.2 input nothing says the provenance of is reported. |
| `mls/validate_commit_test.go` `TestValidateCommitRefusesAListThatIsNotTheCommitsOwnProposalVector` | name | `!driven[name]` | NARROWING/refusal — a field of ProposalOrRef no row makes the list and the vector disagree over is reported. |
| `mls/validate_commit_test.go` `TestValidateCommitRefusesAListThatIsNotTheCommitsOwnProposalVector` | name | `!onTheType[name]` | NARROWING/refusal — the other direction: a row naming a field the type does not carry is reported. |
| `mls/vectors_runner_test.go` `theJsonKeyOf` | attribute | `key == "" where key = strings.Cut(found.Tag.Get("json"), ",")` | NARROWING/refusal — a field with no json key is fatal, because a lookup under its go name would answer "absent" about a corpus that publishes the column perfectly well. |
| `mls/welcome_test.go` `TestTheGroupInfoSignatureCoversEveryFieldOfItsToBeSigned` | name | `name == "Signature"` | NARROWING/complement — CLOSED THIS ROUND: the removed member is now printed beside the class it is removed from. It is one member, named by a literal of this test, and the sentence that puts it out is that a signature does not cover itself. |
| `mls/welcome_test.go` `changeGroupInfoField` | shape | `value.Type().Elem().Kind() == reflect.Uint8` | DRIVER — chooses how to move a value of this kind; the switch's default is fatal, so a kind with no move fails rather than being reported covered. |
| `mls/welcome_test.go` `groupInfoTbsFieldPaths` | shape | `field.Type.Kind() == reflect.Struct` | DRIVER — descends into a nested structure; every field yields a path. |
| `mls/welcome_test.go` `providerGroupInfoPerturbations` | name | `!written where written = edits[name]` | NARROWING/refusal — fatal: a field of GroupInfo no perturbation moves would answer identically under every move this gate makes. |
| `mls/welcome_test.go` `providerWelcomeJoinerPerturbations` | name | `moves == excused where excused = theWelcomeJoinerFieldTheSealDoesNotRead[name] where moves = edits[name]` | NARROWING/refusal — fatal: a field of WelcomeJoiner that is neither moved nor written down as unread by the seal, or that is both, is reported. |
<!-- gates-index:end -->

---

## The nine rows that are here by coincidence, measured rather than excused

Nine rows of the index above — eight functions: `typeReachesByteStorageThrough`,
`epochCachesHeldBy`, `externalDoorsOntoAVerifiedGroupContext`, `externalShadowHasThisTypesShape`,
`comparesOctets`, `theGroupContextParameters`, `reachesByteSliceType` and
`typeReachesNamedThrough` — narrow a **`go/types`** member set, not a reflect one. **They are in
this index by accident, and the accident is that `go/types` spells two of its member doors
`Field` and `Method`, exactly as reflect spells two of its own.** The derivation reads a selector
spelling; it does not know which library the value came from.

That is worth saying out loud rather than letting the rows read as coverage, and it is
**measured** by `TestTheGoTypesRowsOfTheIndexAreACoincidenceOfSPELLINGAndTheCoincidenceIsMeasured`
rather than asserted here. On Go 1.26 that gate reports:

- the member-descriptor sentence — an exported STRUCT carrying a name beside a type — admits
  **0 of `go/types`' 38 exported structs**, because `go/types` hands a member back as an OBJECT
  whose name and type are METHODS. So no door of `go/types` is reached by rule;
- asking the same sentence of a method set instead finds **9 exported named types** of `go/types`
  that name a member and answer its type, reached through **23 exported doors**;
- of those 23, exactly **2 — `Field` and `Method` — are spelled the way a reflect door is.** That
  is the whole of the coincidence and the whole reason those nine rows exist;
- and the **complement is 21 spellings this index does not reach at all**: `At`, `ExplicitMethod`,
  `Insert`, `Lookup`, `LookupFieldOrMethod`, `LookupMethod`, `LookupParent`, `MissingMethod`,
  `NewConst`, `NewField`, `NewFunc`, `NewLabel`, `NewParam`, `NewPkgName`, `NewTypeName`,
  `NewVar`, `Obj`, `ObjectOf`, `Origin`, `PkgNameOf`, `Recv`. These trees use several of them —
  `At`, `Params`, `Results` and `NumFields` all appear in `_test.go` source here — so a narrowing
  spelled over a `*types.Tuple` is outside this index today.

**The decision is to state the boundary and measure it, not to widen it quietly.** Making the
overlap a rule means a second derivation with its own scope, its own over-reports and its own
complement to print — a round's work rather than a line — and a half-built one would put every
`go/types` member set inside this file's universal claim while reaching a tenth of them, which is
the defect this file is about. The gate fails if the coincidence ever becomes total or ever
becomes a rule, so this paragraph cannot quietly stop being true.

---

## One hazard the scope half has here, found while closing the fifth

Several gates in `messagegroup` walk this package's **test** source by bare function name
(`keysource_test.go`'s `TestNothingOnTheReproductionsSideOfTheComparisonComesFromTheModule` is the
one that fires). A name-keyed walk cannot see a receiver, so **a test-only method sharing a name
with a production method makes a selector that used to dangle resolve into somebody else's
closure.** Measured on 2026-09-09: adding a control type with methods named `Epoch` and `Destroyed`
to `epoch_test.go` turned that gate red, reporting its exclusion as "swallowing scope" over an edge
that was a name collision and nothing else. The controls were renamed `ProbeEpoch` and
`ProbeDestroyed`, which cost them nothing -- both classes they prove are decided by results and by
behaviour, neither reads a name -- and the gate returned to its previous reading exactly.

It is recorded here rather than fixed because the fix is in the gate's resolution, not in the
control, and a gate that resolves a method selector as a package-level function is a scope defect of
its own: it will also mis-resolve two production methods of different types that share a name.

---

## What holds this file to the tree

Nine tests in `mls/gates_index_test.go`, and they are the whole reason this file is worth reading:

- `TestTheGatesTableIsTheDerivedClassAndNotAListOfIt` — the index above equals the derived class in
  both directions, every row carries a verdict from the vocabulary, and an `OPEN` row is red.
- `TestTheQueryGatesPublishesIsMeasuredAgainstTheDerivationRatherThanTrusted` — the greps published
  above are compiled and run, their recall is compared against the fraction this file states, and
  the sites they do not reach are named in the log.
- `TestAQueryKeyedToOneSpellingOfTheReceiverStillMissesTheSitesItMissed` — the query this file
  published at `81b97ca`, kept verbatim, is run against the same derivation. Measured today it
  reaches **48 of the 170** narrowing occurrences and misses 122, in eighty functions. The claim
  that it was insufficient is a measurement anybody can re-run, and reverting the published query
  to a receiver-keyed one goes red rather than green.

  **One thing that measurement says out loud, because it is the whole argument.** Of the three sites
  the reviewer named, two — `TestNoVectorRunnerCanSkip` and `trRecordLayerCodecMethods` — are still
  missed and are asserted to be. The third, `mlsEncodingEmitters`, is now REACHED, and nothing about
  what that gate decides changed: closing it rewrote its condition from
  `if name := writer.Method(i).Name; strings.HasPrefix(name, "Write")` to
  `if strings.HasPrefix(method.Name, "Write")`, and `HasPrefix(method.Name` is exactly the literal
  the old query greps for. A query whose recall moves by a site because somebody renamed a local
  variable is measuring spelling, not the property. That is why it was replaced by a derivation, and
  why the test asserts the two rather than quietly keeping a third assertion the tree no longer
  supports.
- `TestEveryRowClaimingAPrintedComplementHasOne` — the two verdicts that make a claim about RUN
  TIME are held to the source that has to carry it. `NARROWING/complement` is separated from `OPEN`
  by one thing, whether the removed members are named at run time, and until this test existed that
  separation was prose: deleting the `t.Logf` out of `framedContentArmFields` left every other gate
  here green. It is a PROXY and says so — it decides that the narrowing's own function calls a
  reporter, not that what it prints IS the removed set — and it fails closed toward the row: a
  complement printed by the caller reads as absent, so move the print or change the verdict. It
  found one wrong row on the commit that added it: `epochIsTheDestroyedFlag`'s signature clause was
  indexed as a refusal and reports nothing at all.

- `TestTheGatesDerivationSeesANarrowingHoweverItsReceiverIsSpelled` — the derivation is driven over
  a control holding one narrowing of each shape it claims to read, with the receiver spelled four
  different ways, and six shapes it must NOT read. A derivation that reported nothing would agree
  with a document that indexed nothing.

  **It is also what holds this file's stated BOUNDARY to something.** Thirteen shapes it must see:
  a name narrowing over a method set, an arity narrowing in another function, a shape narrowing
  through a bound signature, a name narrowing over a FIELD set, a shape narrowing through a bound
  field descriptor, a predicate bound to a name and used as a condition, a predicate spelled as a
  function literal's result, **the five the eighth instance added** — a narrowing by a descriptor's
  `Tag`, by its `PkgPath`, by its `Anonymous`, by the length of its `Index`, and by a key CUT out
  of a tag and compared two lines further down, which is how the three live sites in `mls` are
  spelled — and **one the eighth round's own self-check turned up**: a predicate answering a bool
  BESIDE another result. "A function answering exactly one `bool`" was an ARITY narrowing over the
  predicate class, which is the shape Q1 above calls always suspect, sitting inside the file that
  publishes the warning; its complement was unprinted and, probed, EMPTY, so nothing would have
  gone red on the day the first `(bool, error)` predicate landed. Fifteen it must not: a struct
  field spelled `name`, a directory entry's name, an
  accumulator of member names consumed by `len()`, and **the twelve under-reaches this file
  states** — a signature read off a parameter declared `reflect.Value`, a member held in a struct
  field declared elsewhere, and a predicate answering a defined type whose underlying type is
  `bool` — **and the NINE the ninth instance added**: six container shapes (a member set reached
  through an ITERATOR door and the field twin of it, a member declared `*reflect.Method`, one in a
  `map[K]reflect.Method`, one in a defined slice type, one taken variadically) and three statement
  forms (a type assertion, a comma-ok type assertion, a type switch). Those nine are the ninth
  instance **driven rather than closed**, and they are written to go RED on the day a round widens
  the class: the correct response to that failure is to move them to the seen half, never to
  delete them. A control that starts failing as a class widens is the control saying so.

  The last twelve are the difference between a boundary and a paragraph, and the boundary has now
  been wrong twice while reading as complete. In the sixth round a predicate bound to a name was on
  neither of the two lines that claimed to say what could not be seen. In the seventh the paragraph
  said "exactly three, and all three want a TYPE" while four narrowings over a descriptor's own
  fields wanted no type at all. **Both were counts of the unseen.** What is driven is a list of
  what is asserted NOT found; it is not a proof that nothing else is missing, and this file no
  longer says otherwise.

- `TestEveryComplementThisDerivationComputesIsPrintedByIt` — this file's own rule, turned on this
  file. The derivation narrows three times before it reads a line of these trees: the exported
  structs the descriptor sentence removed, the exported spellings the door sentence removed, and
  the descriptor fields that are neither the name nor the type. It prints all three, and until
  this test existed **nothing held it there** — deleting any one of those `t.Logf` calls left
  every other gate here green, and that mutation was reported as a survivor in two rounds running.
  The class is derived from `gatesDoorSet`'s own declaration, so a complement added to that struct
  in a later round must be printed on the commit that adds it. Like the row gate it is a PROXY —
  it decides that the field is named in a reporter's arguments, not that what is printed is the
  set — and what it does not read, the door map itself, is printed beside the complements it does.

  **And a fourth narrowing, made while it reads these trees, is now held by the same test.**
  `gatesRecordDecided` decides which RESULT POSITIONS of a function are the decision, and until
  this round it took no reporter at all: the round that widened it off *"a function answering
  exactly one `bool`"* closed the arity and left the print, which is one half of one rule closed
  and the other half not. It now hands every non-`bool` result position to the caller, which names
  the ones that actually READ a member — **43 of them in these trees today** — so the complement is
  a set somebody can scan for the shape that reaches past a spelling, a DEFINED type whose
  underlying type is `bool`. The class this test holds is derived, not named: an ACCUMULATOR is a
  parameter the derivation only ever writes into, so the only place left for it to be read is a
  reporter in the caller, and a second one added in a later round is held on the commit that adds
  it. Two proxies, said out loud like the rest: it decides that every function-typed parameter of
  the recorder is CALLED and that the accumulator is NAMED in a reporter — not that what is called
  reports the removals, nor that what is printed IS them.

- `TestTheContainerClauseOnTheDoorSentenceIsANarrowingAndItsSizeIsMeasured` — the ninth instance
  made visible and deliberately not closed. The door sentence's container clause is asked with and
  without, every spelling it removes is named at run time, and the `gates-doors` pair above is held
  to the measurement. It goes red on the day the toolchain adds a container this reading does not
  know, which is the one thing a literal that cannot be moved can still be made to do.
- `TestTheExportedFieldNarrowingRefusesRatherThanContinuing` — the refusal that replaced a
  `continue` in `gatesDescriptorFields`, DRIVEN. Nothing in these trees can drive it, because both
  member descriptors are exported through and through and the complement it guards is empty, so it
  is driven here with a made-up complement instead. A fail-closed path nothing exercises is a
  fail-closed path nobody has checked, and this file has already shipped one of those.
- `TestTheGoTypesRowsOfTheIndexAreACoincidenceOfSPELLINGAndTheCoincidenceIsMeasured` — the nine
  rows of the index that sit over a `go/types` member set are held to being a coincidence: the
  member-descriptor sentence admits nothing in `go/types`, the two spellings the two libraries
  share are named, and the twenty-one `go/types` member doors this index does NOT reach are
  printed. It goes red if the overlap ever becomes total or ever becomes a rule.

## Two rules for anything added here

- **Give the complement, not the worry.** An entry that says "this class looks narrow" is worth
  nothing. An entry that says "this narrowing removes these N members today, and here is the
  property that says they are out" can be checked in one command.
- **Close entries by deleting them,** and name in the same commit the gate where the narrowing now
  fails closed. A row that outlives its narrowing is the file describing a tree that no longer
  exists — and that direction is now decided by a test rather than by the reader's diligence.

---

## Two more instances, in `messagegroup`, and why they are not round ten

Found by a reviewer on the j1 join, closed on 2026-09-10. They are recorded here because they are
this file's class exactly — **a gate whose class is a NAME rather than the property** — and because
the way they were closed is the answer this file already gives rather than another turn of the arms
race. **Neither was closed by making a list longer.** Both were closed by the two questions.

### Instance A — an erase obligation that bound a method NAME

`engineJoinMaterialEraseSites` in `messagegroup/engine_test.go` derived the join's erase obligation
off the syntax tree: in the function that calls `mls.JoinFromWelcome`, count calls whose selector is
spelled `Zeroize`, require exactly one to be deferred and none to be a plain statement. The class
is derived. The literal is `Zeroize`, and **being wrong about it was invisible**: a deferred call
named `Zeroize` that erases nothing reads identically.

**Measured.** `defer keys.Zeroize()` rewritten as `defer mutantEraser{}.Zeroize()`, with
`type mutantEraser struct{}` and a no-op `Zeroize()` beside it in `engine.go`. The source read saw
exactly one deferred erase and no plain one. The whole of `./mls/... ./message/... ./messagegroup/...`
stayed **GREEN at 7,692 passing, 0 failing, 0 skipped** — a join that erased nothing, with both HPKE
private halves and a copy of `device_sig` left in the heap, passing the entire shipped suite.

**What closed it is R7 and not a wider list.** This project already had the rule, in
`recordingAliasStore`'s own header: *an erase is observable only through an ALIAS of the array
erased, and wherever the far side copies, the property must build the alias or it is measuring a
photograph.* The join's material is four copies made in a production local, so no store route
reaches it — but `mls.JoinFromWelcome` opens the welcome secret through
`OpenWithLabel(crypto, keys.InitPrivate, …)`, which reaches `crypto.HpkeOpen(priv, …)` with the
slice passed straight through. `aliasingCryptoProvider` retains that header.
`TestTheJoinErasesTheArrayAndNotOnlyAMethodNamedZeroize` is red under the mutant on both exits, with
an at-call copy as the control so an empty or already-zero array cannot satisfy it. The `Zeroize`
literal is unchanged and is now at a level **where being wrong is visible.**

**The sweep, because the finding named one gate and the class is bigger.** Every erase obligation in
`messagegroup` was read for whether a runtime observation stands behind its source read. The query:
`grep -rn "would pass against\|did nothing\|so this reading would" --include=*_test.go` for the
runtime controls, against `grep -rn "ErasesInSource\|EraseSites\|erase helpers"` for the source
reads. Eleven obligations carry a non-zero control that fails over an erase that did nothing —
`zeroize_test.go`, `keyschedule_test.go`, `ratchet_test.go` twice, `epoch_test.go` twice,
`session_test.go`. `engineNewKeyPackageErasesInSource` is a source read and is covered, because
`TestNewKeyPackageErasesEveryPrivateHalfItMintedBeforeItReturns` holds the same property through
`recordingAliasStore`'s retained headers. `zeroizeEraseClass`'s `Zeroize` and `zeroize` literals are
positive controls that **fatal** when absent, which is this file's keep verdict.

**The join was the only one of the eleven with no runtime observation at all**, and the reason is
worth keeping: it is the only erase whose subject is a local that no interface this package controls
ever sees. Wherever the far side copies, somebody has to go and build the alias.

### Instance B — an impossibility class narrowed to two identifiers

`TestNoProductionSentenceOfThisPackageSaysAJoinIsImpossible` in `messagegroup/enginejoin_test.go`
derived "a production sentence asserting a join is impossible" as: a comment or string literal that
NAMES `TakeKeyPackage` or `ErrEngineJoinUnavailable` **and** carries one of six phrases. The subject
was two identifiers.

**Measured.** Two comment lines above `JoinFromWelcome`'s header — *"A SECOND DEVICE CANNOT JOIN A
GROUP THIS ENGINE FOUNDS. The adapter publishes no joiner material a founder could address, so a
welcome join is not reachable from this package."* The gate reported *"production sentences asserting
a join is impossible: 0"* and the package stayed green. The sentence even carried a phrase from the
list; it simply named neither identifier.

**What closed it.** The subject is now derived and has no literal: the seed is the production
declaration whose body calls `mls.JoinFromWelcome` — found, not named — and the vocabulary is the
words of the path's declaration names, twice narrowed, with both complements printed. The two
narrowings are *shared with the rest of the package* (removes `key`, `package`, `engine`, `handle`,
`err`, `no`, `for`, `from`, `mls`, `connect`, `zeroize`) and **RECURRENCE**: a word that names what
the join IS appears in more than one of the path's names, a word incidental to one helper's spelling
appears in exactly one. Without recurrence the vocabulary is `{join, welcome, with, taken, shape}`
and `with` alone carries the class from 54 sentences to 266. With it, `{join, welcome}`.

**And the predicate literal was answered the way this file says to answer one.** The phrase list
cannot be derived — *"asserts the join cannot happen"* is a judgement, and a negation adjacent to a
join word is not it: *"Rejected: taking only after a successful join, which this interface cannot
express"* is a true sentence of exactly that shape. So the complement is printed sentence by
sentence on every run, and **the set of DECLARATIONS allowed to speak about the join in the negative
is pinned**, seven of them, each with the reason its sentences are true. A new declaration making a
new claim is red on the commit that adds it **whatever words it chooses** — measured with
`engineJoinerHorizon`, whose *"a welcome never yields a member here"* matches no phrase in the list
and is caught by the pin.

That is this file's own closing rule applied rather than quoted: **a derived class whose literal is
invisible and silent is worth less than an enumerated one that refuses and prints.** The class is
derived; the disposition is enumerated; both complements are printed.

### One clause of the fix was itself wrong, and the measurement is why it is not still there

The first version failed closed on an empty SUBJECT complement, on this project's standing rule that
an empty complement is the dangerous reading. **It went red over correct source** the moment
`doc.go` was corrected: the phrase list is deliberately join-specific, so "asserts an impossibility
about something else" is expected to be small or empty, and the clause was measuring the phrase
list's breadth while calling it a class boundary. It was replaced by the check it was reaching for —
that the vocabulary does not admit EVERY sentence, which is the real form of "reported clean having
read nothing" for a subject narrowing. **An empty complement is a question, not a verdict**, and
which one it is depends on whether the narrowing was supposed to remove anything.

### And a third, found while closing the second

`TestThisFileSaysWhatItDoesNotEstablish` asserted four sentences were present by reading its own file
for a needle. A constant was rewritten back to its old, one-sided wording and the gate stayed green:
the needle still matched the **paragraph above the constant, which describes it**. A gate satisfied
by prose about a value is not holding the value. Each needle is now asked twice, of the file and of
the constant — which is not the self-comparison that file warns about, because the needle is
assembled in the gate independently of the constant.

---

## A fourth, and it is a gate ADDED rather than a defect found

Closed on 2026-09-11, over the clone coupling `connect/messagegroup`'s join body rests on.

**The coupling.** `joinWithTakenKeyPackage` assembles `mls.JoinKeyMaterial` over four copies and
defers `(*JoinKeyMaterial).Zeroize` over the result, because that type owns every array it carries.
Its own header calls the fourth copy *"a fourth instance of a discipline this path already spells
three times"* — and the three it names are fill sites in **this** package: `mls/group.go`'s
`signer`, `mls/treekem.go`'s `EncryptionPriv`, `mls/key_package.go`'s `signPriv`. Two of the three
are spelled `cloneBytes(x)` and the third `append(T(nil), x...)`, so a gate keyed to either spelling
is blind to the other — this file's own class, one altitude over.

### The sentence that stood here was false, and the way it got here is the transferable part

This section, and the commit message of `75eec1e`, said **"Nothing held them."** in bold. **It is
false for all three sites**, and it was written from a measurement that had been taken through a
`-run` filter.

| mutation | pre-existing top-level tests that go red, UNFILTERED | of those, in `mls` | in `messagegroup` |
|---|---|---|---|
| `group.go:3422` `signer` ← `keys.SignPrivate`, aliased | **9** | 9 | **0** |
| `treekem.go:120` `EncryptionPriv` ← `encryptionPriv`, aliased | **40** | 40 | **0** |
| `key_package.go:410` `signPriv` ← `signer`, aliased | **19** | 6 | **13** |

One of them, `TestNoConstructionOfSealedStorageRetainsItsCallersArrays` in `mls/caller_arrays_test.go`,
is a **derived runtime aliasing gate added by `aa4838e`, which predates this work entirely** — it
prints the coupling in the coupling's own words and it reddens under two of the three.

**The cause, which is the reason this belongs in this file rather than in an erratum.** `go test`
`-run` **splits its pattern on `/` and applies each part at every nesting level**, so a regex written
to select a few top-level cases also filters every subtest under every case it admits. Over these
three trees that discards roughly two thirds of all entries. The previous pass drove its mutations
under such a filter, saw nothing go red outside the tests it had just written, and published
*"nothing held them"* — a **confident negative derived from a narrowed reading**, which is the exact
shape of the nine defects above it, with the class narrowed by a command-line flag instead of by a
predicate.

**So the rule, and it is not about `-run`:**

> A targeted `-run` regex is for ITERATING. Any claim about what a mutation does or does not catch
> must come from an **unfiltered** run, and the claim must say which it was. A passing suite is
> consistent with every universal and every negative in a commit message being false, so a negative
> is a measurement that has to be taken and reported, never an observation that nothing happened.

**What is actually true is narrower than that correction's own first draft, which is why the fourth
column is there.** The first rewrite of this section said *"nothing caught them from the messagegroup
side"* — and **that is false for the third site**: thirteen pre-existing `messagegroup` cases redden
under the `signPriv` mutation, because this engine defers `keyPackage.Zeroize()` over a key package it
minted with its own signer, so destroying that array breaks the device on its first publication.
The true statement is per-site and it is the table:

> Two of the three join-path sites — and the **fourth** member found later, `group.go:668` — were held
> **only from inside `mls`**, by gates over `mls`'s own arrays. **Zero** `messagegroup` cases reddened
> under any of those three. The third site was already held from both sides.

So `messagegroup/joincoupling_test.go` adds coverage over `group.go:3422`, `treekem.go:120` and
`group.go:668` and adds none over `key_package.go:410` — it re-states that one from a side that
already had thirteen. That is a smaller claim than the commit made, and it is the one the measurement
supports. The derived gate in this package is worth its weight for the second reason its own header
gives rather than the first: it fails **at the statement** instead of three packages away, and it sees
a fill site no join happens to exercise.

**And the fourth site is the argument for keeping both halves.** `group.go:668` — `NewGroup`'s clone
of the founder's signing key — reddens **9 pre-existing `mls` cases and 0 `messagegroup` cases**, and
of the four clauses of the pin that existed before this round, **none**. It is caught here by
`TestTheFounderSurvivesFoundingAndClosingItsOwnGroup` and by this package's own derived gate.

**What was built, in two halves, because neither half is enough alone:**

- `messagegroup/joincoupling_test.go` — a **behavioural pin**. It joins, then asks whether the
  device's own signing array survived, whether the device can still sign through a door driven
  after the join, whether the joined handle can still sign **with a peer as the judge**, and
  whether that handle can still open a path addressed to its own leaf. Each of the four clauses
  is the only one that catches its own site; all three alias mutations and the removal of the
  engine's own defensive copy were driven red through it.
- `mls/erased_field_alias_test.go` — a **derived gate**. The class is derived in three steps that
  each fatal on empty: erase helpers by **body shape** (a `for i := range p { p[i] = 0 }` over a
  parameter — the name `zeroizeSecret` appears nowhere in it), erased fields by where those
  helpers are called on a receiver's field, fill sites by field name over every composite literal
  and assignment. The decision at each site is an **alias** question, never a spelling one: the
  RHS is resolved to the origin of its backing array through parens, reslices, derefs, address-of,
  type conversions including `[]byte(nil)`, `append`'s destination, locals in both the one-to-one
  and the `a, err := f()` forms, and one hop into any function this package declares.

**The measurement that decided the gate's shape, and it is the part worth reading.** Twenty fill
sites exist. Fifteen are not caller-rooted. **Five are, and none of the five is a defect** — four
are ownership transfers this package makes on purpose (`(*SecretTree).newRatchet`'s header states
its own in words) and the fifth is erased by its caller two frames up. *Nothing in the source tells
an ownership transfer from a borrow*; it takes escape analysis or a stated contract. Widening the
decision rule until those five passed would have left a rule that refuses nothing.

**So the class is derived and the DISPOSITION is enumerated** — `eraseOwnershipHandovers`, keyed by
`file:function.field` and never by line, each entry carrying the evidence. Caller-rooted and
undisposed is red; a disposition matching no site is red; an empty reason is red. That is this
file's own remedy rather than a retreat from it: *"a derived class whose literal is invisible and
silent is worth less than an enumerated one that refuses and prints."*

**Held to the two questions:**

| the literal | visible? | fails closed? | prints its complement? |
|---|---|---|---|
| the erase **body shape** in `eraseHelpersIn` | yes — a wrong shape empties the class | yes — `t.Fatal`, and the control corpus spells its erase `wipe` | yes — the helper set is printed every run |
| the **peel forms** in `originOf` | yes — a form it does not know lands in the undecided list | yes — undecided is an error, never an admit | yes — 0 undecided today, printed as a count beside the four other verdicts |
| the **binding-position forms** in `eraseFillSitesIn` | yes — a sub-form with no reading is refused by name and line | yes — a refusal is `t.Error`; a positional literal whose type this gate cannot name is refused | yes — **complement 4**, 3,194 binding positions decided NOT to be members, counted under the reading that decided each, and empty is an error |
| the **five dispositions** | yes — each is printed **with its reason and the array it excuses**, on every passing run | yes in both directions — undisposed is red, stale is red, empty-reason is red, and one reason answering two statements is red | yes — and the 15 admitted sites are printed with where each array came from |
| the **opaque callees** the admissions rest on | partly — they are named every run but not opened | no — an opaque call is admitted | yes — `crypto.DeriveSecret`, `r.ReadOpaque`, `self.crypto.DeriveTreeSecret`, named every run |
| the **readings themselves**, in `eraseReadings` | yes — an arm with no tag is named by file and line, and a name computed rather than written is refused | yes — a declared reading nothing drives is an error, and so is an unreached row that is taken, a stale row, and two arms sharing one name | yes — **complement 5**, the readings the corpora drive that this package's own source does not reach, and empty is an error |

The last row is this gate's open edge, written down rather than argued away: those callees are
admitted on the strength of their copying and this gate does not check that they do. It is named on
every run so a **fourth** name appearing there is visible in a passing log.

### Four holes an adversarial pass found afterwards, by planting statements to falsify the claims above

The three rows the first version of this table did not have are the three it was wrong about. Each
hole below was **planted and measured green** before it was closed, then planted again and measured
red; a passing suite is consistent with every one of them, which is why they were looked for.

**1. The fill-site enumerator was blind to two of the five binding positions, SILENTLY.** It read
`*ast.KeyValueExpr` elements of a composite literal and assignments whose two sides have equal
length. So `&PathSecret{p}` — a **positional** literal — and `x.Field, err = f()` produced **no site,
no complement entry and no undecided count**: in this gate's own log, a tree containing either was
byte-for-byte indistinguishable from a tree containing neither. Twenty-two statements of the second
form are in this package's production source today, because it is how every decode binds the octets
it just read.

*The measurement that isolates it, and the caveat that goes with it, because this section exists
because a negative was published without one.* Planting three aliases — positional, multi-value and
method-spelled — moved this gate from **20 fill sites to 23**: the method call appeared and was
admitted as opaque, and the other two produced **+0**. That `+0` is the defect, and it is the only
reading here that isolates it. What is NOT true is that the plants passed the suite: run unfiltered
at `75eec1e`, they reddened **three other pre-existing `mls` gates** —
`TestEveryConstructionInThisPackageLeavesItsInputAlone`,
`TestEveryTypeHoldingErasableKeyMaterialErasesAllOfIt` and `TestNoStubShapesRemainInSource` — none of
which was reading the aliasing. They fire on the shape of the added *declarations*, so they would
fire on a plant that had no defect in it at all. A probe that is not behaviour-neutral measures
itself as well as its target, and saying which is which is the whole of the correction above.

The remedy is `gatesDeriveDoors`'s and the fix was made the same way: **the boundary is stated and
the walk fails closed.** Go rebinds a struct field in exactly four syntactic positions —
`*ast.CompositeLit` (keyed and positional, at any depth of elision), `*ast.AssignStmt` (one-to-one,
multi-value, operator), `*ast.RangeStmt` and `*ast.IncDecStmt` — every sub-form of each is decided,
a sub-form with no reading is **refused by name and line**, and what is *outside* the boundary is
written down instead of implied: a write **through** the field's existing array (`copy(x.Field, p)`)
and a write through a pointer taken at `&x.Field` are not rebindings, and this gate does not claim to
see them. Positional elements are named by reading the **declaration order of the struct**, which is
why this arm needs a type reading and the keyed arm never did.

And the narrowing now **prints what it removed** — complement 4, 3,194 positions by reading, empty is
an error. *A form nothing counts is a form nothing can miss.*

**2. The disposition list absorbed new fills.** The key was `file:function.field`, and each reason was
read and written about **one statement** — a particular array from a particular parameter. A key that
stops at the field name is satisfied by any statement in that function binding that field.
Demonstrated by planting `secrets.PathSecret = &PathSecret{PathSecret: joinerSecret}` one line under
`BuildWelcome`'s own fill: **green**, excused by a sentence about `joiner.PathSecret`. The key now
carries the rendered right-hand side — still never a line, because a line goes stale on the next edit
above it while the array does not — and one reason answering two statements is red.

**3. A method-call spelling of an alias was admitted.** `originOfCall` answered `opaque` for every
call whose `Fun` was not a bare identifier, so `h.Bytes()` — a method **declared in this package, on
a type this package declares, whose body this gate had already parsed** — was admitted as "a call
this gate did not open". The disclosed open edge does not cover that: it is about callees this gate
*cannot* open. A method on a concrete type of this package is now **opened**, with the receiver
mapped back onto the call's receiver expression, and a receiver whose declared type is an
**interface** stays opaque and stays named in complement 3 — `crypto.DeriveSecret` could be any
implementation of `CryptoProvider`, and reading the one that happens to live here would be a guess
wearing a derivation.

**4. A fourth member of the coupling class had no clause.** The class was re-derived from the
`messagegroup` side — every `mls` entry point reached from its fourteen non-test files, intersected
with retained destinations and the twelve erased field names — and it has **four** members, not
three. `CreateGroup` hands `self.signer` to `mls.NewGroup` with no defensive copy of its own.
**Measured before anything was changed**, because *"there is no copy at the call site"* and *"the
array is retained"* are different claims and only the second is a defect: `mls/group.go:668` fills the
founded group's `signer` with `SignaturePrivateKey(cloneBytes(signer))`, so **the founder path is
safe today, by the very discipline the pin exists to defend.** What it needed was therefore a
**clause and not a copy** — a defensive copy in `CreateGroup` would let the device survive a
`NewGroup` that had stopped cloning, which is the one thing the clause is for.
`TestTheFounderSurvivesFoundingAndClosingItsOwnGroup` founds a group, **closes it** — `(*Group).Close`
zeroizes `self.signer` at `mls/group.go:946` — and then reads the device's own array.

**Nothing held this either — and that sentence is a MEASUREMENT, which is not what stood here.** What
stood here was a reason: *every fixture in `messagegroup` that founds a group closes it in a
`t.Cleanup`, which runs after the last assertion of the case that registered it.* The conclusion is
right and was measured independently. **The universal is false as written, and not marginally.** The
query is `grep -rnE '^[[:space:]]*t\.Cleanup\(' --include=*_test.go` over `connect/messagegroup`, and
it answers **two** statements — `engine_test.go:1493`, which closes a group handle, and
`sessionfixture_test.go:507`, which closes a session — against **forty-two** `defer x.Close()`
statements across eight files. Almost every fixture that founds a group closes it with `defer`, and
`engine_test.go:744` closes one inline. A universal written by hand is a list wearing a quantifier;
this one was a list of two.

What was actually measured, and what the claim is now: run **unfiltered** over
`./mls/... ./message/... ./messagegroup/...`, the `mls/group.go:668` mutation reddens **nine**
pre-existing top-level `mls` cases and **zero** in `messagegroup`, and **all four clauses of the join
pin above are green over it** — only `TestTheFounderSurvivesFoundingAndClosingItsOwnGroup` goes red.
That is the whole of the evidence, and it neither needs the universal nor survives on it.

### Four more, and the check that found them is the transferable part

The round above closed four holes and **checked five of the eight clauses it added** by deleting each
and confirming something noticed. The three it did not check were the three sub-form decisions —
`*ast.RangeStmt`, `*ast.IncDecStmt`, and the operator-assignment reading. Deleting the first two left
an **unfiltered** run over all three trees reading exactly what it read before. That is the
test-that-cannot-fail defect applied to *gate clauses*, and it is the fourth finding on this line to
be about the artifact written to stop the previous one.

So the check was run over **every clause of `mls/erased_field_alias_test.go`** — seventy-one of them,
each deleted or neutralised in a disposable copy, each landing confirmed by a byte-level assert on
the re-read file, each measured by an unfiltered run over `./mls/... ./message/... ./messagegroup/...`.

**THIRTY-THREE of the seventy-one were driven by nothing.** Not wrong — *undriven*. **Three are the
ones the finding named**, and all three are measured at `3e287a4` rather than taken on trust:
deleting the `*ast.RangeStmt` and `*ast.IncDecStmt` arms together, and separately deleting the whole
of the operator-assignment reading, each leaves the unfiltered run over all three trees at **7,701
pass / 0 fail / 1 skip** — which is that copy's own baseline, entry for entry. **Thirty more** came
out of running the same check over the rest of the file once those three had drivers: **thirteen**
further readings of the form walk — four of its refusals and **nine of its thirteen non-member
counts** — and **seventeen** arms of the resolver: the
type-assertion peel, the selector arm, the refusal of a name no scope binds, the scope-qualified
cycle key, the call arm and both comma-ok arms of the result-position reading, the `[]byte(nil)`
conversion, the `make` arm, the builtin arm, the opaque answer itself, the result-qualified cycle
key, the named-result reading, the return walk's stop at a nested literal, the `var` declaration and
the range form of a local's assignments, and the expectation handed to an elided literal.

**THE CAUSE IS THE SHAPE OF THE ASSERTIONS AND NOT THE COVERAGE OF THE CORPUS, and that is the part
that transfers.** Two assertions were doing the work of twenty:

- Complement 4 was asserted **only to be non-empty**. Non-empty is satisfied by twelve readings when
  there are thirteen, so any single reading could stop being taken with nothing anywhere noticing. *A
  complement asserted only to be non-empty is a complement with one clause, however many rows it
  prints.*
- The control corpus asserted only that a copy **is not the caller's**. That is satisfied by a copy
  that degraded into an **opaque** admission — and opaque is admitted *on trust*, named in complement
  3, precisely because this gate did not read it. A gate has more than one admission, and an
  assertion that does not name which one is a weaker assertion than it looks.

Both are now exact. Every spelling is asserted **by the kind it must answer**; the complement is
asserted **reading by reading**; and both corpora are **type-checked with `go/types`**, because a
driver that could not compile is not evidence about a form real source can hold.

**FOUR READINGS ARE UNREACHED RATHER THAN DRIVEN, and are named instead of left looking driven.**
Three are kept and are unreachable for a stated reason, because what deleting them produces is a
panic or a silently undecided binding position rather than a wrong answer:

- the bounds guard on a positional element past the end of a struct's field list — more elements
  than fields is a compile error, and the field list this gate reads holds exactly one entry per
  declared field, embedded ones included;
- the refusal of an assignment whose two sides differ in length and whose right side is not one
  expression — Go's grammar has no such assignment;
- the `UNDECIDED` default of the result-position reading — only a call, a map index, a type assertion
  and a channel receive are multi-valued in Go, and each already has its own arm.

The fourth is **not claimed to be unreachable**: the cycle key in `originOfBody` is qualified by the
result position as well as the callee, and **no compilable driver for that qualification was found**.
Reaching it needs one resolution chain that asks one callee at two different result positions, and
`originOfBody` returns at the first parameter- or receiver-rooted answer, so the attempt did not get
there. *"I could not reach it" is not "it cannot be reached"*, and it is recorded as the first and
not the second.

And one reading was **removed** rather than kept: the two that handed a struct field's declared type
down to a nested literal, for an elision Go permits only *"within a composite literal of array,
slice, or map type"* — `C{{x}}` and `C{H: {x}}` are not Go, and `go/types` refuses both. The
`eraseShape.types` field they were the only reader of went with them.

**One reason was also false rather than merely undriven.** The refusal of an unkeyed element in a
literal whose other elements have keys said *"which Go does not permit"*. Go permits exactly that in
a **slice, an array and a map** literal and forbids it only for a struct, so the arm is reachable —
`[]Pair{0: {1, 2}, {3, 4}}` — and it is now driven by that spelling, with the reason corrected to
what it is actually refused for.

#### The other three findings

**A fill site inside a function literal was resolved in the wrong frame.** `ast.Inspect` over a
declaration's body walks into every `*ast.FuncLit` in it, and every reading took an `*ast.FuncDecl`.
So an alias handed to a **closure parameter** whose name also existed as a fresh local one frame out
resolved to the local and was **admitted**. A literal is now a scope of its own, chained to the one
enclosing it, and a name is bound by the innermost scope of that chain that declares it. The return
walk stops at a nested literal for the same reason: `return x` inside a closure is the closure's
return, and counting it both invented a return the function never makes and handed the caller-side
mapping a name that is a parameter of the literal.

**A method promoted from an embedded struct was not opened**, although it satisfied every word of the
condition the method arm states — declared in this package, on a type this package declares, with its
body already parsed. The only thing between it and the reading was a map lookup keyed to the outer
type's own name. It is opened now, breadth-first by embedding depth, refusing an ambiguity at one
depth the way Go does; an embedded interface is skipped and the call stays opaque, on the same line
the interface receiver already sat on. *The code was widened to the condition rather than the
condition narrowed to the code, because a condition that describes more than the code does is the
defect this whole line has been about.*

**And the fourth is above, in place**: the universal about `t.Cleanup` is gone and the measurement it
was standing in for is what the paragraph now says.

#### The four clone-site mutations, re-driven unfiltered, and the table still holds

| site | mutation | pre-existing `mls` red | pre-existing `messagegroup` red |
|---|---|---:|---:|
| `group.go:3422` | `signer` aliased | 9 | 0 |
| `treekem.go:120` | `EncryptionPriv` aliased | 40 | 0 |
| `key_package.go:410` | `signPriv` aliased | 6 | 13 |
| `group.go:668` | `signer` aliased | 9 | 0 |

**And one of them nearly went the way of the `-run` mistake.** `sed -n '668p'` over `mls/group.go`
answers a line that occurs **twice** — `NewGroup` at 668 and `LoadGroup` at 2883 — so a one-line
anchor would have mutated both sites and measured a different thing entirely. The byte-level landing
assert refused it (`LANDED=NO matches=2`) and the anchor was widened to two lines. *An assert that
says how many times the pattern matched is worth more than one that says it matched.*

#### A fifth, open and named rather than closed

The same check found a defect that is **not** one of the four and was **not** introduced by closing
them. `eraseAssignmentsTo`'s multi-value arm records the one expression on the right for **every**
target on the left, so the **result position is lost**: a local bound at result 1 of a two-result call
is decided by the origin of result 0.

```go
func twoOut(x []byte) ([]byte, []byte) { return copyOf(x), x }

func probe(x []byte) *Held {
    h := &Held{}
    first, second := twoOut(x)
    _ = first
    h.Secret = second          // the caller's array
    return h
}
```

Driven through this resolver over exactly that corpus, the site at `h.Secret = second` comes out **"a
fresh array"** — an alias admitted in silence. The **fill-site walk has this right**; it passes the
target index and resolves through `originOfResult`, so `first, h.Secret = twoOut(x)` is decided
correctly. It is only a local standing in front of the field that loses the position.

**It is latent rather than live, and that is measured.** The query is the reading itself, made to
refuse: with every local bound at a result position other than 0 forced to have no assignment at all
— so that any site resolving through one comes out `UNDECIDED` and is refused — the gate over
`connect/mls` answers the **same twenty fill sites**, 5 parameter / 12 fresh / 0 receiver / 3 opaque
/ **0 undecided**, and stays green. No fill site in this package resolves through such a local today.

It is open on purpose. The round that found it was closing four named findings, and *a round that
opens a fifth front is a round that stops finishing* — but an unnamed defect is worse than an
unclosed one, so it is written here, in `eraseAssignmentsTo`'s own header, and in the judgement that
closed this line.

## The recurrence, not another instance: a standing check that an undriven clause cannot pass

Eleven rounds went into the clone pin and the derived gate that defends it, and the last one ran a
**deletion sweep over all seventy-one clauses** of `mls/erased_field_alias_test.go` and found
**thirty-three driven by nothing**. It closed with the observation that makes this round necessary:

> *"The sweep is a measurement, not a standing gate. Nothing in the suite will tell the NEXT round
> that a newly added clause is undriven; that check has to be re-run by hand. This is the second
> round on this line where an undriven clause shipped."*

So this round closed the recurrence. `TestEveryReadingOfThisGateIsDrivenBySomething` is the check
that refuses it, and it is in the suite.

### The shape, and why it is this one rather than either of the two obvious ones

Two shapes were on the table. **Each alone has the same hole**, and it is the hole the last round
already diagnosed in complement 4: *a reading nothing takes and nothing names is absent from BOTH
sides of the comparison, so the comparison holds.*

- **Readings that register themselves, compared against what the corpora exercise.** If "registered"
  means "wrote its name in during this run", registered is a subset of exercised **by construction**
  and the comparison is vacuous no matter how many arms nothing drives.
- **The control corpus as the authority, every reading needing a named row.** That is the
  `expectedComplement` table the last round already built, and it is exactly what complement 4's
  thirteen readings have. A reading with **no row and no take** is still invisible to it.

**So it is both, with the DECLARED side read out of the gate's own SOURCE TEXT rather than out of a
run.** Every arm that answers an origin or decides a binding position carries a tag that is *part of
the expression it returns* — `self.readings.origin("the slice-expression peel", …)` — and:

| set | what it is | how it is read |
|---|---|---|
| **DECLARED** | every `eraseReadings` tag written anywhere in `erased_field_alias_test.go` | the file is **parsed**; a tag whose argument is not a string literal is refused |
| **EXERCISED** | every reading the two control corpora actually took | both corpora are driven through the whole derivation |
| **UNREACHED** | `eraseUnreachedReadings`, with the reason for each | a row ever taken is red; a row naming no declared arm is red |

and the assertion is **DECLARED minus UNREACHED == EXERCISED, member by member, in both
directions** — never that either side is non-empty. *An existence assertion is satisfied by twelve
readings when there are thirteen, which is the precise mechanism that let this gate ship
twelve-of-thirteen in silence.*

**Four more clauses hold the shape up.** One name may be declared at only one place, or two arms
share one arm's worth of evidence. Every **case clause** of a registered function must carry a tag,
which is what catches an arm written *without* one — deleting a tag is then a compile-visible edit
that this clause fails on, rather than a silent no-op. Every unreached row must begin **`UNREACHABLE:`
or `UNREACHED:`**, because *"no compilable Go reaches this"* and *"I did not find compilable Go that
reaches this"* are different claims and a register that let them share a spelling would quietly
promote the second into the first. And `eraseClausesOutsideTheRegister` may not be empty.

### What it found the first time it ran

Over the gate as it stood at `e331116`, with every arm tagged: **83 readings declared, 57 driven by
the control corpora.** Twenty-six were driven by nothing at all, and **eight of those were driven
only by this package's own production source** — the exact gap carried forward as the round's own
open carry. Seventeen new spellings closed **nineteen** of them, taking the exercised set from 57 to **76**;
the remaining **seven are unreached and are held there by name**:

| arm | claim | reason |
|---|---|---|
| a positional element past the end of the field list | `UNREACHABLE` | more elements than fields is a compile error; the field list holds one entry per declared field. Deleting it produces a **panic**, not a wrong answer |
| an assignment whose two sides differ in length and whose right side is not one expression | `UNREACHABLE` | Go's grammar has no such assignment |
| the `UNDECIDED` default of the result-position reading | `UNREACHABLE` | only a call, a map index, a type assertion and a channel receive are multi-valued in Go; each has its own arm and parens are peeled first |
| `append` with no arguments at all | `UNREACHABLE` | `append()` does not compile. **This register found it**; the seventy-one-clause sweep never named it |
| a callee answering RECEIVER state called as a plain function | `UNREACHABLE` | receiver state is answered only where a scope of the chain binds a receiver name, and a plain function's chain binds none |
| the same, on the forwarded half | `UNREACHABLE` | the same argument |
| the default of `originOf` | **`UNREACHED`** | every Go expression that evaluates to a slice is one of the fourteen `ast.Expr` kinds the switch names; what is left is `*ast.BadExpr`, the type-expression nodes, and `*ast.IndexListExpr`, which is a function value no peel descends into. **No compilable driver was found, and that is the weaker claim** |

### Proved the way this line proves everything else: add a clause and watch it go red

**Seven mutations**, each in a **disposable copy outside both checkouts**, each landing confirmed by a
byte-level assert naming how many times the anchor matched, each measured by an **unfiltered** run
over `./mls/... ./message/... ./messagegroup/...`. That copy has a baseline of its own —
**7,697 pass / 6 fail / 1 skip**, and the six are named rather than waved at: they read `.git`
(`TestTheCommittedSeedCorpusIsPinnedAsBinary`, `TestTheLineEndingPinIsReadTheWayGitResolvesIt`,
`TestThePackageSourceIsOneLineEndingThroughout`), the repository root package
(`TestTheScanRootsAreEveryCryptographicPackageConnectedToThisOne`) and the workflow files
(`TestSyntaxWorkflowRunsEveryFuzzTarget`, `TestSyntaxWorkflowGatesRatherThanReports`), none of
which the copy has. The one skip wants a sibling `sdk`. Every row below is entry for entry
against that baseline.

| mutation | the clause it breaks | run | the one test that went red |
|---|---|---|---|
| a new `case *ast.Ellipsis:` arm of `originOf`, tagged, **driven by nothing** | every declared reading is exercised | 7,696 / 7 / 1 | `TestEveryReadingOfThisGateIsDrivenBySomething` |
| the address-of peel split so one half is reached **only by `welcome.go:608`** and by no spelling | the tree's readings are a **subset** of the control's | 7,696 / 7 / 1 | `TestEveryReadingOfThisGateIsDrivenBySomething` |
| the slice-expression peel's **tag deleted**, behaviour unchanged | every case clause of a registered function carries a tag | 7,696 / 7 / 1 | `TestEveryReadingOfThisGateIsDrivenBySomething` |
| the dereference peel **renamed to the parenthesis peel's name** | one name, one arm | 7,696 / 7 / 1 | `TestEveryReadingOfThisGateIsDrivenBySomething` |
| an `UNREACHED` row written for the slice-expression peel, which the corpora do take | an unreached row is never taken | 7,696 / 7 / 1 | `TestEveryReadingOfThisGateIsDrivenBySomething` |
| the **fifth defect put back** — the multi-value arm drops the result position again | `alias24` answers the result it was bound at | 7,696 / 7 / 1 | `TestTheErasedFieldGateSeesAnAliasHoweverItIsSpelled` |
| `(SecretTree).newRatchet`'s disposition key left at the **pre-change spelling** | a disposition names a site, in **both** directions | 7,696 / 7 / 1 | `TestEveryFieldThisPackageErasesIsFilledFromAnArrayItOwns` |

The second row is the one the last round's concern names explicitly, and it fires **two** clauses at
once — *undriven by either corpus*, and *taken over production source and by neither control*:

> `the reading "the address-of peel onto a COMPOSITE LITERAL" is taken over this package's production
> source and by NEITHER control corpus. … Write the spelling for it`

And the last row is what answers *"any disposition key written before that change is stale"* as a
measurement rather than as a reading of five strings. Spelling one key the old way reddens the gate
**twice over** — the site becomes undisposed, and the key becomes stale:

> `secret_tree.go:370: (SecretTree).newRatchet fills secret with rootSecret, whose backing array is
> the caller's own parameter "rootSecret" …`
>
> `eraseOwnershipHandovers disposes of "secret_tree.go:newRatchet.secret = rootSecret" and no fill
> site of this package answers to it.`

### What this check cannot see, which is the part worth reading

Every previous round on this line found its successor in exactly this paragraph.

1. **A clause added INSIDE an arm that already carries a tag.** The register's unit is the arm.
   Another name in `case "len", "cap", …`, another `||` on a condition, another `if` nested in a
   tagged body, or a sub-expression of a value the arm computes — none of those is a new reading as
   far as this check is concerned. **R25 is exactly such a sub-expression**, and it is filed below
   rather than registered.
2. **The functions outside `eraseRegisteredReadingFunctions`**, which are listed there by name: the
   readings that answer a **type** or a **set** rather than an origin — `declaredTypeNameOf`,
   `eraseDeclaredTypeOfName`, `promotedMethod`, `eraseShapeOf`, `shapeOf`, `elementTypesOf`,
   `eraseCompositeShapes` — the class derivations `eraseHelpersIn`, `eraseFieldsIn`,
   `eraseSiblingFields`, and the traversal helper `eraseInspectScope`. An arm added to any of those
   can be driven by nothing and this check stays green. They are not ungated — each is a `t.Fatal`
   on empty in the gate itself — but that is a different and weaker guarantee.
3. **Whether a reading was taken on purpose.** The register counts that a reading **answered**, not
   that answering it decided anything. A peel reached incidentally on the way to another answer
   counts as driven. *"Driven" here is weaker than "a corpus row asserts what it answers"* — which is
   what the two tables in the corpora tests are for, and they remain the stronger half.
4. **A reading whose name is computed.** The declared set is read out of the text, so a tag whose
   argument is not a string literal is **refused** — which is a refusal, not a reading of it.
5. **An arm deleted outright.** Deleting an arm takes its tag with it, so both sides of the
   comparison shrink together and this check stays green. That direction is the **corpora's** job,
   not this one's — deleting the slice-expression peel leaves `alias3` and `alias4` answering
   something else, which is a failing row with a name on it. *This check catches an arm added
   without a driver; the corpus tables catch an arm removed from under one.* What neither catches is
   an arm removed **together with the only row that drove it**, in one edit.

### The carry the last round left open: complement 4 over the real tree

*Complement 4 is asserted only to be non-empty over the real tree; the exact reading-by-reading
assertion lives in the control corpus, and a reading exercised only by real source and never modelled
in the control is the gap.*

**Item 1 closes that gap, and it was measured open before it did.** The register asserts that the
readings the **tree** takes are a **subset** of the readings the **control** takes, so a reading only
real source reaches is red at its own name. When the register first ran, **eight** readings were in
exactly that state — the forwarded-opaque answer, the literal arm, the unnameable receiver, the
multi-value local at its own result position, the local cycle key, the address-of peel, the index
peel and the selector peel. All eight now have spellings. **What it does not close** is complement
4's *counts* over the real tree: the register compares which readings were taken, never how many
times, so a reading that stops being taken by a **particular statement** while another statement
still takes it is invisible. That is narrower than the gap that was open and it is the honest
remainder.

### One thing the register says that nothing said before

It prints the twenty fill sites under the **member reading** that admitted each, and they split
**13 keyed / 7 one-to-one**. No fill site of an erased field in this package is bound by a
**positional** literal or by a **multi-value** assignment today — both of those member readings are
driven only by the corpora. That is not a contradiction of the finding that put the multi-value form
in: complement 4 counts **22** multi-value assignments in this package's source, and every one of
them binds a field **no erase reaches**. The form is here; it has simply never yet landed on a member
of this class. *Which is exactly the state in which a reading rots, and is why it is printed.*

### The fifth defect: closed, not carried

`eraseAssignmentsTo`'s multi-value arm recorded the one right-hand expression for **every** target on
the left, so the result position was lost: a local bound at result 1 of a two-result call was decided
by the origin of result 0 — an array it never receives.

```go
func twoOut(x []byte) ([]byte, []byte) { return copyOf(x), x }

func probe(x []byte) *Held {
    h := &Held{}
    first, second := twoOut(x)
    _ = first
    h.Secret = second          // the caller's array, answered "a fresh array"
    return h
}
```

It was **latent rather than live**, and that stands as measured: with every local bound at a result
position other than 0 forced to refuse, the gate answered the same twenty fill sites, 5 parameter /
12 fresh / 0 receiver / 3 opaque / 0 undecided, green. **The repair was cheap for the reason the
round that found it gave** — the fill-site walk already passed the target index through
`originOfResult`, so only the position had to be carried. `eraseAssignmentsTo` now answers
value-and-position pairs and `originOfIdent` resolves through `originOfResult`;
`eraseReturnExpressionsAt` carries the same pair because a **named result** assigned by
`out, err = f()` had the identical hole and nothing had named that one.

It is driven by `alias24`/`copy24`, which differ only in **which** result of one callee holds the
caller's array. Put the defect back and the run is **7,696 / 7 / 1** with
`TestTheErasedFieldGateSeesAnAliasHoweverItIsSpelled` red at
*"alias24 must answer `a parameter of the enclosing function` and the gate answers `a fresh array`"*.
The twenty fill sites and their five verdicts are unchanged by the repair, which is what "latent"
predicted.

### R25, filed in the terms it was named in

**R25 — the result qualification of `originOfBody`'s cycle key.** The guard is keyed
`call:NAME:RESULT`. The **arm** is driven, by `copy27`, a recursive callee. The **`:%d` half** is a
sub-expression of the key and has no arm of its own, so the register cannot see it — blind spot 1
above. It is **neither driven nor shown to be beyond compilable Go**: reaching it needs one
resolution chain that asks **one callee at two different result positions**, and `originOfBody`
returns at the first parameter- or receiver-rooted answer, so the attempt did not get there.

> *"I could not reach it" is not "it cannot be reached"*, and it is recorded as the first.

It is in `eraseClausesOutsideTheRegister`, printed on every passing run, and that map may not be
empty — because an empty one would claim the register sees every clause of this gate.

### The four clone-site mutations, re-driven unfiltered again, and the table still holds

Driven in the same disposable copy, against its own **7,697 / 6 / 1** baseline, with the gate carrying
the register and the fifth-defect repair. *Pre-existing* means everything except this line's own four
artifacts — the three gate cases, the new register case, the join pin and the founder pin.

| site | mutation | run | pre-existing `mls` red | pre-existing `messagegroup` red |
|---|---|---|---:|---:|
| `group.go:3422` | `signer` aliased | 7,686 / 17 / 1 | **9** | **0** |
| `treekem.go:120` | `EncryptionPriv` aliased | 7,652 / 51 / 1 | **40** | **0** |
| `key_package.go:410` | `signPriv` aliased | 7,675 / 28 / 1 | **6** | **13** |
| `group.go:668` | `signer` aliased | 7,686 / 17 / 1 | **9** | **0** |

Every one of the four also reddens `TestEveryFieldThisPackageErasesIsFilledFromAnArrayItOwns`, which
is the gate doing its job at the statement. And the claim the last round made about `group.go:668`
still holds exactly: of the join pin's clauses **none** fires, and only
`TestTheFounderSurvivesFoundingAndClosingItsOwnGroup` does — which is why that site needed a clause
of its own rather than a copy.

### The disposition keys, all five checked after the spelling change

A disposition key changed spelling — `newRatchet` → `(SecretTree).newRatchet` — when scopes began
naming methods by receiver type, so **every key written before that change is suspect**. All five
were re-read against the declarations they name:

| key | declaration | current? |
|---|---|---|
| `key_schedule.go:newKeyScheduleFromParts.joinerSecret = joinerSecret` | `key_schedule.go:283`, a **function** | yes — a function takes no receiver qualification |
| `…newKeyScheduleFromParts.welcomeSecret = welcomeSecret` | the same | yes |
| `…newKeyScheduleFromParts.epochSecret = epochSecret` | the same | yes |
| `secret_tree.go:(SecretTree).newRatchet.secret = rootSecret` | `secret_tree.go:367`, a **method** | yes — and it is the one the change was about |
| `welcome.go:BuildWelcome.PathSecret = joiner.PathSecret` | `welcome.go:552`, a **function** | yes |

*And the property is held by the gate rather than by this table*: a key matching no site is red and
a caller-rooted site with no key is red, in both directions, on every run — which is why all five
being current is a **measurement** and not a reading of five strings.


# Message repository refactor

Status: final feasibility review and recommendation, 2026-10-05. This assessment
covers the connect and SDK checkouts. It confirms the architecture is feasible;
the refactor has not been implemented. Message-server implementation and
deployment changes require a separate assessment.

Recommend the following four changes:

1. **Create the message repository**, with a `github.com/urnetwork/message` Go
   module, independent releases, documentation, and CI.
2. **Move messaging out of connect**: records, group sessions, MLS, syntax, and
   the messaging protobuf schema and their tests belong in the message repository.
3. **Move messaging out of SDK**: messaging transports, routes, stores,
   device/group orchestration, bindings, probes, and their tests belong in the
   message repository, including the `message/sdk` client packages.
4. **Use a subprotocol for messaging**: the extracted messaging SDK registers an
   application-owned subprotocol through SDK/connect's generic APIs. Both existing
   transport routes use the new carrier.

The final result is a separately owned messaging application running on top of
SDK/connect. Core SDK and connect have no messaging implementation, schemas,
imports, registrations, or message-specific native exports. They expose reusable
transport and subprotocol APIs; the message repository owns all messaging behavior.
Package extraction and carrier migration should remain independently reviewable
steps, with temporary compatibility dependencies removed before completion.

## Final dependency boundary

Allowed production dependencies point from the messaging client to generic SDK
and/or connect APIs, and from core SDK to connect. Neither core SDK nor connect
may depend on the message module, directly or transitively. The five foundational
message packages remain independent of both transport libraries.

An application composes the messaging client with a core SDK device or a
`connect.Client`. Messaging owns the subprotocol ID, registration, envelope,
authentication, version negotiation, fragmentation, and callback lifetime.
SDK/connect transport opaque payloads and expose generic lifecycle operations.
They do not automatically enable messaging or recognize its application types.

This boundary must hold for native bindings and release builds as well as Go
source imports. A core-only application builds and runs without the message
repository. A messaging application adds the message dependency explicitly.
Generic improvements to SDK/connect are allowed where extraction reveals a
missing reusable API; message-specific hooks or special cases are not part of
the final design.

## Repository layout

```text
message/
  go.mod
  go.sum
  CODESTYLE.md
  message/       records, attachments, authentication preimages
  messagegroup/  group sessions, ratchets, record encryption, MLS adapter
  mls/           MLS implementation
  syntax/        shared serialization codec
  protocol/      messaging protobuf schema and application envelope
  sdk/           messaging transports, routes, durable stream store
    urmessage/   device/group orchestration and durable MLS state
```

The repository root contains module metadata and documentation, with no Go
package or facade importing the directories beneath it. The six immediate packages are
peers, consistent with [CODESTYLE.md](CODESTYLE.md): a package may import a peer
but must never import its own descendants. Promote the current `mls/syntax` codec
to `syntax`; keeping it beneath MLS would retain the existing violation.

| Current package or files | Destination | Dependencies within the new module |
|---|---|---|
| `connect/message` | `message/message` | `syntax` |
| `connect/messagegroup` | `message/messagegroup` | `message`, `mls`, `syntax` |
| `connect/mls`, excluding `syntax` | `message/mls` | `syntax` |
| `connect/mls/syntax` | `message/syntax` | None |
| `connect/protocol/message.proto`, its generated code and message-specific tests | `message/protocol` | None |
| Core SDK `message*.go` production files and their tests | `message/sdk` | `messagegroup`, `protocol` |
| `sdk/urmessage` | `message/sdk/urmessage` | Parent `sdk`, `message`, `messagegroup`, `mls`, `syntax`, `protocol` |

These are the current production dependency directions. The principal external
dependencies would be `golang.org/x/crypto` and
`google.golang.org/protobuf`, with their required transitive dependencies. The new
module's five foundational packages must not import connect or the core SDK.
`message/sdk` owns the transport adapters and durable stream store and may import
connect. Its orchestration child owns the durable MLS state implementation.
The default extracted client can use connect directly; an adapter may import
generic APIs from `github.com/urnetwork/sdk` when needed. Such a dependency points
from message to core SDK and must never introduce a dependency back to message.

One module gives MLS, group sessions, record formats, and wire schemas a shared
release version. MLS remains independently importable at
`github.com/urnetwork/message/mls`; it is versioned with the messaging module.
Preserve source history, applicable license notices, vectors, fuzz corpora,
interop material, and package design documents during extraction.

## Move the SDK messaging implementation

This is feasible and makes messaging optional for core SDK consumers, provided
the core SDK and its default native bindings stop importing the moved packages.
A repository move alone cannot reduce a binary that still imports the same code.

The root SDK currently has 15 messaging production files and 10 associated test
files. Move the complete set: client construction, pinned WebSocket routing,
tunnel routing, request/response transport, Hello and nonce management, push
delivery, fragmentation, errors, stream-index reservation, durable stream storage,
and its Unix/Windows exclusion implementations. The `sdk/urmessage` package adds
17 production files and 44 test files for device/group operations, persistence,
restoration, and security gates. Moving only root SDK transport files would leave
the higher-level messaging implementation in the core repository.

Preserving the current package split gives `message/sdk` and
`message/sdk/urmessage`. The child imports its parent; the parent never imports
the child. Consumers would use aliases such as `messagesdk` and `urmessage`.
An alternative is a peer `message/urmessage`, with the same dependency direction.
If the goal is one public import offering device/group operations directly from
`message/sdk`, merging both packages is additional work: reconcile declarations,
remove unnecessary forwarding wrappers, and redesign the package-scoped stream
key checks to preserve their security guarantees. Do not add a parent facade
that imports `sdk/urmessage`; that would violate the layering rule again.

The important dependencies to resolve are concrete:

- [../sdk/message_client.go](../sdk/message_client.go) uses core SDK
  `NetworkSpaceKey`, `NetworkSpaceValues`, `NormalEnvName`, and `ServiceUrl`.
  Prefer explicit platform/API URLs in the extracted constructor, with the core
  application resolving its network-space configuration before calling it.
  If host/environment convenience construction remains, give its small resolver
  an explicit contract and parity tests for production and non-production URLs.
- [../sdk/message_tunnel.go](../sdk/message_tunnel.go) calls private
  `newDeviceClientSettings` in the core SDK. This installs both the operator
  peer-key cross-check and signed key-history verification, as well as completing
  and copying settings. Using `connect.DefaultClientSettings()` as a replacement
  loses behavior. Extract the shared operator-backed configuration into an
  appropriate generic connect or core SDK API, or inject a settings factory with
  an explicit secure default. Keep the existing verification tests with the shared
  implementation. The shared API must have no message-specific dependencies.
- [../sdk/urmessage/device.go](../sdk/urmessage/device.go) accepts and stores a
  concrete `*sdk.MessageTransport`. Rewrite that dependency to the relocated
  parent. Existing store/reserver interfaces can remain injection seams.
- Messaging signatures currently expose `connect/protocol` application types.
  Update these to `message/protocol` while retaining core transfer types from
  connect. Implement the two subprotocol route adapters described below in the
  extracted SDK package.

Keep the core device, generic subprotocol registration/RPC, VPN/TUN APIs, and
network-space configuration in `github.com/urnetwork/sdk`. The new messaging SDK
can work with an injected `connect.Client` without importing that SDK. Its optional
mesh route still needs connect's TUN, provider selection, and tunnel machinery;
this extraction does not make that route a lightweight direct-WebSocket client.

Native packaging is a substantial part of the move. The handwritten
[../sdk/cgo/exports_message.go](../sdk/cgo/exports_message.go), message callbacks,
`include/urnetwork_message.h`, native ABI tests, export lists, and generator checks
must move to the message repository. Three generated
exports also retain root messaging code: `urnet_new_message_transport`,
`urnet_parse_message_route_mode`, and `urnet_open_stream_store`. Regenerate the
core exports without them; simply removing the handwritten ABI file is insufficient.

Keep the core native library messaging-free. If applications need the existing
shared handles, put an optional combined native build in the message repository,
linking both SDKs into one Go runtime. The message repository owns this composition
entry point; it must not become a messaging build option maintained in core SDK.
A separate message native library requires a defined C boundary: opaque handles
and callback registries from the existing library are not automatically valid in
another library. Duplicating handle registries or Go runtimes does not establish
interoperability. A self-contained messaging library can instead own its own
clients, with its own lifecycle and explicit C interfaces.

Move message-specific probes/examples to the new repository. Update external
application imports and keep generic SDK/connect probes in their existing repos. Rebuild
the path-sensitive source, citation, provenance, durable-store, and layering gates
against the new layout, retaining their original assertions. Keep mobile export
checks: the normal `sdk_mobile_bind` build already excludes root messaging files.
Review JavaScript entry points separately; generic subprotocol support remains
core, and moving Go sources does not automatically create a messaging JS API.

Do the client extraction after the foundational packages have moved, initially
using the existing carrier, then migrate the carrier independently. Avoid core SDK
compatibility aliases importing the new messaging package in the final build;
they retain the dependency and reduce the benefit of optional messaging. The
server-safe packages must never import either SDK package.

## SDK library and loaded-memory measurements

Measured locally on macOS arm64 with Go 1.26.8, using the native C shared-library
entry point, `-trimpath`, and release-style `-s -w` stripping. Temporary Go build
overlays excluded messaging without modifying source files. These are comparative
builds of today's code, not built implementations of the proposed repositories.

| Native library variant | File size | Reduction from current |
|---|---:|---:|
| Current, including messaging | 46,755,954 bytes (44.590 MiB) | — |
| Without root SDK messaging, URmessage reachability, and message ABI exports; schema retained | 44,882,338 bytes (42.803 MiB) | 1,873,616 bytes (1.787 MiB) |
| Also without the messaging protobuf schema | 44,650,498 bytes (42.582 MiB) | 2,105,456 bytes (2.008 MiB; 4.50%) |

The second variant models moving client code while connect still links the
messaging schema. The third approximates the eventual core-only build after the
schema/subprotocol separation. The small standalone C message callback wrapper
remained in both comparison builds. Dependency inspection confirmed the third
build does not import `sdk/urmessage`, `connect/message`, `connect/messagegroup`,
or `connect/mls`. Messaging's incremental size is much smaller than all of its
dependencies combined because core transport already uses many shared libraries.

For loaded memory, a minimal native C process loaded each library with `dlopen`,
called the SDK memory-stat export to complete Go initialization, waited 200 ms,
and sampled Darwin `TASK_VM_INFO`. It then called `urnet_free_memory`, waited
200 ms, and sampled again. The C harness avoids introducing another Go runtime
into the measurement process. Nine fresh processes per variant produced these
medians; no device, message client, group, or connection was created:

| Metric | Current | Without message code and schema | Difference |
|---|---:|---:|---:|
| Physical footprint after load | 12,501,784 bytes | 12,305,176 bytes | 196,608 bytes (192 KiB) |
| Physical footprint after explicit trim | 13,583,152 bytes | 13,419,360 bytes | 163,792 bytes (160 KiB) |
| Resident memory after explicit trim | 30,883,840 bytes | 30,343,168 bytes | 540,672 bytes (528 KiB) |
| Go live heap after explicit trim | 1,301,376 bytes | 1,259,776 bytes | 41,600 bytes (about 41 KiB) |

Memory ranges overlap across runs. Treat the footprint difference as roughly
0.1–0.2 MiB for passive loading on this host, not a guaranteed saving on every
platform. Explicit trimming itself executes and faults in code, so these are
separate snapshots, not a monotonic-memory claim. Resident memory includes clean
file-backed pages; physical footprint and Go heap measure different things.
The 2.008 MiB file saving is not 2.008 MiB of immediately resident heap.

These measurements exclude active-client costs: route/tunnel state, goroutines,
queues, fragmentation buffers, MLS groups, history, and durable-store caches.
Measure those separately with controlled numbers of devices/groups and workloads
if messaging runtime budgets are needed. Android/iOS/Windows results require
their actual build and loading paths. Gomobile already excludes client messaging;
its likely additional saving is principally removing the linked protobuf schema,
and must be measured with the mobile toolchain rather than inferred from this table.

## Preserve the server and client boundary

The server-safe packages are `message`, `syntax`, and `protocol`. Neither
`message` nor `protocol` may import `mls` or `messagegroup`, directly or
transitively. `syntax` remains standard-library-only.

The codec must remain separately importable. [message/doc.go](message/doc.go)
records why the server may use the codec while excluding the MLS parser. Merging
syntax into MLS would make that dependency boundary impossible to retain.

Sharing a module does not cause every package to be linked into a binary. Hold
the boundary with package dependency checks: allow the server-safe package
paths explicitly, rather than allowing the entire
`github.com/urnetwork/message` subtree. Enforce recursive layering for every
package and enforce the additional application-specific restrictions above.
Read actual import declarations, including tests and platform-specific files.

## Messaging wire ownership

Move [protocol/message.proto](protocol/message.proto), `message.pb.go`, and the
`protocol/message*_test.go` checks into the new module. The schema is
self-contained: it imports no other proto file. Move its code generation and
wire-number, canonical-encoding, authentication, attestation, and key-delivery
checks with it.

Core `Frame`, `Pack`, `Ack`, transfer control messages, and the generic
`SubprotocolMessage` and peer-query definitions remain in `connect/protocol`.
Generic transfer lanes, contracts, encryption, and routing fields remain core
transport features. This refactor changes the messaging application's carrier,
not those transport mechanisms.

Change the messaging schema's `go_package` to
`github.com/urnetwork/message/protocol`. Initially preserve its protobuf package
name, message names, field numbers, enum numbers, oneof arm numbers, and
canonical encodings. Several tests use descriptor names, and request arm numbers
are authentication inputs, as [message.proto](protocol/message.proto) explains.

Generate each messaging descriptor once. Do not compile both old and relocated
copies of `message.pb.go` into one process. If temporary Go aliases at the old
import path are needed, they must refer to the new types rather than duplicate
the generated schema. Such aliases temporarily make core protocol depend on the
message module and must be removed before transport separation is complete.

Extracted messaging SDK files using both schemas need distinct imports, such as `connectprotocol`
for transfer types and `messageprotocol` for application types. Update exported
Go signatures, fixtures, descriptor lookups, and binding generators together.

## Use a subprotocol in the extracted messaging SDK

Connect already implements the required generic carrier in
[subprotocol.go](subprotocol.go) and
[protocol/subprotocol.proto](protocol/subprotocol.proto). `MessageType_Subprotocol`
is frame value 30; values 31 and 32 provide the peer capability query. The core
treats the application payload as opaque bytes and supplies the existing
delivery, contract, encryption, and routing behavior.

Allocate one stable URmessage subprotocol ID and define it in the message module
without importing connect. Public registration requires an ID of at least
1024. The old messaging frame values 1000 through 1003 belong to a different
number space and cannot simply be reused as public subprotocol IDs.

Define an application-owned envelope in `message/protocol` with four kinds:
request, response, push, and fragment. Its payloads are the existing
`MessageServerRequest`, `MessageServerResponse`, `MessageServerPush`, and
`MessageServerFragment` encodings. New outer framing must not change the bytes
used for request authentication, record authentication, or signatures. Preserve
the inner request correlation and Hello/version semantics.

SDK's current messaging transport sends `connect/protocol.Frame` through
`SendWithTimeout` and receives generic frame callbacks; see
[message_transport.go](../sdk/message_transport.go). Subprotocol frames are
consumed by connect's dispatcher before those callbacks run. Replacing a frame
number without changing receive registration would therefore lose replies.

Introduce one messaging SDK carrier seam for application payload sends, receives, and
session replacement notifications. Adapt both existing routes behind it:

| SDK route | Required change |
|---|---|
| `MessageClient` or a supplied `connect.Client` | Register an URmessage listener with `AddSubprotocolRawCallback` or a typed codec. Send through the generic subprotocol API. Remove the registration when the binding closes. |
| `MessageRouteClient` | Wrap and unwrap the same subprotocol envelope on the pinned WebSocket connection. This route currently serializes `protocol.Frame` directly and does not execute `connect.Client` dispatch. |

Update the `MessageTransportClient` seam and its callers deliberately: its
current method set is an exported Go API, and changing it requires coordinated
consumer changes or an explicit compatibility adapter. Preserve both the mesh
and direct WebSocket routes, TLS pin verification, reconnect notifications,
Hello nonce replacement, and transport shutdown behavior.

Keep registration and transport-specific code in `message/sdk`. `message/protocol` owns
the application envelope and its codec, without depending on connect. A typed
adapter can use `connect.ProtoCodec` where suitable; a raw adapter must decode
the application envelope itself.

### Fragmentation and buffer ownership

Subprotocols do not fragment oversized payloads. Retain messaging's existing
fragmentation and reassembly behavior, including request IDs, ordered parts,
abort rules, and cleanup. Account for the application envelope and generic
subprotocol wrapper when choosing a part size that fits the applicable frame
budget. Keep fragment limits owned by the messaging layer rather than copying
new constants into core connect.

Make the SDK carrier's buffer contract explicit. The current frame send
interface transfers ownership on success. `SendSubprotocolBytesWithTimeout`
returns its input buffer on every outcome, including refusal. Adapters and
fragment cleanup must account for that difference to avoid returning a buffer
twice or leaking unsent parts. Receive payloads are borrowed until callback
return; retain or copy bytes that outlive the callback.

Receive callbacks remain nonblocking. Preserve the existing waiter correlation
and push handoff behavior, and pass authenticated source and logical transfer
metadata through the connect adapter. Moving to a subprotocol must not add a
blocking reply send to a shared receive callback.

## Tests and integration ownership

This layout preserves many existing relative paths: MLS still has `../message`
and `../messagegroup`, and the record package still has `../mls` and
`../messagegroup`. The crypto and record checks can continue to cover these
packages in one module. Update hard-coded import paths, source fixture strings,
module-derived scan scopes, and test expectations together; do not weaken a
check merely because its path changed.

Move MLS, syntax, record, and group tests into the new repository.
Preserve vector, fuzz, race, interop, and cross-platform coverage and the
appropriate toolchain pin.

Some record checks optionally inspect a sibling SDK checkout; see
[message/record_test.go](message/record_test.go). Give this coverage an explicit
integration job with pinned message and SDK revisions. A standalone message
checkout must exercise all of its own package checks, while integration CI must
fail if a required external checkout is absent. Update SDK's source and citation
checks to resolve the new module and package paths.

Messaging consumers currently in SDK include `urmessage`, message stores and stream adapters, the
transport implementations, C bindings, probes, and their tests. Update affected
nested `go.mod` files and local development replacements, including build, C,
JavaScript, probe, and acceptance modules where they require the moved packages.

## Implementation stages

1. **Create the message repository.** Establish the module, layout, independent
   release process, documentation, CI, and dependency-boundary checks.
2. **Move message parts from connect.** Extract `message`, `messagegroup`, `mls`,
   and `syntax`, preserving their dependency directions, tests, and fixtures.
   Relocate messaging protobufs and their checks as a separate reviewable change,
   regenerate once, and update application types. Keep the existing carrier while
   validating relocation. Any compatibility aliases are temporary.
3. **Move message parts from SDK.** Extract root messaging code and `urmessage`,
   resolve URL/settings dependencies through generic APIs or injection, and move
   messaging bindings, probes, and integration checks. Establish core-only native
   packaging and message-owned composition builds. Update consumers while
   preserving existing carrier behavior for this step.
4. **Use the messaging subprotocol and finish separation.** Allocate the ID,
   define the application envelope, implement both messaging SDK route adapters,
   and migrate request, response, push, and fragmentation paths. Apply the chosen
   rollout policy. Remove temporary aliases and legacy application frame handling,
   and reserve old frame values and names against reuse. Remove messaging imports,
   generated descriptors, registrations, native exports, and release dependencies
   from both core repositories. Verify the final dependency boundary.

The five foundational packages must not depend on connect or the core SDK.
The extracted messaging SDK may depend on generic SDK/connect APIs; neither core
repository may depend on message. Preserve message-owned native ABI contracts in
the messaging distribution while removing those exports from the core-only
distribution. Update consumer packaging explicitly and run generator/currentness
checks for each distribution.

## Compatibility decision

The outer carrier change is a wire change. Existing peers expecting frame values
1000 through 1003 do not automatically understand the new subprotocol envelope.
Choose between a coordinated client/server switch and a period of dual-carrier
support before implementation of stage 4.

If dual support is required, select one carrier per session before an application
request is sent. Capability query timeouts mean support is unknown, not absent.
Define negotiation separately for real connect peers and WebSocket sessions, and
test the mixed-version matrix. Do not resend an uncertain write through a second
carrier merely because the first reply timed out; that introduces an application
duplicate unless the operation's deduplication contract explicitly permits it.

Message-server peer and WebSocket endpoints must agree with the new carrier and
schema. Their implementation and rollout effort is outside this connect/SDK
assessment, so SDK-only acceptance cannot establish end-to-end deployment
compatibility.

## Completion checks

- Every package passes recursive layering checks, and the server-safe dependency
  closure excludes MLS and messagegroup.
- The five foundational packages build without connect or the core SDK; the
  extracted messaging SDK uses only generic SDK/connect transport APIs. All
  local checks, vectors, fuzz seeds, and required platform builds are retained.
- Authenticated inner messages remain byte-compatible: canonical requests,
  operation bytes, record preimages, and attestation/signature inputs agree with
  the pre-refactor implementation.
- Both SDK routes carry Hello, requests, responses, pushes, and fragmented
  traffic through the application envelope. Real connect tests exercise actual
  subprotocol registration and dispatch.
- Request correlation, refusals, fragment boundaries, cancellation, reconnect
  nonce replacement, listener removal, and pooled-buffer reconciliation have
  deterministic coverage.
- Core-only and messaging native distributions build separately, existing
  messaging consumers use the new distribution, JavaScript consumers compile,
  mobile export boundaries remain correct, and native ABI/generator checks pass.
- If legacy support is selected, old/new client and server combinations have
  explicit tests before legacy handling is removed.
- Neither core SDK nor connect has a direct or transitive production dependency
  on the message module, including platform-specific code and release bindings.
  Both build and run without the message checkout or messaging runtime setup.
- Messaging schemas, clients, stores, subprotocol registration, and native exports
  live in the message repository. Core SDK/connect contain no message-specific
  dispatch or active handling of the retired application frame values.
- The extracted messaging SDK uses the generic subprotocol carrier. An application
  can attach it using public APIs without patching or rebuilding core SDK/connect
  with messaging code; native composition follows the explicit ABI boundary above.

## Effort and tradeoffs

| Work | Relative effort | Main uncertainty |
|---|---|---|
| Repository and package extraction | Medium | Path-sensitive gates, fixtures, CI, and SDK module updates |
| Messaging schema relocation | Medium | Public Go type changes and descriptor compatibility |
| Messaging SDK extraction | Medium to high | Shared secure settings, package boundaries, native ABI and optional packaging |
| SDK subprotocol adapters | Medium to high | Two route implementations, fragmentation, and buffer contracts |
| Mixed-version compatibility | Potentially the largest portion | Whether concurrent legacy and new carriers are required |

The shared repository moves more code than an MLS-only extraction, but preserves
the existing cross-package security checks and gives the messaging implementation
one release boundary. The main tradeoff is that MLS versions travel with the
messaging module. Repository extraction and carrier migration remain separately
reviewable changes.

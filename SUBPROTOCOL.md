# Subprotocols on the core transfer layer — design proposal

Status: proposal for review, 2026-09-10. Nothing here is implemented. The
open questions in §8 are the ones to settle before code.

## 0. What is being asked for

An external user of the SDK (a Go program importing `connect`, and later the
mobile SDK) wants to run its own protocol over URnetwork's core: reliable,
ordered, contract-accounted, end-to-end encrypted delivery between clients,
without touching `frame.proto` or the transfer layer for each new protocol.
Concretely:

- register a **subprotocol id** (16-bit) on a `Client`, with a **pluggable
  marshaller** that follows the Go protobuf convention (size, marshal-append,
  unmarshal), so the payload can be protobuf, a hand-rolled codec, or anything
  else the user owns;
- send and receive messages of that subprotocol through the existing send and
  receive machinery, with the wire carrying a small subprotocol header and the
  raw payload bytes;
- keep the hot path as cheap as the core's own frames: every byte written into
  a `message_pool` buffer once, no second copy between the marshaller and the
  frame, no per-message allocations beyond what a core frame already costs.

## 1. How a message travels today

The facts the design leans on, with the code that establishes them.

**One frame type, one enum.** Every application message is a
`protocol.Frame{message_type, message_bytes, raw}` (`protocol/frame.proto`).
`MessageType` is one flat enum ("flatten all message types into this enum"),
currently 0..29. `raw = true` means `message_bytes` is the message itself, not a
protobuf; the IP data path uses it so a packet is carried without the
`IpPacketToProvider{IpPacket{packet_bytes}}` wrappers (`frame.go`
`ipPacketToProviderFrame`, `FromFrame` raw branches). Older peers ignore an
unknown `MessageType` (the `TransferResidentMigrate` and
`IpIpProviderDiagnostics` comments rely on this).

**Frames ride in Packs.** `Client.Send*` takes a `*protocol.Frame` and a
destination; the sequence layer batches frames into a `Pack` (at most
`sendPackBatchMaxFrames = 2` data frames per pack on the current sender),
encodes the `TransferFrame` with the hand-rolled codec in `frame_protobuf.go`
(`sendPackFrame.appendPack` → `appendFrame`, which does one
`append(b, f.MessageBytes...)` into the pooled pack buffer), then the pack is
acked, resent, and optionally wrapped by the per-peer encryption session.
Intermediaries route on the `TransferPath` only; "only the destination inspects
the payload" (DESIGNNOTES §1). Multi-hop (`SendMultiHop`) works the same way.

**Ownership is explicit and pooled.** `message_pool.go` states the three rules:
an owner returns a buffer with `MessagePoolReturn`; ownership is handed off on
send (`Send*` "takes ownership of the frames' message bytes AND of the `frames`
slice itself", `SendMultiWithTimeout` header; the sequence returns them after
marshalling, `transfer.go` 2079/2258/2951); and received bytes are borrowed for
the duration of the receive callback (`ReceiveFunction` doc: "borrowed and valid
only until the callback returns; share or copy data that must outlive it").
`ProtoMarshalWithTag` is the model for marshalling into the pool:
`MessagePoolGet(proto.Size(m))`, `MarshalAppend(buf[:0], m)`, and a cap check
that returns the pool buffer if the marshaller grew past it. Pool classes are
256 B, 2048 B and two large classes; a slice keeps its pool identity only while
its `cap` is the class size plus the 12-byte meta, so a sub-slice
(`b[2:]`) is no longer a pool buffer.

**Receive is inline and allocation-bounded.** Inbound packs decode into a
`decodedPackOwner` (inline `Frame` storage for the common two-frame pack);
`decodeFrameInto` copies `message_bytes` once from the transport buffer into a
pool buffer (`f.MessageBytes = MessagePoolCopy(v)`). `ReceiveSequence.flushDeliver`
hands the batch's frames to `Client.receive`, which fans out to every
registered `ReceiveFunction` (a `CallbackList`), then acks and returns the pool
buffers. Before delivery the sequence already **intercepts one frame type**:
`deliverEncryptedControlFrames` pulls `TransferEncryptedControl` frames out of
the batch and routes them into the encryption session, passing the rest to the
application (`transfer.go` ~10328). That is the precedent for a typed dispatch
ahead of the generic callbacks.

**Consumers switch on the type.** The IP layer's receive callback switches on
`frame.MessageType` and reads the raw bytes with `ipPacketToProviderBytes`
(`ip.go` ~7778); the SDK's provider registers a receive callback the same way
(`sdk/device_local_provider.go:213`). Every consumer of a frame it does not own
ignores the type; nothing today errors on an unknown type in the receive path,
only `FromFrame` does when asked to decode one.

**Size bounds.** A frame must fit one transport message: each transport's
framer `MaxMessageLen`, whose floor is `ClientSettings.MinimumMessageLenLimit()`
= 8 KiB (`transfer.go` 1318), minus the pack and encryption overhead. There is
no fragmentation at the frame layer.

## 2. Design goals and non-goals

Goals:

1. A subprotocol is data to the core: one new `MessageType`, one new `Frame`
   field, no per-subprotocol code in `connect`.
2. Zero extra copies: the marshaller writes straight into the pool buffer that
   becomes `Frame.message_bytes`; the pack encoder's single append into the
   wire buffer is the only copy on send (as for every frame today); on receive
   the codec unmarshals from the pooled `message_bytes` the decoder already
   produced, in place.
3. Zero extra allocations on the hot path beyond the `Frame` struct the core
   already allocates per send; on receive, none (the decoded frame lives in
   the pack owner's inline storage).
4. Registration per `Client`, lock-free on the receive path.
5. Wire compatibility: an old peer or an old platform drops the frames and
   keeps the sequence healthy; the hand-rolled codec and `proto.Unmarshal` both
   decode the new field or skip it.

Non-goals (v1): fragmentation of messages larger than a pack, a
subprotocol-level handshake or version negotiation (that is the subprotocol's
own business, see §8 Q7), mobile SDK bindings (§8 Q9).

## 3. Wire format

Three ways to put a 16-bit id on the wire were considered.

| | A. header inside `message_bytes` | B. nested `SubprotocolFrame` message | C. new `Frame` field |
|---|---|---|---|
| shape | `raw=true`, `message_bytes = [id:2][payload]` | `raw=false`, `message_bytes = proto{id, payload}` | `message_type=Subprotocol, subprotocol_id=id, raw=true, message_bytes=payload` |
| extra copy on send | none if the marshaller appends after the 2 header bytes | one, unless hand-rolled | none |
| payload bytes on receive | `message_bytes[2:]`: a sub-slice, no longer a pool identity | `message_bytes` inner slice | `message_bytes` exactly |
| decodable by generic protobuf tools | id is opaque | yes | yes (a Frame field) |
| header cost | 2 B | ~5 B | 2–4 B (tag + varint) |
| codec change in `frame_protobuf.go` | none | none | one field in `sizeFrame`/`appendFrame`/`decodeFrameInto` |

**Recommendation: C.** The id becomes a first-class field of the frame:

```proto
enum MessageType {
    ...
    IpIpProviderDiagnostics = 29;
    // A message of a subprotocol registered on the receiving Client
    // (SUBPROTOCOL.md). `Frame.subprotocol_id` names the subprotocol and
    // `message_bytes` is the subprotocol's own encoding (`raw` is set). Older
    // clients ignore the unknown message type.
    Subprotocol = 30;
}

message Frame {
    MessageType message_type = 1;
    bytes message_bytes = 2;
    bool raw = 3;
    // set only for `Subprotocol` frames; 1..65535, 0 is invalid
    uint32 subprotocol_id = 4;
}
```

Why C over A: with A the payload the codec sees is `message_bytes[2:]`, which
has lost its pool identity (`cap` no longer matches a class), so every helper
that shares or returns it silently becomes a no-op and the codec cannot hand
the slice on; with C `message_bytes` is the payload, so the existing ownership
rules apply to it unchanged. Why C over B: B is a second length-prefixed
message inside the frame, which either costs a copy (marshal the payload, then
marshal the wrapper) or a second hand-rolled codec; C reuses the frame codec
that already exists and adds one varint. The hand-rolled codec skips unknown
fields (`decodeFrameInto` default branch), `proto.Unmarshal` does too, and the
platform never decodes payloads, so the field is safe to add without a
protocol version gate.

`subprotocol_id` is `uint32` on the wire (protobuf has no 16-bit scalar); the
Go API is `SubprotocolId uint16` and the codec rejects values outside 1..65535.

## 4. The marshaller ("go protobuf convention")

The convention we match is `proto.Size` + `proto.MarshalOptions.MarshalAppend`
+ `proto.Unmarshal`: size first, so the pool buffer is taken once at the right
class; append into a caller-provided buffer, so the bytes land where the frame
needs them; unmarshal from a borrowed slice. Two shapes are possible.

**Per-message interface** (each message type implements it):

```go
type SubprotocolMessage interface {
    Size() int
    MarshalAppend(b []byte) ([]byte, error)
    Unmarshal(b []byte) error
}
```

**Per-subprotocol codec** (one object per registered id, messages stay plain
values):

```go
// Codec is the pluggable marshaller of one subprotocol. It is called from the
// client's send path and, for Unmarshal, inline on the receive goroutine; it
// must be safe for concurrent use.
type SubprotocolCodec[T any] interface {
    // the exact encoded size of m; the send path takes one pool buffer of this size
    Size(m T) int
    // appends the encoding of m to b (len(b) == 0, cap(b) >= Size(m)) and returns it
    MarshalAppend(b []byte, m T) ([]byte, error)
    // decodes b into m; b is borrowed (see §5) and must not be retained
    Unmarshal(b []byte, m T) error
}
```

**Recommendation: the codec, generic over the message type.** A proto-generated
Go message does not carry `Size`/`MarshalAppend` methods (protoc-gen-go emits
neither; `proto.Size(m)` and `MarshalAppend` are package functions), so the
per-message interface would force a wrapper type on every protobuf user
anyway. The codec shape lets `connect` ship one adapter for any
`proto.Message`:

```go
// ProtoCodec adapts a protobuf message type to SubprotocolCodec using
// proto.Size, MarshalOptions.MarshalAppend and UnmarshalOptions.Unmarshal.
func ProtoCodec[T proto.Message]() SubprotocolCodec[T]
```

and lets a user with a hand-rolled or flatbuffer encoding implement three
methods once. A codec may also own a bounded free list of `T` instances (the
`decodedPackOwner` pattern: sharded, capacity-bound, never `sync.Pool`), which
the per-message interface cannot express.

Marshal path (the `ProtoMarshalWithTag` recipe, applied to any codec):

```go
n := codec.Size(m)
buf := MessagePoolGet(n)                       // one pool buffer, the class for n
out, err := codec.MarshalAppend(buf[:0], m)    // writes in place
if cap(out) != cap(buf) { MessagePoolReturn(buf) }   // codec overran Size: the pool buffer is orphaned
frame := &protocol.Frame{MessageType: Subprotocol, SubprotocolId: uint32(id), MessageBytes: out, Raw: true}
```

A codec whose `Size` underestimates still works (the append grows into a heap
slice) but loses the pool; the readout in §7 counts those so a bad codec is
visible rather than silently slow.

## 5. Receive: dispatch, lifetime, typed delivery

Dispatch happens in `Client.receive`, before the generic `ReceiveFunction`
fan-out, mirroring `deliverEncryptedControlFrames`:

```go
func (self *Client) receive(source TransferPath, frames []*protocol.Frame, peer Peer) {
    frames = self.subprotocols.dispatch(self, source, frames, peer)   // strips Subprotocol frames
    if len(frames) == 0 { return }
    for _, receiveCallback := range self.receiveCallbacks.Get() { ... }   // unchanged
}
```

`dispatch` walks the batch once; for each `MessageType_Subprotocol` frame it
looks the id up in an immutable table published through an `atomic.Pointer`
(registration copies the table; the receive path takes no lock), and calls the
handler. The remaining frames are compacted in place (no new slice) and
delivered as today. A batch with no subprotocol frames costs one type check
per frame.

Handler shape, typed through the codec:

```go
// A handler runs inline on the receive goroutine and must not block; `message`
// and any bytes it aliases are borrowed until the handler returns (the same
// contract as ReceiveFunction). `peer` carries provide mode, roles, principal
// and the TransferKey to reply on.
type SubprotocolHandler[T any] = func(source TransferPath, message T, peer Peer)
```

Per frame the dispatcher does: take a `T` from the codec's free list (or
`new(T)` when the codec has none), `codec.Unmarshal(frame.MessageBytes, m)`,
call the handler, release `m`. `frame.MessageBytes` is the pool buffer the
frame decoder already produced, owned by the receive item and returned by
`flushDeliver` after the callback: the codec reads it in place, and a
protobuf codec's `bytes`/`string` fields alias it only if unmarshalled with
aliasing options, which is why the handler contract says borrowed. A handler
that must keep the message calls `RetainSubprotocolBytes(frame)`, which is
`MessagePoolShareReadOnly` plus a `MessagePoolReturn` obligation, or the codec
copies what it keeps. Decode failures are counted per id and the frame is
dropped, never delivered raw.

Unregistered ids are dropped and counted (§8 Q4). Subprotocol frames never
reach the generic `ReceiveFunction`s: every existing callback switches on the
types it owns, and a raw frame with an id nobody registered has no consumer.

Because the handler runs inside the receive sequence's delivery, the whole
existing backpressure story applies: a slow handler slows the sequence, exactly
as a slow `ReceiveFunction` does today. A subprotocol that needs a queue builds
one on top with the zero-timeout, drop-when-full rule from the `ReceiveFunction`
doc; `connect` does not add one.

## 6. Client API

```go
type SubprotocolId uint16

// Registers codec and handler for id on this client. Registration is rare and
// takes the registry lock; the receive path reads an immutable snapshot. An id
// already registered is an error; the returned func unregisters.
func RegisterSubprotocol[T any](client *Client, id SubprotocolId, codec SubprotocolCodec[T],
    handler SubprotocolHandler[T], opts ...SubprotocolOption) (unregister func(), err error)

// Marshals m with the registered codec into one pool buffer and enqueues it as
// one frame; the same destination, ack callback, timeout and send options as
// Send/SendWithTimeout/SendMultiHop. Ownership of the bytes passes to the send
// on success; on failure the buffer is returned here.
func SendSubprotocol[T any](client *Client, id SubprotocolId, m T, destinationId Id,
    ackCallback AckFunction, opts ...any) bool
func SendSubprotocolWithTimeout[T any](...)
func SendSubprotocolMultiHop[T any](...)

// Several messages of one subprotocol in one pack (SendMulti): one pool buffer
// per message, one frames slice.
func SendSubprotocolMulti[T any](client *Client, id SubprotocolId, ms []T, destinationId Id,
    ackCallback AckFunction, opts ...any) bool
```

Package-level generic functions rather than methods because Go methods cannot
be generic; they are thin wrappers over `client.Send*` and the registry, so the
`Client` type gains only the registry field and `receive`'s first line. The
send path does not require the id to be registered locally (a client may only
send a subprotocol), but it does require a codec: `SendSubprotocol` takes it
from the registration when present and from an explicit `WithCodec(codec)`
option otherwise (§8 Q2).

Options at registration (`SubprotocolOption`): `AcceptProvideModes(...)` to
drop frames from peers outside a provide mode set before the handler runs
(the `Peer` already carries it; this only saves the unmarshal), and
`MaxMessageByteCount(n)` to reject oversized frames before decoding.

What does not change: contracts and accounting (`MessageByteCount` is
`len(MessageBytes)` as for any frame), encryption (frames inside packs are
wrapped as a unit), acks (`AckFunction` remains the pack-level ack), routing
(the path is untouched), the platform (it forwards `TransferFrame`s without
reading payloads; its own `Client` instances drop the unknown type).

## 7. Cost accounting

Send, per message, today for a protobuf control frame built with `ToFrame`:
the message struct, `proto.Size` + reflection marshal into one pool buffer,
one `Frame` struct, one `SendPack`, then one append into the pack wire buffer
(the frame codec) and the pool buffer is returned. With the codec path: the
`Frame` and `SendPack` structs are unchanged, the marshal is the codec's own
(no reflection for a hand-rolled codec, the same reflection for `ProtoCodec`),
the pool buffer is taken once at `Size(m)`, and the append into the pack is the
same single copy every frame pays. Net: zero added copies, zero added
allocations; on the wire, 2–4 bytes for field 4.

Receive, per frame, today: the frame decoder's one `MessagePoolCopy` from the
transport buffer into a pool buffer, the inline `Frame` in the pack owner, and
whatever the consumer does. With dispatch: one table lookup, one codec
`Unmarshal` in place, one `T` from the codec's free list. Net: zero added
copies, zero added allocations when the codec pools its messages.

Verification is part of the implementation: `testing.AllocsPerRun` on the send
and receive paths for a hand-rolled codec must report the same counts as an IP
raw frame, and a benchmark against `ToFrame(SimpleMessage)` sets the reference
for `ProtoCodec`. A per-client stat block (`SubprotocolStats`: sent, received,
dropped-unregistered, dropped-decode, dropped-oversized, marshal-overrun) is
exposed next to `ClientReceiveStatsSnapshot` so a misbehaving codec shows up in
numbers.

## 8. Open questions to settle before implementation

1. **Wire shape.** C (a `Frame.subprotocol_id` field, `raw` payload) as
   recommended in §3, or A (a two-byte header inside the bytes, no proto
   change)? C is cleaner for ownership and tooling; A touches no proto and no
   codec. Recommendation: C.
2. **Codec vs per-message interface**, and whether a send without a local
   registration is allowed (with an explicit codec option) or every sender
   must register. Recommendation: generic codec; sends require a codec, from
   the registration or an option.
3. **Typed delivery vs bytes.** Should the handler receive the decoded `T`
   (codec-owned instance, released after the call) or the raw `[]byte` and
   decode itself? Typed is proposed; a `RegisterSubprotocolBytes` variant with
   a `func(source, id, bytes, peer)` handler is cheap to add for users who want
   zero framework between them and the bytes. Recommendation: both, typed
   first.
4. **Unregistered id policy.** Drop and count (proposed), or deliver the raw
   frame to the generic `ReceiveFunction`s so a catch-all can see it?
   Recommendation: drop and count; a catch-all is a bytes registration on the
   ids it wants.
5. **Id space.** 0 invalid. Reserve a range for URnetwork's own future use
   (e.g. 1..255) and leave 256..65535 to external users, or first-come? The
   platform cannot police it; two apps on one client are the only conflict
   case, and registration refuses a duplicate. Recommendation: reserve 1..255,
   document the rest as user-managed.
6. **Destinations.** Peer clients and multi-hop are in scope. Is sending a
   subprotocol frame to `ControlId` (the platform) meaningful in v1? The
   platform's clients would drop it. Recommendation: allowed but documented as
   dropped until the platform registers handlers; no special casing.
7. **Size and fragmentation.** A message must fit one transport message (8 KiB
   floor, per-transport `MaxMessageLen`, less pack and encryption overhead).
   Should v1 refuse oversized sends up front with a clear error (proposed), or
   fragment? Recommendation: refuse; fragmentation is a subprotocol-level
   concern until a second user needs it in the core.
8. **Delivery semantics.** Frames arrive reliably and in order per sequence,
   but an old peer that does not know the type acks the pack and discards the
   frame, so "acked" does not mean "understood". A subprotocol that needs to
   know its peer speaks it needs its own hello. Should `connect` offer a
   capability bit (the peer advertises supported subprotocol ids) as part of
   the existing peer capability exchange? Recommendation: not in v1; note it.
9. **SDK exposure.** The Go API is generic and cannot cross gomobile. The
   mobile SDK would need a bytes-only registration (`id`, `func(bytes)`) on
   `DeviceLocal`. In scope now, or a follow-up once a mobile consumer exists?
   Recommendation: follow-up.
10. **Handler threading.** Inline on the receive goroutine, must not block, as
    today's callbacks (proposed). Offer an optional bounded handoff queue in
    the registration options, or leave it to the user? Recommendation: leave it
    to the user, with the doc pointing at the drop-when-full rule.
11. **Naming.** `Subprotocol` for the message type and `subprotocol_id` for the
    field, `SubprotocolId`/`SubprotocolCodec`/`SubprotocolHandler` in Go, one
    new file `subprotocol.go` plus `subprotocol_test.go` in package `connect`
    (CODESTYLE: shared code stays in the parent package).

## 9. Implementation sketch (after the answers)

1. `protocol/frame.proto`: `Subprotocol = 30`, `Frame.subprotocol_id = 4`;
   regenerate `frame.pb.go` (`protocol/Makefile`).
2. `frame_protobuf.go`: field 4 in `sizeFrame`, `appendFrame`,
   `decodeFrameInto`; round-trip tests against `proto.Marshal` in
   `frame_protobuf_test.go` (the byte-identical invariant).
3. `frame.go`: `FromFrame` returns a typed error for `Subprotocol` ("decoded by
   its codec"); `ToFrame` unchanged (subprotocol frames are built by the
   codec path).
4. `subprotocol.go`: `SubprotocolId`, `SubprotocolCodec`, `ProtoCodec`,
   registry (immutable table behind `atomic.Pointer`, registration lock),
   dispatch, `Register*`/`Send*` functions, stats, `RetainSubprotocolBytes`.
5. `transfer.go`: the registry field on `Client`, the one-line dispatch in
   `receive`, stats exposure.
6. Tests: two clients over the test transport exchanging a hand-rolled codec
   and a `ProtoCodec(SimpleMessage)`; unregistered, oversized and bad-decode
   drops counted; old-peer compatibility (a frame with field 4 decoded by the
   pre-change decoder path and by `proto.Unmarshal`); `AllocsPerRun` on both
   paths; a benchmark next to the pack codec benchmarks.
7. Docs: DESIGNNOTES §1 gains the subprotocol layer line; this file becomes
   the reference and drops "proposal".

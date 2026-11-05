# FILLFRACTION: the contract fill fraction, and no-acknowledgement Packs at a contract boundary

The owner, 2026-11-05, in order:

> The transfer will only fill contracts half full. So a 64mb ceiling will cap contract content at 32mb. Let's keep the current 128mb

> Perhaps it's the half fill logic in connect/transfer that needs revisiting. Perhaps we should look at something like .7 or .8 . The fill fraction should be a setting.

> the fill fraction is for non acked transfer, because it won't wait for a new contract

> Perhaps we should review the fill fraction generally. It seems wrong that non ack won't wait for a new contract or update the contract as usual

> we need to make sure that no-ack also advances new contracts

Line numbers are connect origin/main b8a746af2 unless a section says otherwise.

## 1. Verdict

- **The setting already exists.** `SendBufferSettings.ContractFillFraction` (transfer.go:990) was 0.8. It was 0.5 from its introduction (253f60cd, 2024-03-19) until ca6b07ec (2026-03-23), which is the half the first message remembers. It applies only to the sender. Receivers have built receive contracts with 1.0 since the same commit, so they have always admitted the whole signed size.
- **Nothing reads the headroom today (§2).** The field's original comment said what it was for: "as this ->1, there is more risk that noack messages will get dropped due to out of sync contracts".
  - That risk was a no-acknowledgement Pack with no contract id, which the receiver charged to whatever contract it held current. During a switch, the predecessor absorbed the successor's datagrams.
  - No-acknowledgement Packs have carried their contract id since 2024 (172931e9 `if !ack && contractId != nil`), and the receiver resolves the id exactly (`receiveNack`, 17427-17530).
  - Every full contract still left a fifth of its escrow uncarried.
- **The owner's second point is the real defect (§3).** A no-acknowledgement Pack could not advance the contract itself.
  - At a boundary it was deferred, then promoted onto the acknowledged lane while a successor opened unacknowledged.
  - A sequence carrying only no-acknowledgement traffic never announced a successor ahead at all.
- **An adjacent defect turned up in the same code (§4).** Every contract switched to from its announcement kept one unacknowledged byte for its whole life, so once retired it never closed. The payer's escrow for it stayed held until the sequence ended or the platform expired the contract.
- **The change (§5):**
  - a no-acknowledgement Pack advances the contract itself, the way an acknowledged Pack does, and is never promoted to do it;
  - the opening reservation is released;
  - the default fill fraction is 1, and it stays a setting;
  - counters make the fleet effect visible.

## 2. What the headroom protects: each candidate

| Candidate | Finding | Source |
|---|---|---|
| Fast-path writes charged after the fact | No. Callers reserve from `noAckRemainingByteCount`, which starts from the effective size less what is spent and is shared with ordinary debits once published. A reservation can never pass the effective size. | transfer.go:9024-9026, 18415-18418, 8915-8962 |
| Minimum message rounding, sender against receiver | No. `MinMessageByteCount` is one byte on both sides, now and in 172931e9 and ca6b07ec. | transfer.go:955, 1094 |
| Duplicates or retransmits debited twice at the receiver | No. A Pack below the next sequence number is dropped before the debit ("drop past sequence number"). | transfer.go:17156-17185 |
| Framing or contract-frame bytes counted on one side only | No. Both sides count data frames only. A control Pack counts one minimum message on both sides. | `MessageByteCount` transfer.go:19135; receive 4955 |
| Successor acquisition latency | No. The sender stops at the effective size and never spends the headroom while it waits. The successor request and the announcement threshold are both computed against the effective size. | successorContractDue 11027-11060; announceAheadByteCount 11086-11103; 11149 |
| Server grant, escrow, settlement | No. The grant is `min(max(Min, request), 2 × standard)`. The 2× has been there since e0cab169a (2025-11-07), the day the client scaled its minimum request by `1/fill` (connect e4f4d404). Escrow is the grant. Settlement compares the two parties' counts within `AcceptableTransfersByteDifference`. A sender that fills to 1 reports at most the signed size, which the receiver admits too. | server controller/connect_controller.go:52, 1323-1329; model/subscription_model.go:3603-3616 |

**Measured on Main, 2026-11-05.**
- **The 0.8 cap shows in the data.** In three minutes from 13:00Z, the contracts of at least 4 MiB that used at least 30% of their grant spike at 75-80% used (92 of 211).
- **Over 30 s from 13:30Z:**
  - 142 of 21,228 contracts (0.67%) reached 75% of their grant;
  - they carried 0.754 of 1.451 GiB, 52% of all bytes;
  - they were granted 0.942 GiB, so 0.188 GiB was escrowed and never carried in that sample alone.

## 3. A no-acknowledgement Pack at a contract boundary, before the change

1. **It may not switch contracts.** It debits only the current contract, and only once acknowledged (10618-10620). It may not switch to the announced successor, even one the receiver has acknowledged (10636-10642: "a Pack that may not promote may not change the contract under the sequence either"). It is deferred to recovery admission (8346-8353, 10093-10096).
2. **Ordinary admission promotes it.** In ordinary admission it is debited with promotion allowed: it switches to an unacknowledged successor, or opens one with `createContract`. `prepareSendItem` then promotes it: `effectiveAck = ack || self.sendContract != nil && !self.sendContractAcked` (transfer_send_prepared_item.go:21). It is sequence-numbered, retained, resent and head-of-line, for a round trip at every boundary. Waiting in recovery admission also means it waits behind resend capacity it never uses, which is exactly when capacity is short.
3. **A no-acknowledgement-only sequence never announces.** `maybeAnnounceContractAhead` returned whenever no reliable item was outstanding (11136-11138), which on a UDP-only flow is always. Every boundary took (2).
4. **A fast-path flow never reached the announcement.** Fast-path writes are charged on the owner's pass, which asked only for the successor's request (9683-9686). The callers' wake was armed only for that request.
5. **A no-acknowledgement Pack carried a frame that was due.** It took `sendContractFrameDue` (11619-11633) and carried the new contract's frame. The receiver handles a no-acknowledgement Pack on arrival, so a switch made on it can charge Packs still in flight under the predecessor to the successor.

**Measured.** The composed row below, before the change, promoted one message at each of four boundaries: "4 no-ack messages crossed the wire on the acknowledged lane". THROUGHPUTFIX §39.1 measured 23-47% of a contract's life promoted at 200-400 ms before announcements existed. The announcement fixed that for sequences with reliable items outstanding, but not for no-acknowledgement flows.

## 4. The opening reservation

**The defect:**
- `announceContractAhead` charges the successor one minimum message when it is announced (11196-11202), "the same one `setNextContract` makes". In `setNextContract` that debit is the contract-only head's, which the receiver counts and the head's acknowledgement moves.
- For an announced successor, no Pack under the successor carries it. The announcement is charged to the predecessor on both sides, and the receiver never counts it. Nothing released it when the successor became current.
- So the contract held one unacknowledged byte for its whole life. `tryCloseRetiredSendContract` closes a retired contract only when its unacknowledged count equals its abandoned count (transfer_no_ack_contract_lifetime.go:75), so the contract stayed open after retirement.
- The payer's escrow for it stayed held until the sequence ended, or until the platform expired the contract at its 60-minute deadline. Each such contract also closed one byte apart from its receiver.

**The fix.** `setAheadContract` releases the reservation when the successor becomes current, where the successor's own traffic takes over its accounting. `openingReserved` marks it. `discardAheadContract` still releases it for a successor that is never used.

## 5. The change

**A contract only the receiver has acknowledged.** A no-acknowledgement Pack names its contract by id, and the receiver resolves the id in its open contracts, dropping a Pack whose contract it does not hold ("drop nack contract mismatch"). Such a Pack may arrive before the Pack that introduces its contract, or without it (another route, loss), and nothing retries it. Carrying the contract frame on each no-acknowledgement Pack instead would cost two things:
- the frame, hundreds of bytes and more with certificate chains, on datagram-sized Packs;
- for a successor, a switch at the receiver out of sequence order.

So the introduction stays one acknowledged control Pack, the announcement or the opening head, and never acknowledged data.

**Three debit modes** (`updateContractForMode`):
- `sendContractUpdateAck` is today's acknowledged Pack.
- `sendContractUpdateNoAckBypass` is a no-acknowledgement Pack taken around recovery admission. It rides the acknowledged current contract, or switches to an acknowledged successor at the boundary, and otherwise returns to ordinary admission.
- `sendContractUpdateNoAckAdvance` is a no-acknowledgement Pack in ordinary admission:
  - it switches to an acknowledged successor;
  - while what it would ride is being introduced (the current contract's head, or the successor's announcement, unacknowledged), it holds;
  - otherwise it opens the next contract with its contract-only head, the way an acknowledged Pack does, without charging itself to it, and holds for that head's acknowledgement.

**Holding a Pack:**
- A held Pack is unwritten and uncharged.
- The scheduler passes over it (`noAckPackHeldAtContract`; `TakeFifoEligibleAround`). No-acknowledgement data is unordered, so what is behind it may go first, and ordered traffic is never reordered among itself.
- It is taken when the introduction is acknowledged, or once its caller's budget runs out. The deadline then drops it and counts it as `NoAckContractExpiredCount`.
- The loop wakes for the earliest held deadline (`earliestContractHeldDeadline`).
- The bypass predicate admits a Pack the acknowledged successor can carry (`aheadSendContractCanCarryNoAck`, the same checks `setAheadContract` applies).

**The opening is the one exception.** Before any contract of the sequence is acknowledged (`sendContractEverAcknowledged`), the Pack rides the opening head in order, as it always has. There is nothing the receiver holds for it to ride. Waiting a round trip there would delay every new sequence's first datagrams (DNS, QUIC Initial). It is counted as a promotion.

**Announcing on no-acknowledgement flows:**
- With nothing reliable outstanding, the announcement sends a recovering head under the current contract first, so the announcement is never the head.
- Above the threshold it arms the callers' wake at the threshold (`armSuccessorCheckAt`), so the fast-path reservation that crosses it brings the owner back.
- The owner announces on any pass that charged fast-path writes.
- A queue miss no longer spends the contract's one attempt. It is retried once the manager's `queuedContractGeneration` moves, costing one atomic read per write while it waits.

**Activating a no-acknowledgement switch.** After a no-acknowledgement Pack switches to the successor, one contract-only acknowledged Pack carries the successor's frame on the loop's next pass (`maybeSendContractActivation`). The receiver then makes it current and retires its predecessor where it does today, though no acknowledged data follows.
- A no-acknowledgement Pack never carries a due frame (`contractFrameDue := self.sendContractFrameDue && ack`).
- A failed announcement is withdrawn, so the contract can announce again or the boundary opens its successor.
- An announcement acknowledged after its successor became current makes the current contract acknowledged.

**The default.** `ContractFillFraction` is now 1, with its doc comment rewritten (transfer.go `SendBufferSettings`). It stays a setting.

**Counters** in `ClientSendRecoveryStatsSnapshot`:
- `NoAckContractAdvanceCount`: Packs that moved their sequence onto the next contract themselves;
- `NoAckContractHoldCount`: Packs that then waited for an introduction;
- `NoAckContractExpiredCount`: held Packs dropped at their deadline;
- `NoAckContractPromotedCount`: Packs carried on the acknowledged lane because their contract was unacknowledged. After the change, only openings.

## 6. Compatibility

- **Receivers.** Every receiver has admitted a full contract since 2024. The announcement stays gated on the receiver's advertised `contract_ahead`.
  - To a peer that never advertised it, a boundary opens the successor with its head and the Pack waits one round trip, where it used to be promoted.
  - The activation and the recovering head are contract-only acknowledged Packs of the shape a head has always had. A receiver's `setContract` takes them on its early path.
- **Server.** No change. Grants, the 2× ceiling and settlement are unaffected. Each contract's two parties now close it with the same count (row below), where an announced contract used to differ by one byte.

## 7. Effects

- **Per heavy flow.** Contracts carry a quarter more data each (1/0.8). In the fill row below, the same traffic needs 6 contracts at 1 and 7 at 0.8. The no-acknowledgement data no longer becomes reliable at boundaries: no resends and no head-of-line blocking for datagrams.
  - A hold happens only when the boundary arrives before the announcement's acknowledgement: within a round trip plus the receiver's acknowledgement compression, which the threshold (two delivered round trips, floor 256 KiB) is sized to cover.
  - To a legacy receiver, it happens at every boundary.
- **Fleet creations.** Mimir, one hour to about 10:00Z on 2026-11-05: 11,608 contract creations a second against 485 MB/s relayed, about 24,000 creations per relayed GB. Full contracts are 0.67% of creations, so the fill change removes roughly 0.1-0.2% of creations. It is not a CPU lever. That lever is the roughly 60% of contracts that carry nothing.
- **Escrow and Pending:**
  - The fifth of every full contract that was escrowed and never carried is now carried.
  - Escrow held per open contract is unchanged.
  - Retired announced contracts now close as soon as their data is acknowledged, instead of holding their escrow until the sequence ends or an hour passes.

## 8. Tests

**Root causes**, in transfer_noack_contract_advance_test.go. Each fails on origin/main (d256dbab and b8a746af2) and passes after:
- `TestANoAckPackAdvancesToTheAcknowledgedSuccessorItself`, before: "did not advance to the acknowledged successor: updated=false deferred=true".
- `TestANoAckPackAtAContractBoundaryNeedsNoRecoveryAdmission`, before: "still waits for recovery admission at the boundary".
- `TestANoAckOnlySequenceAnnouncesItsSuccessorAhead`, before: "never announced its successor".
- `TestASendContractFillsToItsSignedSize`, before: "838860 of 1048576 signed bytes may be sent (ContractFillFraction 0.80)".
- `TestANoAckOnlySequenceCrossesContractBoundariesWithoutPromotion`, before: "4 no-ack messages crossed the wire on the acknowledged lane". This row runs at the public send entry, between two real clients.
- `TestARetiredAnnouncedContractClosesWithItsCount`, before: "retired contract … is still open with 128353 acknowledged and 1 unacknowledged bytes".

**A pin that passes before and after:** `TestAReceiverAdmitsAContractFilledToItsSignedSize`.

**Behaviour**, in transfer_noack_contract_advance_behavior_test.go:
- `TestAFastPathReservationAtTheAnnounceThresholdWakesTheOwner`;
- `TestALostAnnouncementHoldsTheBoundaryPackUntilItsRetry`;
- `TestAHeldBoundaryPackExpiresUnchargedAtItsDeadline`;
- `TestMixedTrafficKeepsOrderAndAccountingAcrossContractBoundaries`;
- `TestTheOpeningIsTheOnlyPromotion`;
- `TestTheFillFractionDefaultCarriesTheSameTrafficInFewerContracts`.

The composed rows run under `testing/synctest`, with fake time and no network, so they are deterministic.

## 9. What ships it

The change is on the sender, in every `SendSequence`, and each side benefits on its own:
- **Apps** benefit for client-to-provider traffic, through the sdk's connect dependency and store releases. The builder stamps versions.
- **Providers** benefit for return traffic, the provider-to-client datagrams, through the nightly.
- **Server services** that run connect clients benefit through a server release that takes this connect: the proxy's hosted clients, and the connect service's own sequences.

Receivers need nothing.

## 10. Open

- **The counters are client-local.** The fleet sees them only where a service exports them. The proxy service (server proxy/window_client_metrics.go) is the natural place for a collector of `SendRecoveryStats().NoAckContract*`. That is a server change.
- **The opening still promotes**, by design (§5), and is counted.
- **Legacy receivers** hold a round trip at each boundary until they update.

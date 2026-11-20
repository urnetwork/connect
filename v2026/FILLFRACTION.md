# FILLFRACTION: the contract fill fraction, and no-acknowledgement Packs at a contract boundary

The owner, 2026-11-05, in order:

> The transfer will only fill contracts half full. So a 64mb ceiling will cap contract content at 32mb. Let's keep the current 128mb

> Perhaps it's the half fill logic in connect/transfer that needs revisiting. Perhaps we should look at something like .7 or .8 . The fill fraction should be a setting.

> the fill fraction is for non acked transfer, because it won't wait for a new contract

> Perhaps we should review the fill fraction generally. It seems wrong that non ack won't wait for a new contract or update the contract as usual

> we need to make sure that no-ack also advances new contracts

> actually i remember what it protected, and this should be documented. no ack messages may arrive out of order with new contracts. New contracts are sent with ack. So the receiver needs some spare capacity in the previous contract to wait to keep acceping no ack while waiting potentially for the next contract. Keep the fill at .9

> Add a deterministic root cause test case for this out of order no ack and contract, that shows why the partial fill helps resolve dropped receiver no ack. Also document the reason.

Line numbers are connect origin/main b8a746af2 unless a section says otherwise.

## 1. Verdict

- **The setting already exists.** `SendBufferSettings.ContractFillFraction` (transfer.go:990) was 0.8. It was 0.5 from its introduction (253f60cd, 2024-03-19) until ca6b07ec (2026-03-23), which is the half the first message remembers. It applies only to the sender: receivers build receive contracts with 1.0, so they admit the whole signed size.
- **What the headroom protects (§2).** No-acknowledgement Packs may arrive out of order with a new contract, and a new contract is introduced with an acknowledged Pack. A receiver drops a no-acknowledgement Pack that names a contract it has not been introduced to. During a switch, then, no-acknowledgement traffic needs a contract the receiver already holds, with room left: the previous contract's headroom, the share above the fill. The default is 0.9.
- **The defect (§3).** A no-acknowledgement Pack could not advance the contract itself, and nothing ever charged the headroom:
  - at a boundary the Pack was deferred, then promoted onto the acknowledged lane while a successor opened unacknowledged;
  - a sequence carrying only no-acknowledgement traffic never announced a successor ahead at all.
- **An adjacent defect turned up in the same code (§4).** A contract switched to from its announcement kept one unacknowledged byte for its whole life, so once retired it never closed, and the payer's escrow for it stayed held. An announced successor never used had the same byte at sequence close.
- **The change (§5):**
  - a no-acknowledgement Pack advances the contract itself, the way an acknowledged Pack does, and is never promoted to do it;
  - acknowledged traffic stops at the fill, 0.9 of the signed size;
  - while the next contract's acknowledged introduction is in flight, no-acknowledgement Packs ride the headroom of a contract the receiver holds, and wait at the boundary only once that headroom is used up;
  - the opening reservation is released;
  - counters make the fleet effect visible.

## 2. What the headroom protects

**The mechanism, in the owner's words:** "no ack messages may arrive out of order with new contracts. New contracts are sent with ack. So the receiver needs some spare capacity in the previous contract to wait to keep acceping no ack while waiting potentially for the next contract."

How the receiver works (connect origin/main):
- A no-acknowledgement Pack names its contract by id. The receiver resolves the id among its open contracts and drops the Pack if it does not hold that contract ("drop nack contract mismatch", `receiveNack`). Nothing retries a no-acknowledgement Pack.
- A new contract becomes known to the receiver only through an acknowledged Pack: the contract-only head that opens it, its announcement, or the first Pack carrying its frame. That Pack can be delayed or lost and resent, and no-acknowledgement Packs are not ordered against it, so they can overtake it.
- The receiver keeps a superseded contract open after the switch (`MaxOpenReceiveContract`, 4; receivers have kept this window since 2024), and it admits it up to its signed size.

So a no-acknowledgement Pack sent during a switch must name a contract the receiver already holds, and that contract must have room. If acknowledged traffic has used the whole predecessor, as it does at fill 1.0, there is no such contract until the successor is acknowledged. The traffic then either names the successor and is dropped at the receiver, or waits and is dropped at its deadline. With the fill at 0.9, the predecessor's last tenth carries it.

**The original comment said the same thing**, in its pre-2024 form: "as this ->1, there is more risk that noack messages will get dropped due to out of sync contracts". Then a no-acknowledgement Pack carried no contract id and was charged to whatever contract the receiver held current, so during a switch the predecessor absorbed the successor's datagrams. Since 172931e9 the Pack carries its contract id. The need is the same, and now the sender must choose the predecessor explicitly.

**Other candidates, checked and not what the headroom is for:**

| Candidate | Finding | Source |
|---|---|---|
| Fast-path writes charged after the fact | Callers reserve from `noAckRemainingByteCount`, shared with ordinary debits, so a reservation never passes the effective size. | transfer.go:9024-9026, 18415-18418, 8915-8962 |
| Minimum message rounding, sender against receiver | One byte on both sides, now and in 172931e9 and ca6b07ec. | transfer.go:955, 1094 |
| Duplicates or retransmits debited twice at the receiver | A Pack below the next sequence number is dropped before the debit. | transfer.go:17156-17185 |
| Framing or contract-frame bytes counted on one side only | Both sides count data frames only; a control Pack counts one minimum message on both. | `MessageByteCount` transfer.go:19135; receive 4955 |
| Server grant, escrow, settlement | The grant is `min(max(Min, request), 2 × standard)`; escrow is the grant; settlement compares both parties' counts. | server controller/connect_controller.go:52, 1323-1329; model/subscription_model.go:3603-3616 |

**Measured on Main, 2026-11-05.**
- **The 0.8 cap shows in the data.** In three minutes from 13:00Z, the contracts of at least 4 MiB that used at least 30% of their grant spike at 75-80% used (92 of 211).
- **Over 30 s from 13:30Z:** 142 of 21,228 contracts (0.67%) reached 75% of their grant. They carried 52% of all bytes, and 0.188 GiB of their 0.942 GiB grants was escrowed and never carried.

## 3. A no-acknowledgement Pack at a contract boundary, before the change

1. **It may not switch contracts.** It debits only the current contract, and only once acknowledged (10618-10620). It may not switch to the announced successor, even one the receiver has acknowledged (10636-10642). It is deferred to recovery admission (8346-8353, 10093-10096).
2. **Ordinary admission promotes it.** There it is debited with promotion allowed: it switches to an unacknowledged successor, or opens one with `createContract`. `prepareSendItem` then promotes it (`effectiveAck = ack || self.sendContract != nil && !self.sendContractAcked`, transfer_send_prepared_item.go:21): sequence-numbered, retained, resent and head-of-line, for a round trip at every boundary.
3. **A no-acknowledgement-only sequence never announces.** `maybeAnnounceContractAhead` returned whenever no reliable item was outstanding (11136-11138), which on a UDP-only flow is always.
4. **A fast-path flow never reached the announcement** (9683-9686).
5. **A no-acknowledgement Pack carried a frame that was due** (11619-11633), which can switch the receiver out of order.
6. **Nothing charged the headroom.** Every debit, acknowledged or not, stopped at the effective size, so the share above the fill went uncarried, including during a switch, when it is the only room no-acknowledgement traffic has.

## 4. The opening reservation

**The defect.** `announceContractAhead` charges the successor one minimum message when it is announced (11196-11202). No Pack under the successor carries it, and the receiver never counts it. Nothing released it when the successor became current, so the contract held one unacknowledged byte for its whole life. `tryCloseRetiredSendContract` closes a retired contract only when its unacknowledged count equals its abandoned count (transfer_no_ack_contract_lifetime.go:75), so the contract stayed open after retirement, and the payer's escrow for it stayed held until the sequence ended or the platform expired it. Each such contract also closed one byte apart from its receiver.

**The fix.** The reservation is released wherever the successor stops being only announced:
- `setAheadContract`, when the successor becomes current;
- `discardAheadContract`, for a successor that is withdrawn;
- the send sequence's exit, for a successor still announced and never used, so the two parties close it with the same count.

## 5. The change

**The fill, 0.9.** `SendBufferSettings.ContractFillFraction` stays a setting with its default in `DefaultSendBufferSettings`. Acknowledged and ordinary traffic stops at `transferByteCount * ContractFillFraction`. The share above it is headroom only no-acknowledgement Packs charge, tracked apart from ordinary debits (`sequenceContract.noAckHeadroomByteCount`). Ordinary and fast-path debits stay within the effective size and headroom debits within the headroom, so together they never pass the signed size.

**The headroom carrier** (`SendSequence.noAckHeadroomCarrier`) is one of:
- the acknowledged predecessor, while the current contract (opened, or switched to, by acknowledged traffic) is still unacknowledged. `setContract` keeps such a predecessor open (`noAckHeadroomContract`). Its fast path is retired at the switch exactly as before, so a caller holding the old snapshot is refused and returns to queue admission, and only the sequence loop charges the headroom. Its close waits until the headroom has done its part;
- the acknowledged current contract, past its fill, while its announced successor is still unacknowledged.

Either way the receiver holds the contract. A no-acknowledgement Pack charged to the headroom names that contract (`noAckHeadroomCharge`, read where the Pack is serialized) and is never promoted. A rollback returns headroom, never fast-path budget. The predecessor's headroom ends (`endNoAckHeadroom`) once the successor is acknowledged, when a later switch replaces it, or when the path changes. Until then `tryCloseRetiredSendContract` defers its close; after it, the predecessor closes exactly once, as soon as nothing it carries is outstanding.

**A no-acknowledgement Pack at a boundary, now** (`updateContractForMode`):
1. the acknowledged current contract carries it within its fill;
2. otherwise it switches to an acknowledged successor, if one can carry it;
3. otherwise, while the next contract's introduction is in flight, it rides the carrier's headroom;
4. otherwise it holds while what it would ride is being introduced. If nothing is being introduced, it opens the next contract with its contract-only head, the way an acknowledged Pack does, then rides the predecessor's headroom, or holds if that is used up.

A held Pack is unwritten and uncharged. The scheduler passes over it (`noAckPackHeldAtContract`) and takes it once the introduction is acknowledged, or once its caller's budget runs out, when the deadline drops it (`NoAckContractExpiredCount`). The bypass predicate (`noAckPackCanBypassRecoveryAdmission`) admits a Pack the headroom or an acknowledged successor can carry.

**The opening is the one promotion.** Before any contract of the sequence is acknowledged (`sendContractEverAcknowledged`), the Pack rides the opening head in order, as it always has, so that new sequences' first datagrams (DNS, QUIC Initial) do not wait a round trip.

**Announcing on no-acknowledgement flows.** With nothing reliable outstanding, the announcement sends a recovering head under the current contract first, so the announcement is never the head. Fast-path writes wake the owner at the announcement threshold (`armSuccessorCheckAt`). A queue miss no longer spends the contract's one attempt.

**Activating a no-acknowledgement switch.** After a no-acknowledgement Pack switches to an acknowledged successor, one contract-only acknowledged Pack carries the successor's frame (`maybeSendContractActivation`), so the receiver makes it current and retires its predecessor where it does today. A no-acknowledgement Pack never carries a due frame. A failed announcement is withdrawn, so the contract can announce again.

**Counters** in `ClientSendRecoveryStatsSnapshot`:
- `NoAckContractAdvanceCount`: Packs that moved their sequence onto the next contract themselves;
- `NoAckContractHeadroomCount`: Packs that rode a contract's headroom while the next contract was introduced;
- `NoAckContractHoldCount`: Packs that waited for an introduction, once no headroom was left;
- `NoAckContractExpiredCount`: held Packs dropped at their deadline;
- `NoAckContractPromotedCount`: Packs carried on the acknowledged lane because their contract was unacknowledged. After the change, only openings.

## 6. Compatibility

- **Receivers.** No receiver change is needed. Every receiver has admitted a contract to its signed size, and kept a superseded contract open (`MaxOpenReceiveContract`), since 2024. That is what the headroom rides. The announcement stays gated on the receiver's advertised `contract_ahead`. To a peer that never advertised it, a boundary opens the successor with its head, and no-acknowledgement traffic rides the predecessor's headroom until the head is acknowledged.
- **Server.** No change. Grants, the 2× ceiling and settlement are unaffected. Each contract's two parties close it with the same count, including the predecessor that carried headroom traffic and a successor announced but never used.

## 7. Effects

- **Per heavy flow.** Contracts carry about an eighth more data each (0.9 against 0.8), and up to the whole signed size when a switch's headroom is used. No-acknowledgement data no longer becomes reliable at boundaries: no resends and no head-of-line blocking for datagrams. It waits at a boundary only when the next contract's introduction takes longer than the headroom lasts at the flow's rate.
- **Fleet creations.** Full contracts are 0.67% of creations, so the fill change removes a small share of creations. It is not a CPU lever; that lever is the roughly 60% of contracts that carry nothing.
- **Escrow and Pending.** Retired announced contracts now close as soon as their data is acknowledged, instead of holding their escrow until the sequence ends or an hour passes.

## 8. Tests

**The owner's case**, in transfer_noack_contract_headroom_test.go:
- `TestAReceiverDropsANoAckThatOvertakesItsContractUnlessThePredecessorHasHeadroom`, on the receiver's real no-acknowledgement path. A Pack that overtakes its contract's introduction is dropped. A Pack on the predecessor is dropped at fill 1.0 and admitted at 0.9. A late Pack on the predecessor, after the introduction, is admitted at 0.9. Result: "fill 1.0: 3 of 3 no-ack Packs sent during the switch were dropped by the receiver; fill 0.9: 1 of 3". The one at 0.9 is the Pack that names the unintroduced successor, which no sender sends. The receiver is unchanged, so this row passes before and after; it pins the drop the headroom avoids.
- `TestNoAckRidesThePredecessorsHeadroomThroughADelayedContractIntroduction`, end to end between two real clients. Every introduction after the opening is held fifteen seconds in the network, so no-acknowledgement Packs overtake it, and a burst of 20 datagrams with 300 ms budgets follows the switch. Result: "fill 1.0: 0 of 20 burst datagrams reached the receiver (20 held at the boundary, 20 dropped at their deadline); fill 0.9: 20 of 20 reached it (0 held, 0 dropped), 20 named the predecessor". On the tree before this change (9e752de00) the same row fails at 0.9: "0 of 20 reached it (20 held, 20 dropped)".
- `TestANoAckPackHoldsOnlyWhenTheHeadroomIsExhaustedBeforeTheSuccessorIsAcknowledged`: the predecessor's headroom carries every Pack until it is used up, then a Pack holds, and once the successor is acknowledged traffic rides it and the predecessor is retired.

**Root causes**, in transfer_noack_contract_advance_test.go. Each fails on origin/main (d256dbab and b8a746af2) and passes after:
- `TestANoAckPackAdvancesToTheAcknowledgedSuccessorItself`, before: "did not advance to the acknowledged successor: updated=false deferred=true".
- `TestANoAckPackAtAContractBoundaryNeedsNoRecoveryAdmission`, before: "still waits for recovery admission at the boundary".
- `TestANoAckOnlySequenceAnnouncesItsSuccessorAhead`, before: "never announced its successor".
- `TestANoAckOnlySequenceCrossesContractBoundariesWithoutPromotion`, before: "4 no-ack messages crossed the wire on the acknowledged lane".
- `TestARetiredAnnouncedContractClosesWithItsCount`, before: "retired contract … is still open with 128353 acknowledged and 1 unacknowledged bytes".

**Pins:**
- `TestAcknowledgedTrafficStopsAtTheFillAndTheHeadroomCarriesNoAckToTheSignedSize`: ordinary traffic stops at 0.9; the headroom carries no-acknowledgement traffic to the signed size and no further; a rollback returns headroom.
- `TestAReceiverAdmitsAContractFilledToItsSignedSize`.
- `TestH1NoAckContractRetirementWitness` (existing) keeps every fast-path assertion: a stale reader is refused at the switch, accepted writes finish, and the close happens exactly once with the right bytes. For a predecessor that carries headroom, it now acknowledges the successor before asserting the close.
- `TestNativeBatchNoAckGroupRetiredContractRefuses` (existing, unchanged): a retired snapshot never consumes a new group.

**Behaviour**, in transfer_noack_contract_advance_behavior_test.go:
- `TestAFastPathReservationAtTheAnnounceThresholdWakesTheOwner`;
- `TestALostAnnouncementHoldsTheBoundaryPackUntilItsRetry`;
- `TestAHeldBoundaryPackExpiresUnchargedAtItsDeadline`;
- `TestMixedTrafficKeepsOrderAndAccountingAcrossContractBoundaries`. At 0.9 this row first exposed the announced successor left unused at sequence close, which closed one byte apart from its receiver (§4);
- `TestTheOpeningIsTheOnlyPromotion`;
- `TestTheDefaultFillLetsNoAckTrafficCarryContractsPastTheFillToTheirSignedSize`.

The composed rows run under `testing/synctest`, with fake time and no network, so they are deterministic.

## 9. What ships it

The change is on the sender, in every `SendSequence`, and each side benefits on its own:
- **Apps** benefit for client-to-provider traffic, through the sdk's connect dependency and store releases. The builder stamps versions.
- **Providers** benefit for return traffic, through the nightly.
- **Server services** that run connect clients benefit through a server release that takes this connect: the proxy's hosted clients, MCP, and the connect service's own sequences.

Receivers need nothing.

## 10. Open

- **The counters are client-local.** The fleet sees them only where a service exports them. The proxy service (server proxy/window_client_metrics.go) is the natural place for a collector of `SendRecoveryStats().NoAckContract*`. That is a server change.
- **The opening still promotes**, by design (§5), and is counted.
- **The headroom is a fixed share.** A flow whose rate times the introduction's delay exceeds a tenth of its contract still waits at the boundary once the headroom is used up. That is counted as a hold.

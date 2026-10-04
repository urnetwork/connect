// pq_secret PER EPOCH, ledger item 251's ruling 40.
//
// WHAT WAS WRONG, AND IT IS ONE LINE. Every storage root in this system is
// StorageRoot(mls_secret[n], pq_secret) -- the MLS exporter of epoch n mixed with the only post
// quantum material the design has. The mls_secret half was already per epoch and had to be:
// pastepoch.go rebuilds a prior epoch's whole group to export it. The pq_secret half was a SCALAR
// on the session, so pastepoch.go's `root := StorageRoot(mlsSecret, self.pqSecret)` re-derived a
// PAST epoch's root out of TODAY's secret. While every group is handed the same octets forever
// that is invisible; the day a commit rotates the secret, every epoch below it becomes unopenable
// at the AEAD tag, with nothing anywhere saying why. Reproduced before it was fixed, with the
// unrotated session as the control: pqepoch_test.go's first case.
//
// WHY THE SCALAR WAS THERE AT ALL is item 243: pq_secret was RULED a group lifetime value in
// 2026-09-18, on the explicit condition that rotating it is a prerequisite of shipping REMOVAL --
// because a lifetime value leaves a removed member a permanent contribution to every future
// epoch's storage root. Ruling 40 is that condition coming due. This file is the table; the
// carrier that delivers a rotated secret to the other members is the device wrap, and it is NOT
// here.
//
// WHAT IS HELD, FOR HOW LONG, AND WHAT ERASES IT, because that is the sentence the erase
// discipline asks of every holder in this package.
//
//   - ONE ENTRY PER EPOCH THIS SESSION HAS STOOD AT, keyed by epoch, each of them thirty two
//     octets this session copied out of what its caller handed AdvanceEpoch or the constructor.
//   - BOUNDED BY PastEpochWindow, which is the same bound pastepoch.go refuses below and the same
//     bound connect/mls deletes state below. An entry more than PastEpochWindow behind the
//     session's epoch can serve no open that pastEpochOnLoop would admit, so it is dropped AND
//     ERASED as the window moves past it -- not merely dropped, because these are the post
//     quantum half of a retired epoch's storage root and a map entry nobody deleted is a live
//     secret with no owner.
//   - AND THE WHOLE TABLE GOES AT Close, erased entry by entry.
//
// It is NOT dropped at an epoch install, which is the one place this table's discipline differs
// from every other field installEpochOnLoop touches. The ratchets, the class keys and the prior
// epochs' schedules are re-derived or rebuilt on demand from the epoch the session moved to; a
// past epoch's pq_secret is neither derivable nor rebuildable from anything -- it is a value that
// ARRIVED -- so dropping it at the install would destroy the only copy of the thing that makes a
// past epoch's root computable, which is precisely what this file exists to keep.
//
// THE COMPATIBILITY PATH IS A PREMISE AND NOT A MEASUREMENT, which is what the field it hangs off
// is named for. Every group that exists today was handed one pq_secret at its creation and the
// same octets at every AdvanceEpoch since, and a device that restarts constructs its session AT
// WHATEVER EPOCH THE GROUP HAS REACHED -- so its table holds exactly one epoch, and every prior
// epoch inside the window would be a miss. Before this file the scalar answered all of them. So a
// session BEGINS HOLDING THE PREMISE that pq_secret is a group lifetime value, and while it holds
// the premise the one secret it has answers every epoch, which is the old behaviour exactly.
//
// THE PREMISE IS DEFINED ON EXACTLY THE SET THIS SESSION HAS NO EVIDENCE ABOUT, and that is worth
// stating as a set rather than as a reassurance. Every epoch this session STOOD AT is in the
// table -- the constructor files the epoch it opens at, AdvanceEpoch files each one it opens --
// so the first arm answers all of those out of octets this session was actually handed. The
// premise is reached only for epochs BELOW the one this session was constructed at: precisely the
// epochs it was not present for and can vouch for nothing about. It is sound because of a fact
// about the world -- nothing rotates pq_secret yet -- and not because of anything a session sees.
//
// A SECOND, DIFFERENT SECRET REFUTES THE PREMISE, AND ONLY WITHIN ONE SESSION'S LIFETIME. When a
// value arrives that differs from the one standing at this session's own epoch, OR that differs
// from the entry it is about to replace, "the secret I have" and "the secret that epoch had" have
// been OBSERVED to be different values, the premise is dropped, and answering with the first would
// be the defect above wearing another hat. From then on a miss refuses with
// ErrPqSecretUnknownEpoch and names the epoch, because the caller's only repair is to supply that
// epoch's secret. The second of those two comparisons was MISSING and its absence was the defect
// installPqSecretOnLoop's own doc now carries: the install read the entry it was about to erase
// and never compared it, so an advance destroyed a wrap's pq_secret[n+1] in silence.
//
// AND ONE PATH MAY NOT REPLACE AT ALL. Refuting is what a session does with a difference it can
// still act on; an advance that would OVERWRITE a differing entry has already lost the value, so
// that one is a typed refusal -- ErrPqSecretEpochConflict, raised by refusePqSecretConflictOnLoop
// before anything is touched -- and the deliberate supersede stays with InstallPqSecret, the door
// whose subject is filing.
//
// AND HERE IS THE RESIDUAL, NAMED RATHER THAN LEFT TO BE FOUND, because the first draft of this
// file claimed the opposite in as many words -- "the fallback is gone for good" -- and a reader
// would have built on it. ROTATION IS A PROPERTY OF THE GROUP AND IT IS DURABLE; the refutation
// above is a property of ONE PROCESS and it is not. A device that restarts after its group has
// rotated constructs a session holding today's secret, an empty history and the premise INTACT,
// so it answers a past epoch with today's secret -- ruling 40's defect exactly, surfacing as an
// AEAD failure with no diagnosis. Nothing the constructor is handed carries the fact: sdk's
// durable GroupRecord holds ONE pq_secret scalar -- the scalar ruling 40 says goes with this one
// -- and restore.go hands that one value in. pqrestart_test.go MEASURES this shape in both
// directions rather than describing it, and its first case is written to go RED the day the fact
// does reach a fresh session, so this paragraph cannot rot while the code moves under it.
//
// SO THE FACT IS AN INPUT, AND IT HAS TWO DOORS. InstallPqSecret files a secret at an epoch this
// session IS NOT STANDING AT -- a past one, or, under ruling 37, the next one, which is what the
// device wrap of item 243's next step delivers; its own epoch is refused with
// ErrPqSecretEpochIsCurrent because this door files without re-deriving and the table and the live
// key schedule may not disagree about self.epoch -- and which refutes the premise by the same
// octet comparison AdvanceEpoch's install uses -- and
// DeclarePqSecretRotated states the bare fact, for a restorer holding no past secret to file.
// Neither has a production caller in THIS repository: the caller is sdk's restore path, sdk is
// the other half of item 243's step 4, and sdk is where GroupRecord.PqSecret has to stop being a
// scalar. That is the residual, and it is a residual with a seam rather than a design to reopen.
//
// THE ZERO VALUE OF THE PREMISE IS THE SAFE DIRECTION, which is why the field is spelled
// positively. A GroupSession built by a struct literal that forgot it refuses the past epochs it
// holds no secret for; the same field spelled "has rotated" would default to answering them. A
// lost history is a refusal that says what it is; a root derived from the wrong secret is a
// working messenger that opens nothing and explains nothing.
//
// THE PREMISE IS DROPPED ON THE OCTETS AND NOT ON A COUNT. "Has a rotation been observed" is
// decided by comparing what arrives against what is already held, in constant time, so a caller
// that hands the same secret in a hundred times still holds the premise, and one that hands a
// different secret in once has dropped it for the rest of the session. A count of AdvanceEpoch
// calls would have made every group in the world rotated at its second commit and taken the
// compatibility path away from all of them.
package messagegroup

import (
	"crypto/subtle"
	"fmt"
)

// installPqSecretOnLoop records pq_secret[epoch] and decides whether the premise still stands.
//
// The value is COPIED. The caller's array is the caller's: AdvanceEpoch's own parameter is passed
// straight through from sdk, which holds it for its own reasons, and a table aliasing it would
// erase a caller's buffer when the window moved.
//
// An entry already standing at that epoch is ERASED before it is replaced. It happens when a
// caller advances into an epoch twice -- a retried install -- and when InstallPqSecret supersedes
// a secret this device filed on a commit that lost its CAS race. The octets underneath are a
// storage root's post quantum half however briefly they were wrong.
//
// THERE ARE TWO COMPARISONS HERE AND THEY ARE DIFFERENT QUESTIONS, which is the defect this
// function was repaired for: it compared ONLY against the entry at the epoch being LEFT, and never
// against the entry it was about to ERASE. So a pq_secret[n+1] filed by a wrap -- ruling 37's own
// advertised sequence, install then advance -- was destroyed by the next AdvanceEpoch without a
// comparison, an error or a refutation, and both of sdk's production call sites pass the group
// lifetime scalar today. Reproduced before it was repaired, with the two candidate class-key sets
// for the new epoch asserted unequal first: the table held the scalar, the live schedule was the
// scalar's, and AdvanceEpoch answered nil.
//
//   - AGAINST THE ENTRY BEING REPLACED: two different values have claimed to be pq_secret[epoch].
//     One of them is wrong. That is a rotation OBSERVED as directly as this session can observe
//     one, so it refutes; and the path that did not ask to replace anything refuses before it
//     ever reaches here, through refusePqSecretConflictOnLoop.
//   - AGAINST THE ENTRY AT THIS SESSION'S OWN EPOCH: the secret this session is running on differs
//     from one arriving for another epoch, so the group's secret is not one value for its
//     lifetime. It is the observation the ordinary advance makes and it is kept as the separate
//     thing it is -- the two fire on different states, and a session filing its FIRST value above
//     its own epoch reaches only this one.
//
// The caller is the loop goroutine, or the constructor before the loop exists.
//
// The noinline directive is this package's erase helper class: the store above erases an entry of
// the receiver's own table, and that store outlives this call.
//
//go:noinline
func (self *GroupSession) installPqSecretOnLoop(epoch uint64, pqSecret []byte) {
	// both refutations are decided BEFORE the table is touched, because the entry the second one
	// reads is the entry the erase below blanks. subtle.ConstantTimeCompare and not bytes.Equal:
	// guardrail G8 sends every comparison of secret octets in this tree through it.
	//
	// AN EMPTY TABLE REFUTES NOTHING, and the constructor is the one caller that meets it: it
	// files into an empty map, so neither branch can fire there. That is not an oversight, it is
	// the residual the file header names -- a fresh session has no history to compare against,
	// which is exactly why rotation has to be TOLD to one rather than inferred by it.
	if self.pqLifetime {
		if standing, isHeld := self.pqSecrets[self.epoch]; isHeld {
			if subtle.ConstantTimeCompare(standing, pqSecret) != 1 {
				self.pqLifetime = false
			}
		}
		if superseded, isHeld := self.pqSecrets[epoch]; isHeld {
			if subtle.ConstantTimeCompare(superseded, pqSecret) != 1 {
				self.pqLifetime = false
			}
		}
	}
	if held, isHeld := self.pqSecrets[epoch]; isHeld {
		zeroize(held)
	}
	self.pqSecrets[epoch] = append([]byte(nil), pqSecret...)
}

// refusePqSecretConflictOnLoop refuses a value that would destroy a DIFFERENT secret already filed
// for the same epoch, and answers nil when there is nothing there or the octets agree.
//
// IT READS THE TABLE AND WRITES NOTHING, which is the whole of its contract: it is a predicate
// this file owns because the table, its bound and its premise are this file's subject, in the same
// way dropPqSecretsBelowWindowOnLoop is called from the epoch install and lives here. A reader
// looking for what may overwrite pq_secret[n] finds the rule beside the writer it constrains.
//
// ITS ONE CALLER IS AdvanceEpoch, and that is a judgement rather than an accident. Under ruling 37
// the wrap carrying pq_secret[n+1] is opened at epoch n, before the merge, so the ordinary step-4
// sequence leaves the authority for epoch n+1 already in the table when the advance runs;
// AdvanceEpoch's parameter is the caller's own account of the same value and it did not ask to
// replace anything. InstallPqSecret is the door whose subject IS filing, it erases what it
// supersedes, and it drops the premise on the difference -- so the deliberate replace has a way
// through and the incidental one does not.
//
// FOR EVERY GROUP ALIVE TODAY THIS CHANGES NOTHING, and that is measured rather than hoped: the
// only entry an advance can meet at the epoch it is entering is one a retry of that same advance
// filed, carrying the same octets, and ErrPqSecretEpochConflict compares octets rather than
// counting calls. A retry with a DIFFERENT secret is refused, and it is refused because two
// different values for one epoch is the state this refusal exists to name.
//
// The caller is the loop goroutine.
func (self *GroupSession) refusePqSecretConflictOnLoop(epoch uint64, pqSecret []byte) error {
	standing, isHeld := self.pqSecrets[epoch]
	if !isHeld {
		return nil
	}
	if subtle.ConstantTimeCompare(standing, pqSecret) == 1 {
		return nil
	}
	return fmt.Errorf("%w: epoch %d already holds a pq_secret this session was handed, and the value offered here is a different one; nothing was filed and nothing was erased. Under ledger item 251's ruling 37 the entry already there is the one the epoch's device wrap delivered -- advance with THAT secret, or supersede it deliberately through InstallPqSecret first",
		ErrPqSecretEpochConflict, epoch)
}

// pqSecretForOnLoop answers pq_secret[epoch], or refuses.
//
// THE ORDER OF THE TWO ARMS IS THE WHOLE OF THE RULE. The table first, always, so that a session
// which has seen a rotation never reaches the premise for an epoch it actually holds; then the
// compatibility path, which answers only while the premise stands. A session with nothing at all
// in its table falls through both and refuses, which is the closed session's shape and is what
// Close leaves behind.
//
// The refusal names THE SHAPE OF THE TABLE and never its octets -- a table of secrets is not a
// thing to print into an error string that travels to a log -- because what a caller debugging a
// miss needs is whether the epoch is one this device ever stood at and whether the premise was
// still standing to answer it.
//
// The caller is the loop goroutine.
func (self *GroupSession) pqSecretForOnLoop(epoch uint64) ([]byte, error) {
	if secret, isHeld := self.pqSecrets[epoch]; isHeld {
		return secret, nil
	}
	if self.pqLifetime {
		if secret, isHeld := self.pqSecrets[self.epoch]; isHeld {
			return secret, nil
		}
	}
	oldest := self.epoch
	for held := range self.pqSecrets {
		if held < oldest {
			oldest = held
		}
	}
	return nil, fmt.Errorf("%w: epoch %d; this session stands at epoch %d, holds %d pq_secret(s) from epoch %d up, and still holds the group-lifetime premise: %t",
		ErrPqSecretUnknownEpoch, epoch, self.epoch, len(self.pqSecrets), oldest, self.pqLifetime)
}

// InstallPqSecret files pq_secret[epoch] for an epoch this session did not stand at.
//
// IT IS THE OTHER HALF OF InstallEphRoot AND IT WAS MISSING. A device wrap carries two values
// that a session cannot derive and that have to ARRIVE -- eph_root[k] and pq_secret[k] -- and
// this package had a door for one of them. m1 Task 14 defines honouring a wrap as INSTALLING
// what it carries rather than reading it, so both halves need a door or half of a wrap has
// nowhere to land. This is that door, and it is the one a restorer of a rotated group uses to
// hand back the past epochs it persisted.
//
// IT REFUTES THE PREMISE BY THE SAME COMPARISON AdvanceEpoch's INSTALL USES, and that matters
// more than the filing: a restorer that files even ONE past secret differing from today's has
// thereby told this session that its group rotates, and the compatibility path is gone without
// anybody having to say so separately. A restorer with nothing to file says it with
// DeclarePqSecretRotated instead.
//
// THE WINDOW IS REFUSED HERE AND NOT SWALLOWED. An entry more than PastEpochWindow behind this
// session's epoch can serve no open pastEpochOnLoop would admit -- that function refuses the
// epoch before it ever asks for a secret -- so filing one would hold a retired epoch's post
// quantum half until the next advance erased it, for nothing, and would tell the caller its
// history was recovered when it was not. The refusal is ErrEpochOutOfWindow, the same sentinel
// and the same arithmetic pastEpochOnLoop refuses with, because a caller meeting two different
// errors for one bound would have to derive which door it came through. An epoch ABOVE the
// session's own is accepted: ruling 37 has the wraps for epoch n+1 submitted at epoch n, so the
// secret of an epoch this session has not yet entered is a value that legitimately arrives early.
//
// AND THE SESSION'S OWN EPOCH IS REFUSED, which this door's first draft accepted while the
// paragraph above it said "an epoch this session did not stand at". It was not a documentation
// slip: this door files and does not re-derive, so filing at self.epoch left the table and the
// live key schedule holding two different answers about ONE epoch -- the session kept sealing
// under the root installEpochOnLoop had already extracted, and one advance later pastEpochOnLoop
// rebuilt that epoch out of the TABLE and every record of it stopped opening at the AEAD tag. That
// is ruling 40's own defect, an epoch's root built from a secret that epoch did not run on,
// arriving through the door added to close it. Reproduced before it was repaired, with both
// candidate class-key sets asserted unequal first. ErrPqSecretEpochIsCurrent carries why, and the
// contrast that makes it a defect rather than a choice: AdvanceEpoch files AND re-derives, this
// door files and does not, and nothing said so.
//
// The noinline directive is this package's erase helper class, reached through the install: that
// install erases the entry it replaces, and the store is the receiver's own table.
//
//go:noinline
func (self *GroupSession) InstallPqSecret(epoch uint64, pqSecret []byte) error {
	var err error
	if postErr := self.do(func() {
		if self.closing {
			err = ErrSessionClosed
			return
		}
		if len(pqSecret) == 0 {
			err = fmt.Errorf("%w: an install of nothing would file thirty two zeros under epoch %d and make them that epoch's post quantum half",
				ErrNilPqSecret, epoch)
			return
		}
		if len(pqSecret) != PqSecretBytes {
			err = fmt.Errorf("%w: %d octets, and it is the ikm of the storage_root of epoch %d",
				ErrPqSecretLength, len(pqSecret), epoch)
			return
		}
		if epoch == self.epoch {
			err = fmt.Errorf("%w: epoch %d; this door files and does not re-derive, so a value filed here would leave pq_secret[%d] and this session's live storage_root disagreeing about one epoch, and the next advance would rebuild that epoch out of the table. Ruling 37 has a wrap deliver epoch %d's secret, and a session whose own epoch ran on a different secret has to be built with it",
				ErrPqSecretEpochIsCurrent, epoch, epoch, epoch+1)
			return
		}
		if epoch < self.epoch && self.epoch-epoch > PastEpochWindow {
			err = fmt.Errorf("%w: epoch %d is %d epochs behind this session's %d, and the window is %d; no open at that epoch would be admitted, so nothing was filed",
				ErrEpochOutOfWindow, epoch, self.epoch-epoch, self.epoch, PastEpochWindow)
			return
		}
		self.installPqSecretOnLoop(epoch, pqSecret)
	}); postErr != nil {
		return postErr
	}
	return err
}

// DeclarePqSecretRotated states the one fact about a group that no session can observe.
//
// WHY A DOOR FOR A BOOLEAN. Rotation is durable and belongs to the GROUP; the refutation
// installPqSecretOnLoop performs belongs to one PROCESS and dies with it. A device that restarts
// after its group has rotated comes back holding today's secret and an empty history, so it can
// observe nothing, and the compatibility path -- correct for every group alive today -- would
// hand it today's secret for a past epoch. This is how its restorer tells it otherwise. The file
// header carries the whole account; pqrestart_test.go measures both branches.
//
// IT IS ONE WAY AND THERE IS NO UNDO, which is the direction that cannot lose anything: a session
// told that its group rotates refuses the epochs it holds no secret for, and the repair is to
// file them with InstallPqSecret. A door back to the premise would be a door to re-enabling a
// derivation the caller has already said is wrong.
//
// Calling it on a group that never rotated is not an error and costs exactly the epochs this
// session did not stand at: they stop opening and start refusing by name instead of opening
// correctly. That is a real cost and it is why this is a declaration rather than a default.
func (self *GroupSession) DeclarePqSecretRotated() error {
	var err error
	if postErr := self.do(func() {
		if self.closing {
			err = ErrSessionClosed
			return
		}
		self.pqLifetime = false
	}); postErr != nil {
		return postErr
	}
	return err
}

// dropPqSecretsBelowWindowOnLoop erases and drops every entry the window has moved past.
//
// THE BOUND IS pastEpochOnLoop's OWN, spelled the same way round: that function refuses an epoch
// when `self.epoch-epoch > PastEpochWindow`, so an entry satisfying the same comparison can serve
// no open this session would admit, and holding it is holding a retired epoch's post quantum
// secret for nothing. The subtraction is guarded by `epoch < self.epoch` because these are
// uint64s: an entry at or above the session's epoch underflows the comparison into a number that
// is always past the window, and the entry that would drop is the CURRENT epoch's.
//
// The caller is the loop goroutine.
//
// The noinline directive is this package's erase helper class: the erase below lands in the
// receiver's own table and outlives this call.
//
//go:noinline
func (self *GroupSession) dropPqSecretsBelowWindowOnLoop() {
	for epoch, secret := range self.pqSecrets {
		if epoch < self.epoch && self.epoch-epoch > PastEpochWindow {
			zeroize(secret)
			delete(self.pqSecrets, epoch)
		}
	}
}

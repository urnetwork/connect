package connect

import (
	"iter"
	"slices"
	"sort"
	"time"
)

// Beside each group of the affinity index (affinityIp4Paths, and the v6
// twin) the multi-client keeps a log of the group's entry writes in join time
// order, so the donor scan of a group reads the members young enough to donate
// from the log's newest end instead of iterating the whole group. A busy group
// holds every flow within its idle timeout, minutes of them, while only the
// last AffinityMaxTime of them can donate (inheritAffinityGroupClient4WithLock).
//
// A record is an entry's tuple and join time, written by the same canonical
// writers as the entry (joinAffinityIp4WithLock), and dropped by a trim once it
// is AffinityMaxTime old at a later write. A record no longer names its entry
// when the tuple was written again with a later time or left the group; the
// scan passes over it. One tuple and join time is recorded once.
//
// The log answers for its group only when it holds a record of every entry
// younger than AffinityMaxTime at the scan (covers): its count of the entries
// the writers added equals the group's size, and no record a trim dropped
// would be young at the scan's time and AffinityMaxTime (the setting can grow
// at runtime). Otherwise the scan reads the group as before. Either way the
// scan reads the same members: every entry younger than AffinityMaxTime and
// every stale entry (affinityDonorMembers).
//
// Guarded by the multi-client's stateLock, as the index is.
type affinityJoinLog[P comparable] struct {
	// in join time order; the records before headIndex are trimmed
	joins     []affinityJoin[P]
	headIndex int
	// the group's entries the canonical writers added and did not remove
	entryCount int
	// the latest join time a trim dropped; zero until a trim drops a record
	trimmedJoinTime time.Time
}

type affinityJoin[P comparable] struct {
	path     P
	joinTime time.Time
}

// Records a write of path's entry at joinTime, then drops the records that are
// maxTime old at joinTime. entryAdded is whether the write added the entry.
// Join times arrive in order from time.Now; an earlier one (the rebuild's, a
// fixture's) is inserted in its place.
func (self *affinityJoinLog[P]) record(path P, joinTime time.Time, entryAdded bool, maxTime time.Duration) {
	if entryAdded {
		self.entryCount += 1
	}
	insertIndex := len(self.joins)
	if self.headIndex < insertIndex && joinTime.Before(self.joins[insertIndex-1].joinTime) {
		insertIndex = self.headIndex + sort.Search(insertIndex-self.headIndex, func(i int) bool {
			return joinTime.Before(self.joins[self.headIndex+i].joinTime)
		})
	}
	recorded := false
	for i := insertIndex - 1; self.headIndex <= i && self.joins[i].joinTime.Equal(joinTime); i -= 1 {
		if self.joins[i].path == path {
			recorded = true
			break
		}
	}
	if !recorded {
		self.joins = slices.Insert(self.joins, insertIndex, affinityJoin[P]{path: path, joinTime: joinTime})
	}
	self.trim(joinTime, maxTime)
}

// Drops the records that are maxTime old at now, and moves the rest to the
// front of the array once half of it is dropped, into a smaller array when
// the rest fills less than a quarter of it.
func (self *affinityJoinLog[P]) trim(now time.Time, maxTime time.Duration) {
	for self.headIndex < len(self.joins) && maxTime <= now.Sub(self.joins[self.headIndex].joinTime) {
		// an earlier join time inserted after a trim can be dropped later
		if joinTime := self.joins[self.headIndex].joinTime; self.trimmedJoinTime.Before(joinTime) {
			self.trimmedJoinTime = joinTime
		}
		self.joins[self.headIndex] = affinityJoin[P]{}
		self.headIndex += 1
	}
	if self.headIndex == 0 || 2*self.headIndex < len(self.joins) {
		return
	}
	liveCount := len(self.joins) - self.headIndex
	switch {
	case liveCount == 0:
		self.joins = nil
	case 4*liveCount < cap(self.joins):
		self.joins = slices.Clone(self.joins[self.headIndex:])
	default:
		copy(self.joins, self.joins[self.headIndex:])
		clear(self.joins[liveCount:])
		self.joins = self.joins[:liveCount]
	}
	self.headIndex = 0
}

// Whether the log holds a record of every entry of the group (paths) that is
// younger than maxTime at now.
func (self *affinityJoinLog[P]) covers(paths map[P]time.Time, now time.Time, maxTime time.Duration) bool {
	if self == nil || len(paths) != self.entryCount {
		return false
	}
	return self.trimmedJoinTime.IsZero() || maxTime <= now.Sub(self.trimmedJoinTime)
}

// The records the log holds, trimmed or not yet.
func (self *affinityJoinLog[P]) recordCount() int {
	if self == nil {
		return 0
	}
	return len(self.joins) - self.headIndex
}

// A log of each group of an index built whole (the rebuild), holding every
// entry in join time order.
func affinityJoinLogsOf[P comparable](groupPaths map[P]map[P]time.Time) map[P]*affinityJoinLog[P] {
	joinLogs := make(map[P]*affinityJoinLog[P], len(groupPaths))
	for group, paths := range groupPaths {
		joins := make([]affinityJoin[P], 0, len(paths))
		for path, joinTime := range paths {
			joins = append(joins, affinityJoin[P]{path: path, joinTime: joinTime})
		}
		slices.SortFunc(joins, func(a affinityJoin[P], b affinityJoin[P]) int {
			return a.joinTime.Compare(b.joinTime)
		})
		joinLogs[group] = &affinityJoinLog[P]{joins: joins, entryCount: len(paths)}
	}
	return joinLogs
}

// The members of one group that a donor scan reads, each with its entry's
// join time. readAll reads every member (a pinned placement, or a member map
// that is not an index group). Otherwise the scan reads every member younger
// than maxTime at now and every stale member, whose entry no longer names its
// tuple's current generation: from the newest end of the group's log when it
// covers the group, the young members newest first and then the old stale
// ones; else from the whole group.
//
// The scan must not write the index while it reads.
func affinityDonorMembers[P comparable](
	paths map[P]time.Time,
	stalePaths map[P]bool,
	joinLog *affinityJoinLog[P],
	readAll bool,
	now time.Time,
	maxTime time.Duration,
) iter.Seq2[P, time.Time] {
	return func(yield func(P, time.Time) bool) {
		switch {
		case readAll:
			for path, joinTime := range paths {
				if !yield(path, joinTime) {
					return
				}
			}
		case joinLog.covers(paths, now, maxTime):
			for i := len(joinLog.joins) - 1; joinLog.headIndex <= i; i -= 1 {
				join := joinLog.joins[i]
				if maxTime <= now.Sub(join.joinTime) {
					// every earlier record is as old
					break
				}
				if joinTime, ok := paths[join.path]; !ok || !joinTime.Equal(join.joinTime) {
					// the tuple was written again, or left
					continue
				}
				if !yield(join.path, join.joinTime) {
					return
				}
			}
			for path := range stalePaths {
				joinTime, ok := paths[path]
				if !ok || now.Sub(joinTime) < maxTime {
					// a young stale member was read from its record
					continue
				}
				if !yield(path, joinTime) {
					return
				}
			}
		default:
			for path, joinTime := range paths {
				if maxTime <= now.Sub(joinTime) && !stalePaths[path] {
					continue
				}
				if !yield(path, joinTime) {
					return
				}
			}
		}
	}
}

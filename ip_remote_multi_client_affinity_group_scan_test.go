// The indexed donor scan passes over expired members without reading their
// updates, and answers exactly what reading every member answers.
package connect

import (
	"context"
	"fmt"
	"math/rand/v2"
	"os"
	"regexp"
	"strings"
	"testing"
	"testing/synctest"
	"time"
)

func affinityGroupScanTestDonor(parent *RemoteUserNatMultiClient) *multiClientChannel {
	return &multiClientChannel{
		ctx: parent.ctx, settings: parent.settings, healthy: true,
		args: &multiClientChannelArgs{DestinationStats: DestinationStats{IpFamily: IpFamilyDualstack}},
	}
}

// Runs both scans of one group for two fresh, unregistered flows and returns
// whether they placed the same way.
func affinityGroupScanTestCompare(parent *RemoteUserNatMultiClient, version int, group Ip4Path, group6 Ip6Path, pinned bool) (string, bool) {
	path := affinityTimeTestPath(version, 50000)
	full := newMultiClientChannelUpdate(parent.ctx, path)
	indexed := newMultiClientChannelUpdate(parent.ctx, path)
	defer full.Close()
	defer indexed.Close()
	full.pinned, indexed.pinned = pinned, pinned
	parent.stateLock.Lock()
	defer parent.stateLock.Unlock()
	var fullVerdict, indexedVerdict donorVerdict
	var fullScattered, indexedScattered bool
	if version == 4 {
		fullVerdict, fullScattered = parent.inheritAffinityClient4WithLock(full, parent.affinityIp4Paths[group])
		indexedVerdict, indexedScattered = parent.inheritAffinityGroupClient4WithLock(indexed, group)
	} else {
		fullVerdict, fullScattered = parent.inheritAffinityClient6WithLock(full, parent.affinityIp6Paths[group6])
		indexedVerdict, indexedScattered = parent.inheritAffinityGroupClient6WithLock(indexed, group6)
	}
	summary := fmt.Sprintf("full=(%v %v %p) indexed=(%v %v %p)", fullVerdict, fullScattered, full.client.Load(), indexedVerdict, indexedScattered, indexed.client.Load())
	return summary, fullVerdict == indexedVerdict && fullScattered == indexedScattered && full.client.Load() == indexed.client.Load()
}

// Every entry that is not stale names a current generation whose affinity
// origin is no later than the entry: the fact that lets the scan pass over
// it once the entry is AffinityMaxTime old. Stale entries are a subset of
// the group's entries.
func affinityGroupScanTestCheckIndex(t *testing.T, parent *RemoteUserNatMultiClient, step int) {
	t.Helper()
	parent.stateLock.Lock()
	defer parent.stateLock.Unlock()
	for group, paths := range parent.affinityIp4Paths {
		for path, joinTime := range paths {
			if parent.staleAffinityIp4Paths[group][path] {
				continue
			}
			if current := parent.ip4PathUpdates[path]; current != nil {
				if origin := current.client.originTime; !origin.IsZero() && joinTime.Before(origin) {
					t.Fatalf("step %d: a v4 entry that is not stale predates its current generation's origin", step)
				}
			}
		}
	}
	for group, stalePaths := range parent.staleAffinityIp4Paths {
		for path := range stalePaths {
			if _, ok := parent.affinityIp4Paths[group][path]; !ok {
				t.Fatalf("step %d: a stale v4 mark has no entry", step)
			}
		}
	}
	for group, paths := range parent.affinityIp6Paths {
		for path, joinTime := range paths {
			if parent.staleAffinityIp6Paths[group][path] {
				continue
			}
			if current := parent.ip6PathUpdates[path]; current != nil {
				if origin := current.client.originTime; !origin.IsZero() && joinTime.Before(origin) {
					t.Fatalf("step %d: a v6 entry that is not stale predates its current generation's origin", step)
				}
			}
		}
	}
	for group, stalePaths := range parent.staleAffinityIp6Paths {
		for path := range stalePaths {
			if _, ok := parent.affinityIp6Paths[group][path]; !ok {
				t.Fatalf("step %d: a stale v6 mark has no entry", step)
			}
		}
	}
	now := time.Now()
	maxTime := parent.reliabilitySettings().AffinityMaxTime
	affinityJoinLogTestCheckGroups(t, fmt.Sprintf("step %d v4", step), parent.affinityIp4Paths, parent.affinityIp4JoinLogs, now, maxTime)
	affinityJoinLogTestCheckGroups(t, fmt.Sprintf("step %d v6", step), parent.affinityIp6Paths, parent.affinityIp6JoinLogs, now, maxTime)
}

// Each group written through the production writers has a log that covers it
// at now: its records are in join time order, and every entry younger than
// maxTime has exactly one record of its tuple and join time. No log outlives
// its group.
func affinityJoinLogTestCheckGroups[P comparable](
	t *testing.T,
	at string,
	groupPaths map[P]map[P]time.Time,
	joinLogs map[P]*affinityJoinLog[P],
	now time.Time,
	maxTime time.Duration,
) {
	t.Helper()
	for group := range joinLogs {
		if _, ok := groupPaths[group]; !ok {
			t.Fatalf("%s: a join log outlives its group", at)
		}
	}
	for group, paths := range groupPaths {
		joinLog := joinLogs[group]
		if joinLog == nil {
			t.Fatalf("%s: a group has no join log", at)
		}
		if !joinLog.covers(paths, now, maxTime) {
			t.Fatalf("%s: a group's join log does not cover it (entries %d, counted %d)", at, len(paths), joinLog.entryCount)
		}
		live := joinLog.joins[joinLog.headIndex:]
		for i := 1; i < len(live); i += 1 {
			if live[i].joinTime.Before(live[i-1].joinTime) {
				t.Fatalf("%s: join log records out of join time order", at)
			}
		}
		for path, joinTime := range paths {
			if maxTime <= now.Sub(joinTime) {
				continue
			}
			matches := 0
			for _, join := range live {
				if join.path == path && join.joinTime.Equal(joinTime) {
					matches += 1
				}
			}
			if matches != 1 {
				t.Fatalf("%s: a young entry has %d records", at, matches)
			}
		}
	}
}

// Through the production writers only (sendUpdate, the reaper, generation
// reuse at a tuple, a learned server name that moves a tuple's next
// generation to another group, the index rebuild), in virtual time across
// the AffinityMaxTime boundary, the indexed scan places every fresh flow as
// the full scan does, pinned or not, and the index keeps the fact the
// indexed scan relies on.
func TestAffinityGroupScanMatchesFullScan(t *testing.T) {
	for _, version := range []int{4, 6} {
		synctest.Test(t, func(t *testing.T) {
			parent := affinityTimeTestParent(t)
			// deterministic so a failing sequence reproduces (CODESTYLE: tests may seed)
			rng := rand.New(rand.NewPCG(uint64(version), 77))
			donors := []*multiClientChannel{}
			for range 4 {
				donors = append(donors, affinityGroupScanTestDonor(parent))
			}
			named := stubServerNameLookup{names: []string{"shop.affinity.example"}}
			ports := []int{}
			for port := 40000; port < 40060; port++ {
				ports = append(ports, port)
			}
			compared := 0
			for step := range 600 {
				// distinct join times keep the legacy recency rank free of ties
				time.Sleep(time.Duration(1+rng.IntN(4000)) * time.Millisecond)
				switch op := rng.IntN(10); {
				case op < 5:
					// a new connection, or a new generation of a finished one
					if rng.IntN(4) == 0 {
						parent.config.Store(&multiClientConfig{serverNameLookup: named})
					} else {
						parent.config.Store(&multiClientConfig{})
					}
					port := ports[rng.IntN(len(ports))]
					update, _, client := parent.sendUpdate(affinityTimeTestPath(version, port), flowPin{})
					if update != nil && client == nil && rng.IntN(3) != 0 {
						donor := donors[rng.IntN(len(donors))]
						update.selectAffinityClient(donor)
						parent.bindClientFlow(update, donor)
					}
				case op < 7:
					// a connection ends
					parent.stateLock.Lock()
					for update := range parent.flowUpdates {
						if rng.IntN(5) == 0 {
							update.cancel()
						}
					}
					parent.stateLock.Unlock()
				case op < 8:
					retired, _, _ := parent.detachIdleFlows(time.Now())
					parent.finishRetiredFlows(retired)
				case op < 9:
					// minutes pass: entries cross the boundary in bulk
					time.Sleep(time.Duration(30+rng.IntN(60)) * time.Second)
				default:
					if rng.IntN(20) == 0 {
						parent.stateLock.Lock()
						parent.rebuildAffinityDonorIndexesWithLock(true)
						parent.stateLock.Unlock()
					}
				}
				affinityGroupScanTestCheckIndex(t, parent, step)
				parent.stateLock.Lock()
				groups4 := []Ip4Path{}
				for group := range parent.affinityIp4Paths {
					groups4 = append(groups4, group)
				}
				groups6 := []Ip6Path{}
				for group := range parent.affinityIp6Paths {
					groups6 = append(groups6, group)
				}
				parent.stateLock.Unlock()
				for _, pinned := range []bool{false, true} {
					for _, group := range groups4 {
						if summary, same := affinityGroupScanTestCompare(parent, 4, group, Ip6Path{}, pinned); !same {
							t.Fatalf("v%d step %d pinned=%v: %s", version, step, pinned, summary)
						}
						compared += 1
					}
					for _, group := range groups6 {
						if summary, same := affinityGroupScanTestCompare(parent, 6, Ip4Path{}, group, pinned); !same {
							t.Fatalf("v%d step %d pinned=%v: %s", version, step, pinned, summary)
						}
						compared += 1
					}
				}
			}
			if compared < 600 {
				t.Fatalf("v%d: only %d scans compared", version, compared)
			}
		})
	}
}

// A tuple's entry goes stale when the generation that wrote it is replaced
// by one in another group (a server name learned in between), and the stale
// entry is read in full: the newer generation it now resolves to can donate,
// as the full scan lets it. Rewriting the entry, the reaper and the rebuild
// clear the mark.
func TestAffinityGroupScanReadsStaleEntries(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		parent := affinityTimeTestParent(t)
		donor := affinityGroupScanTestDonor(parent)
		path := affinityTimeTestPath(4, 44000)
		// with no server name, the destination's group
		parent.stateLock.Lock()
		portGroup := parent.affinityIpPathsWithLock(path)[0].ToIp4Path()
		parent.stateLock.Unlock()

		first, _, _ := parent.sendUpdate(path, flowPin{})
		if first == nil || !first.affinityIp4Paths[portGroup] {
			t.Fatal("the first generation did not join the port group")
		}
		// the entry ages past the boundary, then the connection ends
		time.Sleep(2 * time.Minute)
		first.cancel()

		// the next generation at the tuple learns the site's name and joins
		// its group, not the port group, and wins a fresh exit
		parent.config.Store(&multiClientConfig{serverNameLookup: stubServerNameLookup{names: []string{"shop.affinity.example"}}})
		second, _, _ := parent.sendUpdate(path, flowPin{})
		if second == nil || second == first || second.affinityIp4Paths[portGroup] {
			t.Fatal("the second generation did not replace the first outside the port group")
		}
		second.selectAffinityClient(donor)
		parent.bindClientFlow(second, donor)
		parent.stateLock.Lock()
		stale := parent.staleAffinityIp4Paths[portGroup][path.ToIp4Path()]
		parent.stateLock.Unlock()
		if !stale {
			t.Fatal("the replaced generation's port-group entry is not stale")
		}

		// the full scan resolves the old entry to the second generation and
		// inherits its exit; so must the indexed scan, though the entry is old
		parent.config.Store(&multiClientConfig{})
		summary, same := affinityGroupScanTestCompare(parent, 4, portGroup, Ip6Path{}, false)
		if !same {
			t.Fatalf("a stale entry was passed over: %s", summary)
		}
		probe := newMultiClientChannelUpdate(parent.ctx, affinityTimeTestPath(4, 44009))
		defer probe.Close()
		parent.stateLock.Lock()
		verdict, _ := parent.inheritAffinityGroupClient4WithLock(probe, portGroup)
		parent.stateLock.Unlock()
		if verdict == donorRefused || probe.client.Load() != donor {
			t.Fatal("positive control: the stale entry's current generation did not donate")
		}

		// a generation that writes the entry again clears the mark
		second.cancel()
		third, _, _ := parent.sendUpdate(path, flowPin{})
		if third == nil || !third.affinityIp4Paths[portGroup] {
			t.Fatal("the third generation did not join the port group")
		}
		parent.stateLock.Lock()
		stale = parent.staleAffinityIp4Paths[portGroup][path.ToIp4Path()]
		parent.stateLock.Unlock()
		if stale {
			t.Fatal("rewriting the entry left it stale")
		}

		// the reaper removes the entry with its mark; the rebuild drops all marks
		third.cancel()
		retired, _, _ := parent.detachIdleFlows(time.Now())
		parent.finishRetiredFlows(retired)
		parent.stateLock.Lock()
		_, entry := parent.affinityIp4Paths[portGroup][path.ToIp4Path()]
		_, mark := parent.staleAffinityIp4Paths[portGroup][path.ToIp4Path()]
		parent.rebuildAffinityDonorIndexesWithLock(true)
		marks := len(parent.staleAffinityIp4Paths)
		parent.stateLock.Unlock()
		if entry || mark || marks != 0 {
			t.Fatalf("after the reaper and the rebuild: entry=%v mark=%v marks=%d", entry, mark, marks)
		}
	})
}

// The stale marks rely on every change of a tuple's current generation going
// through the canonical writers, so nothing else in the package assigns into
// or deletes from the tuple maps (close clears them whole, with the marks).
func TestIpPathUpdatesHaveCanonicalWriters(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	write := regexp.MustCompile(`ip[46]PathUpdates\[[^\]]*\]\s*=[^=]|delete\([a-z]+\.ip[46]PathUpdates,`)
	allowed := map[string]bool{
		"setIp4PathUpdateWithLock":    true,
		"deleteIp4PathUpdateWithLock": true,
		"setIp6PathUpdateWithLock":    true,
		"deleteIp6PathUpdateWithLock": true,
	}
	funcName := regexp.MustCompile(`^func (?:\([^)]*\) )?([A-Za-z0-9_]+)`)
	writers := 0
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		data, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		current := ""
		for i, line := range strings.Split(string(data), "\n") {
			if match := funcName.FindStringSubmatch(line); match != nil {
				current = match[1]
			}
			if strings.HasPrefix(strings.TrimSpace(line), "//") || !write.MatchString(line) {
				continue
			}
			if !allowed[current] {
				t.Errorf("%s:%d: %s writes a tuple map outside the canonical writers", name, i+1, current)
			}
			writers += 1
		}
	}
	if writers != 4 {
		t.Fatalf("found %d canonical writes, want 4 (the scan of this package may be broken)", writers)
	}
}

// One placement into a group of a busy hosted device: memberCount members
// whose entries are minutes old (their updates bound to an exit) and 50 young
// ones, written through the production writers. The full scan reads every
// member's update; the read of the whole group, as before the join log,
// iterates every member and reads the young ones' updates; the log scan reads
// the young ones from the newest end of the group's join log.
func BenchmarkAffinityGroupScan(b *testing.B) {
	for _, memberCount := range []int{100, 1000, 5000} {
		ctx, cancel := context.WithCancel(context.Background())
		settings := DefaultMultiClientSettings()
		settings.Log = NewNoopLogger()
		parent := flowReaperTestParent(ctx, settings)
		parent.cancel = cancel
		parent.reliabilityMetrics = newReliabilityMetrics()
		parent.config.Store(&multiClientConfig{})
		exit := affinityGroupScanTestDonor(parent)
		group := (&IpPath{Version: 4, DestinationIp: affinityTimeTestPath(4, 0).DestinationIp, DestinationPort: 443}).ToIp4Path()
		old := time.Now().Add(-5 * time.Minute)
		parent.stateLock.Lock()
		for i := range memberCount + 50 {
			path := affinityTimeTestPath(4, 10000+i)
			update := newMultiClientChannelUpdate(ctx, path)
			joinTime := time.Now()
			if i < memberCount {
				// an old member: its origin and its entry are both minutes old
				joinTime = old.Add(time.Duration(i) * time.Millisecond)
				update.client.originTime = joinTime
				update.openTime = joinTime
			}
			update.selectAffinityClient(exit)
			parent.setIp4PathUpdateWithLock(path.ToIp4Path(), update)
			parent.joinAffinityIp4WithLock(group, path.ToIp4Path(), joinTime)
		}
		parent.stateLock.Unlock()
		fresh := newMultiClientChannelUpdate(ctx, affinityTimeTestPath(4, 60000))
		for _, scan := range []string{"full", "group", "log"} {
			b.Run(fmt.Sprintf("members%d/%s", memberCount, scan), func(b *testing.B) {
				parent.stateLock.Lock()
				defer parent.stateLock.Unlock()
				for b.Loop() {
					fresh.client.Store(nil)
					var verdict donorVerdict
					switch scan {
					case "full":
						verdict, _ = parent.inheritAffinityClient4WithLock(fresh, parent.affinityIp4Paths[group])
					case "group":
						verdict, _ = parent.inheritAffinityClient4FromWithLock(fresh, parent.affinityIp4Paths[group], parent.staleAffinityIp4Paths[group], nil, true)
					default:
						verdict, _ = parent.inheritAffinityGroupClient4WithLock(fresh, group)
					}
					if verdict == donorRefused {
						b.Fatal("a young member did not donate")
					}
				}
			})
		}
		cancel()
	}
}

// A flow is done once its own context or the multi-client's is canceled, and
// never before; a bare or nil update is live.
func TestFlowUpdateIsDoneFollowsCancellation(t *testing.T) {
	parentCtx, parentCancel := context.WithCancel(context.Background())
	own := newMultiClientChannelUpdate(parentCtx, affinityTimeTestPath(4, 41000))
	inherited := newMultiClientChannelUpdate(parentCtx, affinityTimeTestPath(4, 41001))
	if own.IsDone() || inherited.IsDone() {
		t.Fatal("a new flow reads done")
	}
	own.cancel()
	if !own.IsDone() || inherited.IsDone() {
		t.Fatal("canceling one flow did not end exactly that flow")
	}
	parentCancel()
	if !inherited.IsDone() {
		t.Fatal("canceling the multi-client did not end its flow")
	}
	var bare *multiClientChannelUpdate
	if bare.IsDone() || (&multiClientChannelUpdate{}).IsDone() {
		t.Fatal("a nil or bare update reads done")
	}
}

// One reaper pass over memberCount live flows, none due: the pass asks each
// flow whether it is done.
func BenchmarkFlowReaperPass(b *testing.B) {
	for _, memberCount := range []int{1000, 5000} {
		ctx, cancel := context.WithCancel(context.Background())
		settings := DefaultMultiClientSettings()
		settings.Log = NewNoopLogger()
		parent := flowReaperTestParent(ctx, settings)
		parent.cancel = cancel
		parent.reliabilityMetrics = newReliabilityMetrics()
		for i := range memberCount {
			update := newMultiClientChannelUpdate(ctx, affinityTimeTestPath(4, 10000+i))
			update.activityTime = time.Now()
			parent.flowUpdates[update] = true
		}
		b.Run(fmt.Sprintf("flows%d", memberCount), func(b *testing.B) {
			for b.Loop() {
				if retired, _, _ := parent.detachIdleFlows(time.Now()); len(retired) != 0 {
					b.Fatal("a live flow was retired")
				}
			}
		})
		cancel()
	}
}

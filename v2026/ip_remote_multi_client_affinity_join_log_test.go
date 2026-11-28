// The affinity index's join logs hold each group's last AffinityMaxTime of
// writes, pass over the records their tuples no longer name, and give way to a
// read of the whole group when they cannot answer for it.
package connect

import (
	"os"
	"regexp"
	"strings"
	"testing"
	"testing/synctest"
	"time"
)

// The destination group of the fixture paths, which carry no server name.
func affinityJoinLogTestGroups(parent *RemoteUserNatMultiClient) (Ip4Path, Ip6Path) {
	parent.stateLock.Lock()
	defer parent.stateLock.Unlock()
	return parent.affinityIpPathsWithLock(affinityTimeTestPath(4, 0))[0].ToIp4Path(),
		parent.affinityIpPathsWithLock(affinityTimeTestPath(6, 0))[0].ToIp6Path()
}

// The members a donor scan of the v4 group reads, with every join time it reads
// each with.
func affinityJoinLogTestRead4(parent *RemoteUserNatMultiClient, group Ip4Path) map[Ip4Path][]time.Time {
	parent.stateLock.Lock()
	defer parent.stateLock.Unlock()
	members := map[Ip4Path][]time.Time{}
	for path, joinTime := range affinityDonorMembers(
		parent.affinityIp4Paths[group],
		parent.staleAffinityIp4Paths[group],
		parent.affinityIp4JoinLogs[group],
		false,
		time.Now(),
		parent.reliabilitySettings().AffinityMaxTime,
	) {
		members[path] = append(members[path], joinTime)
	}
	return members
}

// A busy group through the production placement: 2,000 flows that opened
// minutes ago and 50 in the last minute. The group's log holds the 50 young
// records only, so the scan walks 50 records where the read of the whole group
// iterates 2,050 members, and it places a fresh flow as the full scan does.
func TestAffinityGroupScanReadsOnlyTheLastMinute(t *testing.T) {
	for _, version := range []int{4, 6} {
		synctest.Test(t, func(t *testing.T) {
			parent := affinityTimeTestParent(t)
			exit := affinityGroupScanTestDonor(parent)
			open := func(port int) {
				update, _, client := parent.sendUpdate(affinityTimeTestPath(version, port), flowPin{})
				if update == nil {
					t.Fatal("a flow was not admitted")
				}
				if client == nil {
					update.selectAffinityClient(exit)
					parent.bindClientFlow(update, exit)
				}
				// distinct join times, as time.Now gives them
				time.Sleep(time.Millisecond)
			}
			for i := range 2000 {
				open(10000 + i)
			}
			time.Sleep(5 * time.Minute)
			for i := range 50 {
				open(30000 + i)
			}
			group4, group6 := affinityJoinLogTestGroups(parent)
			parent.stateLock.Lock()
			now := time.Now()
			maxTime := parent.reliabilitySettings().AffinityMaxTime
			var memberCount, recordCount, readCount int
			var covers bool
			if version == 4 {
				paths, joinLog := parent.affinityIp4Paths[group4], parent.affinityIp4JoinLogs[group4]
				memberCount, recordCount, covers = len(paths), joinLog.recordCount(), joinLog.covers(paths, now, maxTime)
				for range affinityDonorMembers(paths, parent.staleAffinityIp4Paths[group4], joinLog, false, now, maxTime) {
					readCount += 1
				}
			} else {
				paths, joinLog := parent.affinityIp6Paths[group6], parent.affinityIp6JoinLogs[group6]
				memberCount, recordCount, covers = len(paths), joinLog.recordCount(), joinLog.covers(paths, now, maxTime)
				for range affinityDonorMembers(paths, parent.staleAffinityIp6Paths[group6], joinLog, false, now, maxTime) {
					readCount += 1
				}
			}
			parent.stateLock.Unlock()
			if memberCount != 2050 || recordCount != 50 || !covers || readCount != 50 {
				t.Fatalf("v%d: members=%d records=%d covers=%v read=%d, want 2050, 50, true, 50", version, memberCount, recordCount, covers, readCount)
			}
			summary, same := affinityGroupScanTestCompare(parent, version, group4, group6, false)
			if !same {
				t.Fatalf("v%d: %s", version, summary)
			}
			// positive control: a young member donates the exit
			probe := newMultiClientChannelUpdate(parent.ctx, affinityTimeTestPath(version, 31000))
			defer probe.Close()
			parent.stateLock.Lock()
			verdict := donorRefused
			if version == 4 {
				verdict, _ = parent.inheritAffinityGroupClient4WithLock(probe, group4)
			} else {
				verdict, _ = parent.inheritAffinityGroupClient6WithLock(probe, group6)
			}
			parent.stateLock.Unlock()
			if verdict == donorRefused || probe.client.Load() != exit {
				t.Fatalf("v%d: positive control: no young member donated", version)
			}
		})
	}
}

// A record no longer names its entry once its tuple is written again with a
// later join time, or leaves the group; the scan passes over it, reading each
// young member once, with its entry's join time. A leave and a join again at
// the same instant leave one record.
func TestAffinityJoinLogPassesOverSupersededRecords(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		parent := affinityTimeTestParent(t)
		group, _ := affinityJoinLogTestGroups(parent)
		a := affinityTimeTestPath(4, 45000).ToIp4Path()
		b := affinityTimeTestPath(4, 45001).ToIp4Path()
		join := func(path Ip4Path, joinTime time.Time) {
			parent.stateLock.Lock()
			defer parent.stateLock.Unlock()
			parent.joinAffinityIp4WithLock(group, path, joinTime)
		}
		first := time.Now()
		join(a, first)
		join(b, first)
		time.Sleep(10 * time.Second)
		second := time.Now()
		join(a, second)

		members := affinityJoinLogTestRead4(parent, group)
		if len(members) != 2 || len(members[a]) != 1 || !members[a][0].Equal(second) || len(members[b]) != 1 || !members[b][0].Equal(first) {
			t.Fatalf("after a's second write: %v", members)
		}

		func() {
			parent.stateLock.Lock()
			defer parent.stateLock.Unlock()
			parent.leaveAffinityIp4WithLock(group, a)
		}()
		members = affinityJoinLogTestRead4(parent, group)
		if _, ok := members[a]; ok || len(members) != 1 {
			t.Fatalf("after a left: %v", members)
		}

		join(a, second)
		join(a, second)
		members = affinityJoinLogTestRead4(parent, group)
		if len(members) != 2 || len(members[a]) != 1 || !members[a][0].Equal(second) {
			t.Fatalf("after a joined again at the same instant: %v", members)
		}
		parent.stateLock.Lock()
		recordCount := parent.affinityIp4JoinLogs[group].recordCount()
		parent.stateLock.Unlock()
		if recordCount != 3 {
			t.Fatalf("records = %d, want a's two writes and b's", recordCount)
		}
	})
}

// A group's log holds its writes of the last AffinityMaxTime: each write drops
// the records that are AffinityMaxTime old, the array stays bounded, and the
// group's last leave drops the log.
func TestAffinityJoinLogHoldsTheLastMinuteOfWrites(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		parent := affinityTimeTestParent(t)
		group, _ := affinityJoinLogTestGroups(parent)
		maxTime := parent.reliabilitySettings().AffinityMaxTime
		if maxTime != time.Minute {
			t.Fatalf("AffinityMaxTime = %v, want the default minute", maxTime)
		}
		writeCount := 180
		for i := range writeCount {
			// one write a second for three minutes
			func() {
				parent.stateLock.Lock()
				defer parent.stateLock.Unlock()
				parent.joinAffinityIp4WithLock(group, affinityTimeTestPath(4, 46000+i).ToIp4Path(), time.Now())
				joinLog := parent.affinityIp4JoinLogs[group]
				now := time.Now()
				for _, join := range joinLog.joins[joinLog.headIndex:] {
					if maxTime <= now.Sub(join.joinTime) {
						t.Fatalf("write %d: a record %v old outlived the write", i, now.Sub(join.joinTime))
					}
				}
				if want := min(i+1, 60); joinLog.recordCount() != want {
					t.Fatalf("write %d: records = %d, want %d", i, joinLog.recordCount(), want)
				}
				if 256 < cap(joinLog.joins) {
					t.Fatalf("write %d: the log's array grew to %d records", i, cap(joinLog.joins))
				}
			}()
			time.Sleep(time.Second)
		}
		parent.stateLock.Lock()
		defer parent.stateLock.Unlock()
		for i := range writeCount {
			parent.leaveAffinityIp4WithLock(group, affinityTimeTestPath(4, 46000+i).ToIp4Path())
		}
		if _, ok := parent.affinityIp4JoinLogs[group]; ok {
			t.Fatal("the log outlived its group")
		}
	})
}

// AffinityMaxTime can grow at runtime. A log that dropped a record that would
// be young at the new AffinityMaxTime no longer covers its group, and the scan
// reads the whole group: it places a fresh flow through the member the log no
// longer holds, as the full scan does.
func TestAffinityGroupScanReadsTheGroupWhenAffinityMaxTimeGrows(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		parent := affinityTimeTestParent(t)
		donor := affinityGroupScanTestDonor(parent)
		group, _ := affinityJoinLogTestGroups(parent)
		first, _, client := parent.sendUpdate(affinityTimeTestPath(4, 47000), flowPin{})
		if first == nil || client != nil {
			t.Fatal("the first flow was not unbound")
		}
		first.selectAffinityClient(donor)
		parent.bindClientFlow(first, donor)
		// two minutes on, a write drops the first member's record
		time.Sleep(2 * time.Minute)
		if second, _, _ := parent.sendUpdate(affinityTimeTestPath(4, 47001), flowPin{}); second == nil {
			t.Fatal("the second flow was not admitted")
		}
		parent.stateLock.Lock()
		recordCount := parent.affinityIp4JoinLogs[group].recordCount()
		parent.stateLock.Unlock()
		if recordCount != 1 {
			t.Fatalf("records = %d, want the second member's", recordCount)
		}

		settings := *parent.reliabilitySettings()
		settings.AffinityMaxTime = 5 * time.Minute
		parent.SetReliabilitySettings(&settings)
		parent.stateLock.Lock()
		covers := parent.affinityIp4JoinLogs[group].covers(parent.affinityIp4Paths[group], time.Now(), settings.AffinityMaxTime)
		parent.stateLock.Unlock()
		if covers {
			t.Fatal("the log covers its group past a dropped record that is young again")
		}

		summary, same := affinityGroupScanTestCompare(parent, 4, group, Ip6Path{}, false)
		if !same {
			t.Fatalf("the grown AffinityMaxTime: %s", summary)
		}
		probe := newMultiClientChannelUpdate(parent.ctx, affinityTimeTestPath(4, 47009))
		defer probe.Close()
		parent.stateLock.Lock()
		verdict, _ := parent.inheritAffinityGroupClient4WithLock(probe, group)
		parent.stateLock.Unlock()
		if verdict == donorRefused || probe.client.Load() != donor {
			t.Fatal("positive control: the member young again did not donate")
		}
	})
}

// An entry written around the canonical writers (as a fixture may) has no
// record, and the log's count of the group's entries no longer matches: the
// scan reads the whole group and finds the entry.
func TestAffinityGroupScanReadsTheGroupWrittenAroundItsLog(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		parent := affinityTimeTestParent(t)
		donor := affinityGroupScanTestDonor(parent)
		group, _ := affinityJoinLogTestGroups(parent)
		if update, _, _ := parent.sendUpdate(affinityTimeTestPath(4, 48000), flowPin{}); update == nil {
			t.Fatal("the first flow was not admitted")
		}
		written := affinityTimeTestPath(4, 48001)
		update := newMultiClientChannelUpdate(parent.ctx, written)
		update.selectAffinityClient(donor)
		parent.stateLock.Lock()
		parent.setIp4PathUpdateWithLock(written.ToIp4Path(), update)
		parent.affinityIp4Paths[group][written.ToIp4Path()] = time.Now()
		parent.stateLock.Unlock()
		defer update.Close()

		summary, same := affinityGroupScanTestCompare(parent, 4, group, Ip6Path{}, false)
		if !same {
			t.Fatalf("an entry written around the log: %s", summary)
		}
		probe := newMultiClientChannelUpdate(parent.ctx, affinityTimeTestPath(4, 48009))
		defer probe.Close()
		parent.stateLock.Lock()
		verdict, _ := parent.inheritAffinityGroupClient4WithLock(probe, group)
		parent.stateLock.Unlock()
		if verdict == donorRefused || probe.client.Load() != donor {
			t.Fatal("the entry written around the log did not donate")
		}
	})
}

// The join logs rely on every write of the affinity index going through its
// writers, so nothing else in the package assigns a group or the index, writes
// an entry by index, or deletes or clears them. Writes through a local alias of
// a group are not seen here; keep them in the writers.
func TestAffinityIndexHasCanonicalWriters(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	write := regexp.MustCompile(`self\.(affinityIp[46]Paths|affinityIp[46]JoinLogs)(\[[^\]]*\])*\s*=[^=]|(delete|clear)\(self\.(affinityIp[46]Paths|affinityIp[46]JoinLogs)\b`)
	allowed := map[string]bool{
		"joinAffinityIp4WithLock":             true,
		"leaveAffinityIp4WithLock":            true,
		"joinAffinityIp6WithLock":             true,
		"leaveAffinityIp6WithLock":            true,
		"rebuildAffinityDonorIndexesWithLock": true,
		"Close":                               true,
	}
	funcName := regexp.MustCompile(`^func (?:\([^)]*\) )?([A-Za-z0-9_]+)`)
	writersSeen := map[string]bool{}
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
			if !allowed[current] || name != "ip_remote_multi_client.go" {
				t.Errorf("%s:%d: %s writes the affinity index outside its writers", name, i+1, current)
			}
			writersSeen[current] = true
		}
	}
	for writer := range allowed {
		if !writersSeen[writer] {
			t.Errorf("found no write in %s (the scan of this package may be broken)", writer)
		}
	}
}

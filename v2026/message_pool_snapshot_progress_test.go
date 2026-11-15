package connect

import (
	"bytes"
	"runtime"
	"sync"
	"testing"
	"time"
)

// Stack observation is test-only. It establishes that the named actual getter
// reached a mutex wait, without synctest's unsupported mutex quiescence or a
// sleep used as evidence that the goroutine ran.
func telemetryPoolCurrentGoroutine() string {
	var stack [512]byte
	count := runtime.Stack(stack[:], false)
	fields := bytes.Fields(stack[:count])
	if len(fields) < 2 {
		return ""
	}
	return string(fields[1])
}

type telemetryPoolProbe struct{ stack [1 << 20]byte }

func (self *telemetryPoolProbe) mutexWait(id, method string) bool {
	count := runtime.Stack(self.stack[:], true)
	for _, row := range bytes.Split(self.stack[:count], []byte("\n\n")) {
		if !bytes.HasPrefix(row, []byte("goroutine "+id+" ")) {
			continue
		}
		line := bytes.SplitN(row, []byte("\n"), 2)[0]
		return (bytes.Contains(line, []byte("sync.Mutex.Lock")) || bytes.Contains(line, []byte("semacquire"))) && bytes.Contains(row, []byte(method))
	}
	return false
}

func TestMessagePoolSnapshotDoesNotConvoyOtherPacketShards(t *testing.T) {
	pool := newMessagePool(256, 8)
	probe := new(telemetryPoolProbe)
	last := &pool.shards[messagePoolShardCount-1]
	last.stateLock.Lock()
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(last.stateLock.Unlock) }
	snapshotDone, takeDone := make(chan struct{}), make(chan struct{})
	snapshotId, takeId := make(chan string, 1), make(chan string, 1)
	var packet []byte
	takeStarted, returned := false, false
	t.Cleanup(func() {
		release()
		select {
		case <-snapshotDone:
		case <-time.After(5 * time.Second):
			t.Error("pool snapshot did not join")
		}
		if takeStarted {
			select {
			case <-takeDone:
				if !returned {
					pool.release(packet[:pool.size+MessagePoolMetaByteCount])
					returned = true
				}
			case <-time.After(5 * time.Second):
				t.Error("packet take did not join")
			}
		}
	})
	go func() { snapshotId <- telemetryPoolCurrentGoroutine(); _ = pool.snapshot(); close(snapshotDone) }()
	id := <-snapshotId
	deadline := time.Now().Add(5 * time.Second)
	for !probe.mutexWait(id, "(*messagePool).snapshot") {
		if time.Now().After(deadline) {
			t.Fatal("actual snapshot did not reach the held final shard")
		}
		runtime.Gosched()
	}
	// The only externally held shard is the last one. This ordinary packet
	// owner selects shard0 and must progress while the snapshot waits on last.
	pool.nextShard.Store(0)
	takeStarted = true
	go func() { takeId <- telemetryPoolCurrentGoroutine(); packet = pool.take(60, 7); close(takeDone) }()
	id = <-takeId
	blocked := false
observe:
	for {
		select {
		case <-takeDone:
			break observe
		default:
		}
		if probe.mutexWait(id, "(*messagePool).take") {
			blocked = true
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("packet owner neither completed nor reached its actual mutex wait")
		}
		runtime.Gosched()
	}
	release()
	<-snapshotDone
	<-takeDone
	if !pool.release(packet[:pool.size+MessagePoolMetaByteCount]) {
		t.Fatal("exact packet owner failed to return")
	}
	returned = true
	final := pool.snapshot()
	if final.capacity != 8 || final.retained != 1 || final.takenTags[7] != 1 || final.returnedTags[7] != 1 {
		t.Fatal("pool telemetry changed packet capacity or exact ownership")
	}
	t.Log("TELEMETRY_POOL_BEHAVIOR_DRAINED exact_take_return=1 capacity=8")
	t.Log("TELEMETRY_POOL_CLEANUP_JOINED snapshot=true packet_owner=true")
	if blocked {
		t.Error("TELEMETRY_SNAPSHOT_CONVOYS_UNRELATED_PACKET_SHARD")
	}
}

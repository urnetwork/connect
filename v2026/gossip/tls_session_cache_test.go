// The session cache of the operator links that go through a supplied dialer
// (tls_session_cache.go, connect#227).
//
// The sessions here are bare: what a cache keeps and hands out is the point,
// and crypto/tls checks a session's contents only once it is handed one.

package gossip

import (
	"crypto/tls"
	"fmt"
	"sync"
	"testing"
)

// One distinct session, told apart from the others by its pointer.
func newTestClientSession(t *testing.T, ticket string) *tls.ClientSessionState {
	t.Helper()
	session, err := tls.NewResumptionState([]byte(ticket), &tls.SessionState{})
	if err != nil {
		t.Fatal(err)
	}
	return session
}

// Every session is handed out once, newest first, and a server whose sessions
// are all handed out has none (RFC 8446 C.4).
func TestGossipTlsSessionCacheHandsEachSessionOutOnce(t *testing.T) {
	cache := newTlsSessionCache(defaultTlsSessionCacheSettings())
	first := newTestClientSession(t, "first")
	second := newTestClientSession(t, "second")
	cache.Put("gossip.space.example", first)
	cache.Put("gossip.space.example", second)

	if session, ok := cache.Get("gossip.space.example"); !ok || session != second {
		t.Fatalf("the first get handed out %p (%v), expected the newest session %p", session, ok, second)
	}
	if session, ok := cache.Get("gossip.space.example"); !ok || session != first {
		t.Fatalf("the second get handed out %p (%v), expected the older session %p", session, ok, first)
	}
	if session, ok := cache.Get("gossip.space.example"); ok {
		t.Fatalf("a session was handed out twice: %p", session)
	}
	if session, ok := cache.Get("other.space.example"); ok {
		t.Fatalf("a server that issued nothing had the session %p", session)
	}
}

// A nil put is crypto/tls forgetting the session it was handed. That session
// is already gone, and the server's other sessions stay.
func TestGossipTlsSessionCacheNilPutKeepsTheOtherSessions(t *testing.T) {
	cache := newTlsSessionCache(defaultTlsSessionCacheSettings())
	first := newTestClientSession(t, "first")
	second := newTestClientSession(t, "second")
	cache.Put("gossip.space.example", first)
	cache.Put("gossip.space.example", second)

	if session, ok := cache.Get("gossip.space.example"); !ok || session != second {
		t.Fatalf("get handed out %p (%v), expected %p", session, ok, second)
	}
	// the resumption with the second session failed
	cache.Put("gossip.space.example", nil)
	if session, ok := cache.Get("gossip.space.example"); !ok || session != first {
		t.Fatalf("after a nil put, get handed out %p (%v), expected the remaining session %p", session, ok, first)
	}
}

// Past the per-server bound the oldest session goes, and past the server bound
// the least recently used server goes, where a get counts as a use.
func TestGossipTlsSessionCacheBoundsServersAndSessions(t *testing.T) {
	cache := newTlsSessionCache(&tlsSessionCacheSettings{
		MaxServerCount:           2,
		MaxSessionCountPerServer: 2,
	})

	sessions := []*tls.ClientSessionState{}
	for i := range 3 {
		session := newTestClientSession(t, fmt.Sprintf("a%d", i))
		sessions = append(sessions, session)
		cache.Put("a.space.example", session)
	}
	if count := testTlsSessionCount(cache, "a.space.example"); count != 2 {
		t.Fatalf("a server kept %d sessions, expected the bound of 2", count)
	}
	if session, ok := cache.Get("a.space.example"); !ok || session != sessions[2] {
		t.Fatalf("get handed out %p (%v), expected the newest %p", session, ok, sessions[2])
	}
	if session, ok := cache.Get("a.space.example"); !ok || session != sessions[1] {
		t.Fatalf("get handed out %p (%v), expected %p, the oldest kept", session, ok, sessions[1])
	}
	if _, ok := cache.Get("a.space.example"); ok {
		t.Fatal("the oldest session outlived the per-server bound")
	}

	cache.Put("a.space.example", newTestClientSession(t, "a"))
	cache.Put("a.space.example", newTestClientSession(t, "a"))
	cache.Put("b.space.example", newTestClientSession(t, "b"))
	// a get makes a the most recently used, so c pushes out b
	if _, ok := cache.Get("a.space.example"); !ok {
		t.Fatal("a lost its sessions")
	}
	cache.Put("c.space.example", newTestClientSession(t, "c"))
	if count := testTlsSessionCount(cache, "b.space.example"); count != 0 {
		t.Fatalf("the least recently used server kept %d sessions past the server bound", count)
	}
	if count := testTlsSessionCount(cache, "a.space.example"); count != 1 {
		t.Fatalf("a kept %d sessions, expected 1", count)
	}
	if count := testTlsSessionCount(cache, "c.space.example"); count != 1 {
		t.Fatalf("c kept %d sessions, expected 1", count)
	}
}

// Settings that bound nothing take the defaults, rather than a cache that
// keeps nothing or everything.
func TestGossipTlsSessionCacheTakesTheDefaultBounds(t *testing.T) {
	cache := newTlsSessionCache(&tlsSessionCacheSettings{})
	defaults := defaultTlsSessionCacheSettings()
	if cache.settings.MaxServerCount != defaults.MaxServerCount {
		t.Errorf("server bound = %d, expected %d", cache.settings.MaxServerCount, defaults.MaxServerCount)
	}
	if cache.settings.MaxSessionCountPerServer != defaults.MaxSessionCountPerServer {
		t.Errorf(
			"session bound = %d, expected %d",
			cache.settings.MaxSessionCountPerServer,
			defaults.MaxSessionCountPerServer,
		)
	}
	// the process's cache is one value, made from the defaults
	if processTlsSessionCache() != processTlsSessionCache() {
		t.Error("the process's cache is not shared")
	}
}

// Many dials at once, as a miner's slots make them: no session is ever handed
// to two of them, and every one handed out was put first.
func TestGossipTlsSessionCacheConcurrentDialsNeverShareASession(t *testing.T) {
	cache := newTlsSessionCache(&tlsSessionCacheSettings{
		MaxServerCount:           4,
		MaxSessionCountPerServer: 8,
	})
	serverKeys := []string{"a.space.example", "b.space.example", "c.space.example"}

	putSessions := map[*tls.ClientSessionState]bool{}
	for i := range 64 {
		for _, serverKey := range serverKeys {
			putSessions[newTestClientSession(t, fmt.Sprintf("%s-%d", serverKey, i))] = true
		}
	}
	queue := make(chan *tls.ClientSessionState, len(putSessions))
	for session := range putSessions {
		queue <- session
	}
	close(queue)

	handedLock := sync.Mutex{}
	handedSessions := map[*tls.ClientSessionState]int{}
	wg := sync.WaitGroup{}
	for i := range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			serverKey := serverKeys[i%len(serverKeys)]
			for session := range queue {
				cache.Put(serverKey, session)
				if handed, ok := cache.Get(serverKey); ok {
					func() {
						handedLock.Lock()
						defer handedLock.Unlock()
						handedSessions[handed] += 1
					}()
				}
			}
		}()
	}
	wg.Wait()

	if len(handedSessions) == 0 {
		t.Fatal("no session was handed out")
	}
	for session, count := range handedSessions {
		if 1 < count {
			t.Errorf("a session was handed out %d times", count)
		}
		if !putSessions[session] {
			t.Error("a session that was never put was handed out")
		}
	}
}

// The sessions a cache holds for one server.
func testTlsSessionCount(cache *tlsSessionCache, serverKey string) int {
	cache.stateLock.Lock()
	defer cache.stateLock.Unlock()
	if i := cache.serverIndexWithLock(serverKey); 0 <= i {
		return len(cache.servers[i].sessions)
	}
	return 0
}

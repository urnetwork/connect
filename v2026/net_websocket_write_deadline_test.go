// Gorilla installs its stored write deadline on the socket before every frame.
// On a TCP socket each installation takes the poller's locks and, when the
// deadline moves, is a timer heap modify, so the batch conn forwards a write
// deadline only when it changes, zero included (server CPUOPT2.md §9). An
// expired write deadline may be replaced before the next operation. These
// pin that reduction and every installation that must still reach the socket.
package connect

import (
	"bytes"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// One deadline repeated across frames, a multi-frame message and a ready batch
// reaches the socket once, while every frame's bytes still do.
func TestWebSocketWriteBatchForwardsRepeatedWriteDeadlineOnce(t *testing.T) {
	ws, adapter, raw := newWebSocketDeadlineFixture(t)
	raw.resetDeadlineInstalls()
	deadline := time.Now().Add(time.Minute)
	if err := ws.SetWriteDeadline(deadline); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if err := ws.WriteMessage(websocket.BinaryMessage, []byte("synthetic frame")); err != nil {
			t.Fatal(err)
		}
	}
	// larger than the dialer's default write buffer, so gorilla splits it into
	// several frames and installs the deadline before each
	if err := ws.WriteMessage(websocket.BinaryMessage, bytes.Repeat([]byte{0x5a}, 10*1024)); err != nil {
		t.Fatal(err)
	}
	adapter.BeginWriteBatch()
	for range 3 {
		if err := ws.WriteMessage(websocket.BinaryMessage, []byte("synthetic batched frame")); err != nil {
			adapter.AbortWriteBatch()
			t.Fatal(err)
		}
	}
	if err := adapter.FlushWriteBatch(); err != nil {
		t.Fatal(err)
	}

	installs := raw.deadlineInstallsSnapshot()
	if len(installs) != 1 || installs[0].setter != "write" || !installs[0].deadline.Equal(deadline) {
		t.Fatalf("one unchanged write deadline reached the socket %d times, want once: %+v", len(installs), installs)
	}
	if raw.writeCount < 5 {
		t.Fatalf("socket writes=%d, want every frame written", raw.writeCount)
	}
}

// Every installation that changes the socket's write deadline is forwarded:
// a changed deadline, a WriteControl deadline, SetDeadline, and a zero deadline,
// which like any other is not forwarded again while the socket holds it. Each
// replaces the remembered value, so the data deadline is installed again after
// any of them.
func TestWebSocketWriteBatchForwardsEveryDeadlineChange(t *testing.T) {
	ws, adapter, raw := newWebSocketDeadlineFixture(t)
	raw.resetDeadlineInstalls()
	now := time.Now()
	dataDeadline := now.Add(time.Minute)
	controlDeadline := now.Add(2 * time.Minute)
	combinedDeadline := now.Add(3 * time.Minute)
	writeData := func() {
		t.Helper()
		if err := ws.WriteMessage(websocket.BinaryMessage, []byte("synthetic frame")); err != nil {
			t.Fatal(err)
		}
	}

	if err := ws.SetWriteDeadline(dataDeadline); err != nil {
		t.Fatal(err)
	}
	writeData()
	if err := ws.WriteControl(websocket.PingMessage, nil, controlDeadline); err != nil {
		t.Fatal(err)
	}
	writeData()
	if err := adapter.SetDeadline(combinedDeadline); err != nil {
		t.Fatal(err)
	}
	writeData()
	for range 2 {
		if err := adapter.SetWriteDeadline(time.Time{}); err != nil {
			t.Fatal(err)
		}
	}
	writeData()

	want := []webSocketDeadlineInstall{
		{setter: "write", deadline: dataDeadline},
		{setter: "write", deadline: controlDeadline},
		{setter: "write", deadline: dataDeadline},
		{setter: "deadline", deadline: combinedDeadline},
		{setter: "write", deadline: dataDeadline},
		{setter: "write", deadline: time.Time{}},
		{setter: "write", deadline: dataDeadline},
	}
	installs := raw.deadlineInstallsSnapshot()
	match := len(installs) == len(want)
	for i := 0; match && i < len(want); i += 1 {
		match = installs[i].setter == want[i].setter && installs[i].deadline.Equal(want[i].deadline)
	}
	if !match {
		t.Fatalf("socket deadline installations=%+v, want %+v", installs, want)
	}
	if !raw.writeDeadlineSnapshot().Equal(dataDeadline) {
		t.Fatalf("socket write deadline=%s, want the data deadline", raw.writeDeadlineSnapshot())
	}
}

// SetDeadline may race SetWriteDeadline (net.Conn's concurrent contract). The
// remembered value must still be the one the socket holds afterwards, or a
// later identical deadline is skipped while the socket keeps the other one.
func TestWebSocketWriteBatchWriteDeadlineFollowsConcurrentSetters(t *testing.T) {
	_, adapter, raw := newWebSocketDeadlineFixture(t)
	base := time.Now().Add(time.Hour)
	const setCount = 2000
	// distinct values per setter: the write setter's are even seconds, the
	// combined setter's odd
	writeDeadline := func(i int) time.Time { return base.Add(time.Duration(2*i) * time.Second) }
	combinedDeadline := func(i int) time.Time { return base.Add(time.Duration(2*i+1) * time.Second) }
	var setters sync.WaitGroup
	setters.Add(2)
	go func() {
		defer setters.Done()
		for i := range setCount {
			adapter.SetWriteDeadline(writeDeadline(i))
		}
	}()
	go func() {
		defer setters.Done()
		for i := range setCount {
			adapter.SetDeadline(combinedDeadline(i))
		}
	}()
	setters.Wait()

	for _, deadline := range []time.Time{writeDeadline(setCount - 1), combinedDeadline(setCount - 1), writeDeadline(setCount - 1)} {
		if err := adapter.SetWriteDeadline(deadline); err != nil {
			t.Fatal(err)
		}
		if held := raw.writeDeadlineSnapshot(); !held.Equal(deadline) {
			t.Fatalf("socket holds write deadline %s after installing %s", held, deadline)
		}
	}
}

// With no write deadline set, gorilla installs zero before every frame. Once the socket
// holds zero, none of those reaches it again, across single frames, a split
// message and a ready batch.
func TestWebSocketWriteBatchForwardsRepeatedZeroWriteDeadlineOnce(t *testing.T) {
	ws, adapter, raw := newWebSocketDeadlineFixture(t)
	if err := adapter.SetWriteDeadline(time.Now().Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	raw.resetDeadlineInstalls()
	if err := ws.SetWriteDeadline(time.Time{}); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if err := ws.WriteMessage(websocket.BinaryMessage, []byte("synthetic frame")); err != nil {
			t.Fatal(err)
		}
	}
	if err := ws.WriteMessage(websocket.BinaryMessage, bytes.Repeat([]byte{0x5a}, 10*1024)); err != nil {
		t.Fatal(err)
	}
	adapter.BeginWriteBatch()
	for range 3 {
		if err := ws.WriteMessage(websocket.BinaryMessage, []byte("synthetic batched frame")); err != nil {
			adapter.AbortWriteBatch()
			t.Fatal(err)
		}
	}
	if err := adapter.FlushWriteBatch(); err != nil {
		t.Fatal(err)
	}
	installs := raw.deadlineInstallsSnapshot()
	if len(installs) != 1 || installs[0].setter != "write" || !installs[0].deadline.IsZero() {
		t.Fatalf("a repeated zero write deadline reached the socket %d times, want once: %+v", len(installs), installs)
	}
}

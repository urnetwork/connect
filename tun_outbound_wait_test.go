package connect

import (
	"context"
	"testing"
	"time"
)

// A netstack writer must never wait forever for tun queue space: the
// goroutine that drains the queue can itself be the one injecting inbound
// packets (socks tun reader -> SendPacket -> receive callback -> Tun.Write ->
// gVisor reply), which is a self-deadlock when the queue is full. After the
// bounded wait the write drops the rest and the writer returns.
func TestTunOutboundQueueDropsAfterBoundedWait(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	settings := DefaultTunSettingsWithBufferSize(1)
	settings.OutboundQueueWaitTimeout = 50 * time.Millisecond
	tun, err := CreateTun(ctx, settings)
	if err != nil {
		t.Fatal(err)
	}
	defer tun.Close()

	firstPacket := newTunLinkTestPacket(1)
	defer firstPacket.DecRef()
	if result := writeTunLinkPacket(tun.ep, firstPacket); result.err != nil || result.n != 1 {
		t.Fatalf("first link write = %d, %v; want 1, nil", result.n, result.err)
	}

	secondPacket := newTunLinkTestPacket(2)
	defer secondPacket.DecRef()
	start := time.Now()
	result := writeTunLinkPacket(tun.ep, secondPacket)
	elapsed := time.Since(start)
	if result.err != nil || result.n != 0 {
		t.Fatalf("full-queue link write = %d, %v; want dropped 0, nil", result.n, result.err)
	}
	if elapsed < settings.OutboundQueueWaitTimeout || 2*time.Second < elapsed {
		t.Fatalf("full-queue link write took %s, want a bounded wait of about %s", elapsed, settings.OutboundQueueWaitTimeout)
	}
	if count := tun.OutboundDropCount(); count != 1 {
		t.Fatalf("outbound drop count = %d, want 1", count)
	}

	// the queue is intact: the first packet is still readable and a later
	// write proceeds once there is space
	firstRead, readErr := tun.Read()
	if readErr != nil {
		t.Fatal(readErr)
	}
	MessagePoolReturn(firstRead)
	thirdPacket := newTunLinkTestPacket(3)
	defer thirdPacket.DecRef()
	if result := writeTunLinkPacket(tun.ep, thirdPacket); result.err != nil || result.n != 1 {
		t.Fatalf("post-drop link write = %d, %v; want 1, nil", result.n, result.err)
	}
	thirdRead, readErr := tun.Read()
	if readErr != nil {
		t.Fatal(readErr)
	}
	MessagePoolReturn(thirdRead)
	if count := tun.OutboundDropCount(); count != 1 {
		t.Fatalf("outbound drop count after recovery = %d, want 1", count)
	}
}

// A non-positive bound keeps the original unbounded backpressure.
func TestTunOutboundQueueUnboundedWaitWhenDisabled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	settings := DefaultTunSettingsWithBufferSize(1)
	settings.OutboundQueueWaitTimeout = 0
	tun, err := CreateTun(ctx, settings)
	if err != nil {
		t.Fatal(err)
	}
	defer tun.Close()

	firstPacket := newTunLinkTestPacket(1)
	defer firstPacket.DecRef()
	if result := writeTunLinkPacket(tun.ep, firstPacket); result.err != nil || result.n != 1 {
		t.Fatalf("first link write = %d, %v; want 1, nil", result.n, result.err)
	}
	secondPacket := newTunLinkTestPacket(2)
	defer secondPacket.DecRef()
	done := make(chan tunLinkWriteResult, 1)
	go func() {
		done <- writeTunLinkPacket(tun.ep, secondPacket)
	}()
	select {
	case result := <-done:
		t.Fatalf("unbounded write returned without space: %d, %v", result.n, result.err)
	case <-time.After(100 * time.Millisecond):
	}
	firstRead, readErr := tun.Read()
	if readErr != nil {
		t.Fatal(readErr)
	}
	MessagePoolReturn(firstRead)
	select {
	case result := <-done:
		if result.err != nil || result.n != 1 {
			t.Fatalf("released link write = %d, %v; want 1, nil", result.n, result.err)
		}
	case <-time.After(time.Second):
		t.Fatal("queue space did not release the blocked link writer")
	}
	secondRead, readErr := tun.Read()
	if readErr != nil {
		t.Fatal(readErr)
	}
	MessagePoolReturn(secondRead)
}

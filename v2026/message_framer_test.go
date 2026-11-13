package connect

import (
	"bytes"
	"context"
	"fmt"
	"io"
	mathrand "math/rand"
	"net"
	"time"

	"testing"
)

// TestFramerObservedHandshakeCarrierAdmission preserves the production
// handshake-carrier regression with a larger synthetic carrier. The former
// 8-KiB cap rejected the observed envelope, causing the sender to retransmit the same
// immutable carrier while the receiver had already closed the stream. The
// bounded 16-KiB default must admit and preserve it; this is not a license to
// make ordinary data framing unbounded.
func TestFramerObservedHandshakeCarrierAdmission(t *testing.T) {
	const syntheticHandshakeCarrierByteCount = 10 * 1024
	carrier := bytes.Repeat([]byte{0xa5}, syntheticHandshakeCarrierByteCount)

	legacy := NewFramer(DefaultFramerSettings(8 * 1024))
	if err := legacy.Write(io.Discard, carrier); err == nil {
		t.Fatalf("former 8-KiB cap admitted %d-byte carrier", len(carrier))
	}

	limit := int(DefaultClientSettings().MinimumMessageLenLimit())
	if limit != 16*1024 {
		t.Fatalf("handshake admission limit %d, want bounded 16 KiB", limit)
	}
	current := NewFramer(DefaultFramerSettings(limit))
	var wire bytes.Buffer
	if err := current.Write(&wire, carrier); err != nil {
		t.Fatalf("16-KiB cap rejected observed %d-byte carrier: %s", len(carrier), err)
	}
	received, err := current.Read(&wire)
	if err != nil {
		t.Fatalf("read admitted carrier: %s", err)
	}
	defer MessagePoolReturn(received)
	if !bytes.Equal(received, carrier) {
		t.Fatalf("admitted carrier changed during framing")
	}
}

func TestFramerWriteRead(t *testing.T) {
	forEachIpVersion(t, func(t *testing.T, ipVersion int) {

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		timeout := 1 * time.Minute
		n := 10
		maxMessageLen := 1500

		randMessage := func(messageLen int, i int) []byte {
			source := mathrand.NewSource(int64(i*maxMessageLen + messageLen))
			r := mathrand.New(source)
			message := make([]byte, messageLen)
			r.Read(message)
			return message
		}

		settings := DefaultFramerSettings(maxMessageLen)
		framer := NewFramer(settings)

		select {
		case <-time.After(1 * time.Second):
		}

		listener, err := net.ListenTCP(testTcpNetwork(ipVersion), &net.TCPAddr{
			IP:   net.ParseIP(testLoopbackIp(ipVersion)),
			Port: 0,
		})
		if err != nil {
			panic(err)
		}
		defer listener.Close()
		go func() {
			defer cancel()

			s, err := listener.Accept()
			if err != nil {
				panic(err)
			}
			defer s.Close()

			for messageLen := 0; messageLen <= maxMessageLen; messageLen += 1 {
				if messageLen%10 == 0 {
					fmt.Printf("[framer]check len=%d...\n", messageLen)
				}
				for i := range n {
					message, err := framer.Read(s)
					if err != nil {
						panic(err)
					}
					AssertEqual(t, message, randMessage(messageLen, i))

				}
			}
		}()

		s, err := net.Dial(testTcpNetwork(ipVersion), listener.Addr().String())
		if err != nil {
			panic(err)
		}
		defer s.Close()

		for messageLen := 0; messageLen <= maxMessageLen; messageLen += 1 {
			for i := range n {
				message := randMessage(messageLen, i)
				err := framer.Write(s, message)
				if err != nil {
					panic(err)
				}
				// check that write does not modify the message
				AssertEqual(t, message, randMessage(messageLen, i))
			}
		}

		select {
		case <-ctx.Done():
		case <-time.After(timeout):
			t.FailNow()
		}
	})
}

func TestFramerSpeedup(t *testing.T) {
	forEachIpVersion(t, func(t *testing.T, ipVersion int) {
		ctx := context.Background()

		n := 100000
		message := make([]byte, 1500)

		runTcp := func(framer *Framer) {
			handleCtx, handleCancel := context.WithCancel(ctx)
			defer handleCancel()

			select {
			case <-time.After(1 * time.Second):
			}

			listener, err := net.ListenTCP(testTcpNetwork(ipVersion), &net.TCPAddr{
				IP:   net.ParseIP(testLoopbackIp(ipVersion)),
				Port: 0,
			})
			if err != nil {
				panic(err)
			}
			defer listener.Close()
			go func() {
				defer handleCancel()

				s, err := listener.Accept()
				if err != nil {
					panic(err)
				}
				defer s.Close()

				for range n {
					framer.Read(s)
				}
			}()

			s, err := net.Dial(testTcpNetwork(ipVersion), listener.Addr().String())
			if err != nil {
				panic(err)
			}
			defer s.Close()

			for range n {
				framer.Write(s, message)
			}

			select {
			case <-handleCtx.Done():
			}
		}

		settings := DefaultFramerSettings(2048)

		var startTime time.Time
		var endTime time.Time

		maxSpeedupTcp := float64(0)
		for range 4 {
			settings.SplitMinimumLen = len(message) + 1
			framerCopy := NewFramer(settings)
			startTime = time.Now()
			runTcp(framerCopy)
			endTime = time.Now()
			copyTcpDuration := endTime.Sub(startTime)

			settings.SplitMinimumLen = 256
			framerSplit := NewFramer(settings)
			startTime = time.Now()
			runTcp(framerSplit)
			endTime = time.Now()
			splitTcpDuration := endTime.Sub(startTime)

			speedupTcp := float64(splitTcpDuration) / float64(copyTcpDuration)
			fmt.Printf("[framer]tcp speedup %.2fx\n", speedupTcp)
			maxSpeedupTcp = max(maxSpeedupTcp, speedupTcp)
		}
		if maxSpeedupTcp <= 1 {
			t.FailNow()
		}
	})
}

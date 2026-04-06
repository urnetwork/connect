package connect

import (
	"context"
	"fmt"
	mathrand "math/rand"
	"net"
	"time"
	// "reflect"

	"testing"

	"github.com/go-playground/assert/v2"
)

func TestFramerWriteRead(t *testing.T) {

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

	settings := DefaultFramerSettings()
	settings.MaxMessageLen = maxMessageLen
	framer := NewFramer(settings)

	select {
	case <-time.After(1 * time.Second):
	}

	listener, err := net.ListenTCP("tcp", &net.TCPAddr{
		IP:   net.ParseIP("127.0.0.1"),
		Port: 5050,
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
				assert.Equal(t, message, randMessage(messageLen, i))

			}
		}
	}()

	s, err := net.Dial("tcp", "127.0.0.1:5050")
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
			assert.Equal(t, message, randMessage(messageLen, i))
		}
	}

	select {
	case <-ctx.Done():
	case <-time.After(timeout):
		t.FailNow()
	}
}

func TestFramerWriteReadPacket(t *testing.T) {

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	timeout := 1 * time.Minute
	n := 10
	// try to exceed the mtu to test fragmentation
	maxMessageLen := 3000

	randMessage := func(messageLen int, i int) []byte {
		source := mathrand.NewSource(int64(i*maxMessageLen + messageLen))
		r := mathrand.New(source)
		message := make([]byte, messageLen)
		r.Read(message)
		return message
	}

	settings := DefaultFramerSettings()
	settings.MaxMessageLen = maxMessageLen
	settings.MaxPacketMessageLen = maxMessageLen
	framer := NewFramer(settings)

	select {
	case <-time.After(1 * time.Second):
	}

	listener, err := net.ListenUDP("udp", &net.UDPAddr{
		IP:   net.ParseIP("127.0.0.1"),
		Port: 5051,
	})
	if err != nil {
		panic(err)
	}
	defer listener.Close()
	go func() {
		defer cancel()

		for messageLen := 0; messageLen <= maxMessageLen; messageLen += 1 {
			if messageLen%10 == 0 {
				fmt.Printf("[framer]check len=%d...\n", messageLen)
			}
			for i := range n {
				message, err := framer.ReadPacket(listener)
				if err != nil {
					panic(err)
				}
				assert.Equal(t, message, randMessage(messageLen, i))
			}
		}
	}()

	s, err := net.Dial("udp", "127.0.0.1:5051")
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
			assert.Equal(t, message, randMessage(messageLen, i))
		}
	}

	select {
	case <-ctx.Done():
	case <-time.After(timeout):
		t.FailNow()
	}
}

func TestFramerSpeedup(t *testing.T) {
	ctx := context.Background()

	n := 100000
	message := make([]byte, 1500)

	runTcp := func(framer *Framer) {
		handleCtx, handleCancel := context.WithCancel(ctx)
		defer handleCancel()

		select {
		case <-time.After(1 * time.Second):
		}

		listener, err := net.ListenTCP("tcp", &net.TCPAddr{
			IP:   net.ParseIP("127.0.0.1"),
			Port: 5050,
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

		s, err := net.Dial("tcp", "127.0.0.1:5050")
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

	runUdp := func(framer *Framer) {
		handleCtx, handleCancel := context.WithCancel(ctx)
		defer handleCancel()

		select {
		case <-time.After(1 * time.Second):
		}

		listener, err := net.ListenUDP("udp", &net.UDPAddr{
			IP:   net.ParseIP("127.0.0.1"),
			Port: 5051,
		})
		if err != nil {
			panic(err)
		}
		defer listener.Close()
		go func() {
			defer handleCancel()

			for range n {
				framer.ReadPacket(listener)
			}
		}()

		s, err := net.Dial("udp", "127.0.0.1:5051")
		if err != nil {
			panic(err)
		}
		defer s.Close()

		for {
			select {
			case <-handleCtx.Done():
				return
			default:
			}
			framer.Write(s, message)
		}
	}

	settings := DefaultFramerSettings()

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

	maxSpeedupUdp := float64(0)
	for range 4 {
		settings.SplitMinimumLen = len(message) + 1
		framerCopy := NewFramer(settings)
		startTime = time.Now()
		runUdp(framerCopy)
		endTime = time.Now()
		copyUdpDuration := endTime.Sub(startTime)

		settings.SplitMinimumLen = 256
		framerSplit := NewFramer(settings)
		startTime = time.Now()
		runUdp(framerSplit)
		endTime = time.Now()
		splitUdpDuration := endTime.Sub(startTime)

		speedupUdp := float64(splitUdpDuration) / float64(copyUdpDuration)
		fmt.Printf("[framer]udp speedup %.2fx\n", speedupUdp)
		maxSpeedupUdp = max(maxSpeedupUdp, speedupUdp)
	}
	if maxSpeedupUdp <= 1 {
		t.FailNow()
	}

}

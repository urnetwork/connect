package connect

import (
	"encoding/binary"
	"fmt"
	"io"
	"math"
	// "time"
	// "github.com/urnetwork/connect/v2025"
	// "github.com/urnetwork/glog/v2025"
)

// a message framer that optimizes memory copies to reduce cpu+memory usage
// on a typical connection, writing into the connection buffer will trigger a packet send
// and incur some fixed overhead
// to avoid small packets and excessive write calls, this framer approach breaks
// messages above a threshold into exactly two writes, resulting in an effective
// halving of the packet size on the wire
// the benefit of this approach is the framing can be done with zero additional memory allocation
// and a small constant memory copies before handing the message to the connection
// versus allocating and copying into a new framed message buffer,
// this approach is ~2x more cpu+memory efficient to send framed messages on a tcp/udp connection
// the framer read/write op is called billions of times in a typical user hour

type FramerSettings struct {
	MaxMessageLen   int
	SplitMinimumLen int
}

func DefaultFramerSettings() *FramerSettings {
	return &FramerSettings{
		MaxMessageLen:   2048,
		SplitMinimumLen: 256,
	}
}

// Read/ReadPacket and Write must be called from a single goroutine each
type Framer struct {
	readBuffer  []byte
	writeBuffer []byte
	settings    *FramerSettings
}

func NewFramerWithDefaults() *Framer {
	return NewFramer(DefaultFramerSettings())
}

func NewFramer(settings *FramerSettings) *Framer {
	framer := &Framer{
		readBuffer:  make([]byte, settings.MaxMessageLen),
		writeBuffer: make([]byte, settings.MaxMessageLen),
		settings:    settings,
	}
	if len(framer.writeBuffer) < settings.SplitMinimumLen+4 {
		panic(fmt.Errorf("SplitMinimumLen must be less than %d", len(framer.writeBuffer)-4))
	}
	return framer
}

func (self *Framer) Read(r io.Reader) ([]byte, error) {

	h := self.readBuffer[:]
	if _, err := io.ReadFull(r, h[0:4]); err != nil {
		return nil, err
	}

	messageLen := int(binary.LittleEndian.Uint16(h[0:2]))
	// splitIndex := int(binary.LittleEndian.Uint16(h[2:4]))

	if self.settings.MaxMessageLen < messageLen {
		// glog.Infof("READ MAX\n")
		return nil, fmt.Errorf("Max message len exceeded (%d<%d)", self.settings.MaxMessageLen, messageLen)
	}

	// message := make([]byte, messageLen)
	message := MessagePoolGet(messageLen)

	if _, err := io.ReadFull(r, message); err != nil {
		return nil, err
	}

	return message, nil
}

// use this version if the reader dequeues an entire packet per read
func (self *Framer) ReadPacket(r io.Reader) ([]byte, error) {
	h := self.readBuffer[:]

	n, err := r.Read(h)
	if err != nil {
		return nil, err
	}

	messageLen := int(binary.LittleEndian.Uint16(h[0:2]))
	splitIndex := int(binary.LittleEndian.Uint16(h[2:4]))

	if self.settings.MaxMessageLen < messageLen {
		// glog.Infof("READ MAX\n")
		return nil, fmt.Errorf("Max message len exceeded (%d<%d)", self.settings.MaxMessageLen, messageLen)
	}

	// message := make([]byte, messageLen)
	message := MessagePoolGet(messageLen)

	if splitIndex < 16 {
		// no split
		// note we could use 4 bit for additional signaling if needed

		copy(message[0:min(n-4, messageLen)], h[4:min(n, messageLen+4)])

		if n-4 < messageLen {
			if _, err := io.ReadFull(r, message[n-4:messageLen]); err != nil {
				return nil, err
			}
		}
	} else {
		copy(message[0:min(n-4, splitIndex)], h[4:min(n, splitIndex+4)])

		if n-4 < splitIndex {
			if _, err := io.ReadFull(r, message[n:splitIndex]); err != nil {
				return nil, err
			}
		}
		if splitIndex < n-4 {
			copy(message[splitIndex:n-4], h[splitIndex+4:n])
		}

		if _, err := io.ReadFull(r, message[n-4:messageLen]); err != nil {
			return nil, err
		}
	}

	return message, nil
}

// we assume a packet writer will fragment the message internally as needed
func (self *Framer) Write(w io.Writer, message []byte) error {
	messageLen := len(message)
	if self.settings.MaxMessageLen < messageLen {
		// glog.Infof("WRITE MAX\n")
		return fmt.Errorf("Max message len exceeded (%d<%d)", self.settings.MaxMessageLen, messageLen)
	}
	if math.MaxUint16 < messageLen {
		return fmt.Errorf("Max possible message len exceeded (%d<%d)", math.MaxUint16, messageLen)
	}
	if messageLen < max(16, self.settings.SplitMinimumLen) {
		messageWithHeader := self.writeBuffer[:]
		binary.LittleEndian.PutUint16(messageWithHeader[0:2], uint16(messageLen))
		binary.LittleEndian.PutUint16(messageWithHeader[2:4], uint16(0))
		copy(messageWithHeader[4:4+messageLen], message)
		if _, err := w.Write(messageWithHeader[0 : messageLen+4]); err != nil {
			return err
		}
	} else {
		// use half size packets and avoid large memory copy by writing the message in two parts

		splitIndex := messageLen / 2

		h := self.writeBuffer[:]
		binary.LittleEndian.PutUint16(h[0:2], uint16(messageLen))
		binary.LittleEndian.PutUint16(h[2:4], uint16(splitIndex))
		copy(h[4:4+splitIndex], message[0:splitIndex])

		if _, err := w.Write(h[0 : 4+splitIndex]); err != nil {
			return err
		}

		if _, err := w.Write(message[splitIndex:messageLen]); err != nil {
			return err
		}
	}

	return nil
}

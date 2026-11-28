// A frame read allocates nothing but its message: the bytes read before the
// message go into buffers the read side keeps, where a local array escaped
// through the reader on every frame. The uplink gate's channel sweeps snapshot
// onto the stack, where they built a slice on every sweep.
package connect

import (
	"bytes"
	"encoding/binary"
	"io"
	"net"
	"testing"
)

// A connection that reads from memory, for the frame reads below.
type frameReadTestConn struct {
	net.Conn
	reader *bytes.Reader
}

func (self *frameReadTestConn) Read(b []byte) (int, error) {
	return self.reader.Read(b)
}

func TestFramerReadHeaderAllocatesNothing(t *testing.T) {
	framer := NewFramer(DefaultFramerSettings(1024))
	frame := []byte{0, 3, 0, 0}
	reader := bytes.NewReader(frame)
	allocs := testing.AllocsPerRun(100, func() {
		reader.Reset(frame)
		if messageLen, err := framer.ReadHeader(reader); err != nil || messageLen != 3 {
			t.Fatalf("ReadHeader = %d, %v", messageLen, err)
		}
	})
	if allocs != 0 {
		t.Fatalf("ReadHeader allocates %v times a frame", allocs)
	}
}

func TestFramerXlReadHeaderAllocatesNothing(t *testing.T) {
	framer := NewFramerXl(DefaultFramerXlSettings(1024))
	frame := []byte{0, 0, 0, 3}
	reader := bytes.NewReader(frame)
	allocs := testing.AllocsPerRun(100, func() {
		reader.Reset(frame)
		if messageLen, err := framer.ReadHeader(reader); err != nil || messageLen != 3 {
			t.Fatalf("ReadHeader = %d, %v", messageLen, err)
		}
	})
	if allocs != 0 {
		t.Fatalf("ReadHeader allocates %v times a frame", allocs)
	}
}

// A header frame's header lands in the caller's header only once the frame is
// read whole, and the read allocates nothing but the pooled message.
func TestFramerReadWithHeaderAllocatesOnlyTheMessage(t *testing.T) {
	settings := DefaultFramerSettings(1024)
	settings.HeaderByteCount = transportDelayHeaderByteCount
	framer := NewFramer(settings)
	frameHeader := bytes.Repeat([]byte{7}, transportDelayHeaderByteCount)
	message := []byte{1, 2, 3}
	frame := binary.BigEndian.AppendUint16(nil, uint16(len(frameHeader)+len(message)))
	frame = append(frame, 0, 0)
	frame = append(frame, frameHeader...)
	frame = append(frame, message...)

	reader := bytes.NewReader(frame)
	header := make([]byte, transportDelayHeaderByteCount)
	read, err := framer.ReadWithHeader(reader, header)
	if err != nil || !bytes.Equal(read, message) || !bytes.Equal(header, frameHeader) {
		t.Fatalf("ReadWithHeader = %v, %v, header %v", read, err, header)
	}
	MessagePoolReturn(read)

	// a short frame leaves the caller's header as it was
	clear(header)
	reader.Reset(frame[:len(frame)-1])
	if _, err := framer.ReadWithHeader(reader, header); err == nil || !bytes.Equal(header, make([]byte, len(header))) {
		t.Fatalf("a short frame: %v, header %v", err, header)
	}

	allocs := testing.AllocsPerRun(100, func() {
		reader.Reset(frame)
		read, err := framer.ReadWithHeader(reader, header)
		if err != nil {
			t.Fatal(err)
		}
		MessagePoolReturn(read)
	})
	if allocs != 0 {
		t.Fatalf("ReadWithHeader allocates %v times a frame besides the pooled message", allocs)
	}
}

// A framed stream with the delay header reads the header into the reader's
// buffer and hands it to the read lane.
func TestFramedMessageConnDelayHeaderAllocatesNothing(t *testing.T) {
	message := []byte{1, 2, 3}
	frameHeader := make([]byte, transportDelayHeaderByteCount)
	frame := binary.BigEndian.AppendUint16(nil, uint16(len(frameHeader)+len(message)))
	frame = append(frame, 0, 0)
	frame = append(frame, frameHeader...)
	frame = append(frame, message...)

	conn := &frameReadTestConn{reader: bytes.NewReader(frame)}
	framed, err := NewFramedMessageConn(conn, H1FramerProtocol, 1024, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := framed.enableTransportDelay(newTransportDelay(nil, NewId(), TransportTypeH1, nil)); !ok {
		t.Fatal("the delay header was not enabled")
	}
	read := make([]byte, len(message))
	readFrame := func() {
		conn.reader.Reset(frame)
		_, body, err := framed.NextReader()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.ReadFull(body, read); err != nil || !bytes.Equal(read, message) {
			t.Fatalf("the message = %v, %v", read, err)
		}
	}
	readFrame()
	allocs := testing.AllocsPerRun(100, readFrame)
	if allocs != 0 {
		t.Fatalf("a delay header frame read allocates %v times", allocs)
	}
}

// A multi-client with clientCount channels holding flows, every second one
// sending and every third one receiving.
func channelSweepTestClient(clientCount int) (*RemoteUserNatMultiClient, []*multiClientChannel) {
	settings := DefaultMultiClientSettings()
	mc := &RemoteUserNatMultiClient{
		settings:                   settings,
		defaultReliabilitySettings: ReliabilitySettingsFrom(settings),
		clientUpdates:              map[*multiClientChannel]map[*multiClientChannelUpdate]bool{},
	}
	clients := []*multiClientChannel{}
	for i := range clientCount {
		client := stallTestChannel()
		mc.clientUpdates[client] = map[*multiClientChannelUpdate]bool{new(multiClientChannelUpdate): true}
		if i%2 == 0 {
			client.packetStats.sendNackCount = 1
		}
		if i%3 == 0 {
			client.addReceiveAck(1440)
		}
		clients = append(clients, client)
	}
	return mc, clients
}

func TestChannelSweepsAllocateNothing(t *testing.T) {
	mc, clients := channelSweepTestClient(24)
	if sending := mc.sendingChannelCount(); sending != 12 {
		t.Fatalf("sending channels = %d, want 12", sending)
	}
	// clients[1] does not receive, so all eight that do count
	if receiving := mc.receivingChannelCount(clients[1]); receiving != 8 {
		t.Fatalf("receiving channels = %d, want 8", receiving)
	}
	allocs := testing.AllocsPerRun(100, func() {
		mc.sendingChannelCount()
		mc.receivingChannelCount(clients[1])
	})
	if allocs != 0 {
		t.Fatalf("the two sweeps allocate %v times", allocs)
	}
}

// Past the stack array the sweeps count every channel.
func TestChannelSweepsCountPastTheStackArray(t *testing.T) {
	mc, clients := channelSweepTestClient(150)
	if sending := mc.sendingChannelCount(); sending != 75 {
		t.Fatalf("sending channels = %d, want 75", sending)
	}
	// clients[0] receives and is excluded
	if receiving := mc.receivingChannelCount(clients[0]); receiving != 49 {
		t.Fatalf("receiving channels = %d, want 49", receiving)
	}
}

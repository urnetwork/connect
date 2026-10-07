// The feed server (EXTENDER.md A8, D4).
//
// The feed is the bounded bootstrap of a client directory, and it is plain
// framed protobuf over one extender stream -- no libp2p at all. It lives here
// rather than in connect root only because it reads the same directory the node
// fills, and connect root never imports this package.
//
// `Serve` is what an extender installs as `ExtenderSettings.FeedConnHandler`.
// It owns the stream until it returns (A8), which for a subscriber is the whole
// life of the subscription, so the extender does not close a stream the client
// is still reading.
//
// One stream is: the client's request, up to `SampleCount` open records of
// the client's partition for this epoch with this node's own record first
// (Q2), `end_of_sample`, and then, when the client subscribed, every
// revocation the directory applies and every open record it applies that is
// in the client's partition, with a keepalive on an idle stream. The client's
// vantage is its address prefix (connect.ExtenderVantageKeyOfAddr): a client
// that polls or stays subscribed forever learns its partition of this
// extender's open tier and no more, and a gated record is never served (Q1).
// A subscriber that cannot keep up is disconnected rather than waited on --
// the directory's bounded subscription closes underneath it -- because one
// slow client must never hold up an apply.
//
// The server is safe for concurrent use.

package gossip

import (
	"context"
	"net"
	"sync"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/connect/protocol"
)

type FeedServerSettings struct {
	Log connect.Logger

	// The largest sample this server serves, whatever the client asks for (D4).
	MaxSampleCount int
	// Concurrent subscribers (D4). A client over the cap still gets its
	// sample; only the subscription is refused, so it takes its bootstrap and
	// reconnects elsewhere.
	MaxSubscriberCount int
	// An idle subscription is kept alive at this period (D4). A client treats
	// three missed keepalives as a dead stream (E3).
	KeepaliveTimeout time.Duration
	// Budget of the client's opening request, which bounds a connection that
	// opens the service and then says nothing.
	RequestTimeout time.Duration
	// Budget of one frame write, so a client that stops reading releases its
	// subscriber slot.
	WriteTimeout time.Duration
}

func DefaultFeedServerSettings() *FeedServerSettings {
	return &FeedServerSettings{
		MaxSampleCount:     connect.ExtenderFeedMaxSampleCount,
		MaxSubscriberCount: 256,
		KeepaliveTimeout:   30 * time.Second,
		RequestTimeout:     30 * time.Second,
		WriteTimeout:       30 * time.Second,
	}
}

type FeedServer struct {
	ctx    context.Context
	cancel context.CancelFunc
	log    connect.Logger

	settings  *FeedServerSettings
	directory *connect.ExtenderDirectory
	// the identity whose record is served first, empty on a node that has none
	ownPublicKey []byte

	stateLock       sync.Mutex
	subscriberCount int
}

func NewFeedServer(
	ctx context.Context,
	directory *connect.ExtenderDirectory,
	ownPublicKey []byte,
	settings *FeedServerSettings,
) *FeedServer {
	if settings == nil {
		settings = DefaultFeedServerSettings()
	}
	cancelCtx, cancel := context.WithCancel(ctx)
	return &FeedServer{
		ctx:          cancelCtx,
		cancel:       cancel,
		log:          loggerOrDefault(settings.Log),
		settings:     settings,
		directory:    directory,
		ownPublicKey: append([]byte(nil), ownPublicKey...),
	}
}

// Ends every open stream by cancelling the serves, which return and let the
// extender close their connections.
func (self *FeedServer) Close() {
	self.cancel()
}

// Serve runs one feed stream to completion (A8, D4). It returns when the
// sample is served and the client did not subscribe, when the client goes
// away, when the subscription falls behind, or when the server is closed.
func (self *FeedServer) Serve(conn net.Conn) {
	if err := conn.SetReadDeadline(time.Now().Add(self.settings.RequestTimeout)); err != nil {
		return
	}
	request, err := connect.ReadExtenderFeedRequest(conn)
	if err != nil {
		if self.log.V(1).Enabled() {
			self.log.Infof("[gossip]feed request err = %s\n", err)
		}
		return
	}
	if err := conn.SetReadDeadline(time.Time{}); err != nil {
		return
	}

	// the client sends nothing after the request, so a read that returns
	// anything at all means the client is gone or is not speaking the
	// protocol. The reader starts only now, because before this it would race
	// the request read for the same bytes.
	clientGone := make(chan struct{})
	go connect.HandleError(func() {
		defer close(clientGone)
		buffer := make([]byte, 1)
		conn.Read(buffer)
	})
	defer func() {
		// unblock the reader and join it, so no goroutine outlives the stream
		if err := conn.SetReadDeadline(time.Now()); err != nil {
			conn.Close()
		}
		<-clientGone
	}()

	// subscribe before the sample is taken, so a record applied between the
	// two is streamed rather than lost
	var messages <-chan *protocol.ExtenderGossipMessage
	if request.Subscribe && self.beginSubscriber() {
		var unsubscribe func()
		messages, unsubscribe = self.directory.Subscribe()
		defer unsubscribe()
		defer self.endSubscriber()
	}

	// the client's vantage, which binds it to one partition of the open tier
	// for the sample and the stream alike (Q2)
	vantage := connect.ExtenderVantageKeyOfAddr(conn.RemoteAddr())

	sampleCount := int(request.SampleCount)
	if self.settings.MaxSampleCount < sampleCount {
		sampleCount = self.settings.MaxSampleCount
	}
	for _, message := range self.directory.SampleRecords(sampleCount, self.ownPublicKey, vantage) {
		frame := &protocol.ExtenderFeedFrame{}
		switch {
		case message.GetRecord() != nil:
			frame.Frame = &protocol.ExtenderFeedFrame_Record{Record: message.GetRecord()}
		case message.GetRevocation() != nil:
			frame.Frame = &protocol.ExtenderFeedFrame_Revocation{Revocation: message.GetRevocation()}
		default:
			continue
		}
		if err := self.write(conn, frame); err != nil {
			return
		}
	}
	if err := self.write(conn, &protocol.ExtenderFeedFrame{
		Frame: &protocol.ExtenderFeedFrame_EndOfSample{EndOfSample: true},
	}); err != nil {
		return
	}
	if messages == nil {
		return
	}

	for {
		select {
		case <-self.ctx.Done():
			return
		case <-clientGone:
			return
		case message, ok := <-messages:
			if !ok {
				// the directory closed the subscription because this client
				// fell a whole buffer behind (D4)
				if self.log.V(1).Enabled() {
					self.log.Infof("[gossip]feed subscriber fell behind\n")
				}
				return
			}
			frame := &protocol.ExtenderFeedFrame{}
			switch {
			case message.GetRecord() != nil:
				if !self.streamsRecord(message.GetRecord(), vantage) {
					// outside the client's partition: as unseen on the
					// stream as in the sample (Q2)
					continue
				}
				frame.Frame = &protocol.ExtenderFeedFrame_Record{Record: message.GetRecord()}
			case message.GetRevocation() != nil:
				frame.Frame = &protocol.ExtenderFeedFrame_Revocation{Revocation: message.GetRevocation()}
			default:
				continue
			}
			if err := self.write(conn, frame); err != nil {
				return
			}
		case <-time.After(self.settings.KeepaliveTimeout):
			if err := self.write(conn, &protocol.ExtenderFeedFrame{
				Frame: &protocol.ExtenderFeedFrame_Keepalive{Keepalive: true},
			}); err != nil {
				return
			}
		}
	}
}

// Whether one applied record is streamed to a subscriber at `vantage` (Q2):
// the directory holds it in the open tier and in the vantage's partition. The
// body is decoded here without verifying it again -- the directory applied
// it, so its signature held -- and a body that does not decode is not
// streamed.
func (self *FeedServer) streamsRecord(record *protocol.ExtenderRecord, vantage []byte) bool {
	body := &protocol.ExtenderRecordBody{}
	if err := proto.Unmarshal(record.Body, body); err != nil {
		return false
	}
	return self.directory.OpenPartitionContains(vantage, body.PublicKey)
}

// Writes one frame under the write budget, so a client that stops reading does
// not hold its subscriber slot for the life of the process.
func (self *FeedServer) write(conn net.Conn, frame *protocol.ExtenderFeedFrame) error {
	if 0 < self.settings.WriteTimeout {
		if err := conn.SetWriteDeadline(time.Now().Add(self.settings.WriteTimeout)); err != nil {
			return err
		}
		defer conn.SetWriteDeadline(time.Time{})
	}
	return connect.WriteExtenderFeedFrame(conn, frame)
}

// Takes one subscriber slot, reporting whether there was one (D4).
func (self *FeedServer) beginSubscriber() bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if 0 < self.settings.MaxSubscriberCount &&
		self.settings.MaxSubscriberCount <= self.subscriberCount {
		return false
	}
	self.subscriberCount += 1
	return true
}

func (self *FeedServer) endSubscriber() {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.subscriberCount -= 1
}

// The subscribers this server is serving right now.
func (self *FeedServer) SubscriberCount() int {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.subscriberCount
}

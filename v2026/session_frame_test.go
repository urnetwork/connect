package connect

import (
	"testing"

	"github.com/urnetwork/connect/v2026/protocol"
	"google.golang.org/protobuf/proto"
)

func TestSessionControlFramesRoundTrip(t *testing.T) {
	for _, message := range []proto.Message{
		&protocol.NetworkSessionsChanged{Generation: "new-incarnation", EventId: 17},
		&protocol.StreamAuthorization{StreamId: []byte("stream"), AuthorizationGeneration: []byte("generation"), LeaseMillis: 90000, Retired: true, ClockId: []byte("challenge"), ClockUnixMillis: 1234, DeadlineUnixMillis: 91234},
		&protocol.StreamOpen{AuthorizationGeneration: []byte("generation"), AuthorizationLeaseMillis: 90000, AuthorizationDeadlineUnixMillis: 91234},
		&protocol.StreamClose{AuthorizationGeneration: []byte("generation")},
	} {
		for _, version := range []int{1, 2} {
			frame, err := ToFrame(message, version)
			if err != nil {
				t.Fatal(err)
			}
			decoded, err := FromFrame(frame)
			MessagePoolReturn(frame.MessageBytes)
			if err != nil || !proto.Equal(message, decoded) {
				t.Fatalf("%T version %d lost typed authorization fields: %v", message, version, err)
			}
		}
	}
}

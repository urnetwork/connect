package connect

import (
	"fmt"
	"hash/maphash"
	"io"
	"runtime"
	"sync"

	"google.golang.org/protobuf/proto"
)

// set this to true to tag messages with useful debugging information e.g. the creation site
const debugTags = false

var debugStateLock sync.Mutex
var debugTagCallers = map[uint8]map[string]bool{}

func debugTag() uint8 {
	_, file2, line2, ok := runtime.Caller(2)
	if !ok {
		return 0
	}
	_, file3, line3, ok := runtime.Caller(3)
	if !ok {
		return 0
	}
	seed := maphash.MakeSeed()
	caller := fmt.Sprintf("%s:%d->%s:%d", file3, line3, file2, line2)
	tag := uint8(maphash.String(seed, caller))
	func() {
		debugStateLock.Lock()
		defer debugStateLock.Unlock()

		callers, ok := debugTagCallers[tag]
		if !ok {
			callers = map[string]bool{}
			debugTagCallers[tag] = callers
		}
		callers[caller] = true
	}()
	return tag
}

func MessagePoolReadAll(r io.Reader) ([]byte, error) {
	var tag uint8
	if debugTags {
		tag = debugTag()
	}
	return MessagePoolReadAllWithTag(r, tag)
}

func MessagePoolCopy(message []byte) []byte {
	b, _ := MessagePoolCopyDetailed(message)
	return b
}

func MessagePoolCopyDetailed(message []byte) ([]byte, bool) {
	var tag uint8
	if debugTags {
		tag = debugTag()
	}
	return MessagePoolCopyDetailedWithTag(message, tag)
}

func MessagePoolCopyDetailedWithTag(message []byte, tag uint8) ([]byte, bool) {
	poolMessage, pooled := MessagePoolGetDetailedWithTag(len(message), tag)
	copy(poolMessage, message)
	return poolMessage, pooled
}

func MessagePoolGet(n int) []byte {
	b, _ := MessagePoolGetDetailed(n)
	return b
}

func MessagePoolGetDetailed(n int) ([]byte, bool) {
	var tag uint8
	if debugTags {
		tag = debugTag()
	}
	return MessagePoolGetDetailedWithTag(n, tag)
}

func ProtoMarshal(m proto.Message) ([]byte, error) {
	var tag uint8
	if debugTags {
		tag = debugTag()
	}
	return ProtoMarshalWithTag(m, tag)
}

func ProtoMarshalWithTag(m proto.Message, tag uint8) ([]byte, error) {
	if m == nil {
		return nil, nil
	}

	buf, _ := MessagePoolGetDetailedWithTag(proto.Size(m), tag)

	out, err := proto.MarshalOptions{}.MarshalAppend(buf[:0], m)
	if err != nil {
		MessagePoolReturn(buf)
		return nil, err
	}
	return out, nil
}

func ProtoUnmarshal(b []byte, m proto.Message) error {
	return proto.Unmarshal(b, m)
}

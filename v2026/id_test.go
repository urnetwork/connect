package connect

import (
	"encoding/json"
	"testing"

	"github.com/go-playground/assert/v2"
)

func TestIdOrder(t *testing.T) {
	// ulids are ordered by create time
	// we use this property in the system, where ulids from the same source can be ordered

	a := NewId()
	for range 1024 * 1024 {
		b := NewId()
		assert.Equal(t, a.LessThan(b), true)
		assert.Equal(t, b.LessThan(a), false)
		assert.Equal(t, b.LessThan(b), false)
		assert.Equal(t, b == a, false)
		assert.Equal(t, b == b, true)
		a = b
	}
}

var sinkId Id

func TestNewIdAllocs(t *testing.T) {
	// NewId is on the per-packet hot path (pack message ids); it must not allocate.
	allocs := testing.AllocsPerRun(10000, func() {
		sinkId = NewId()
	})
	if allocs != 0 {
		t.Fatalf("NewId allocates %v per call, want 0", allocs)
	}
}

func TestIdJsonCodec(t *testing.T) {
	type Test struct {
		A Id  `json:"a,omitempty"`
		B *Id `json:"b,omitempty"`
	}

	test1 := &Test{}
	test1.A = NewId()
	b_ := NewId()
	test1.B = &b_

	test1Json, err := json.Marshal(test1)
	assert.Equal(t, err, nil)

	test2 := &Test{}
	err = json.Unmarshal(test1Json, test2)
	assert.Equal(t, err, nil)

	assert.Equal(t, test1.A, test2.A)
	assert.Equal(t, test1.B, test2.B)

	test3 := &Test{}
	test3.A = NewId()

	test3Json, err := json.Marshal(test3)
	assert.Equal(t, err, nil)

	test4 := &Test{}
	err = json.Unmarshal(test3Json, test4)
	assert.Equal(t, err, nil)

	assert.Equal(t, test3.A, test4.A)
	assert.Equal(t, test3.B, nil)
	assert.Equal(t, test3.B, test4.B)
}

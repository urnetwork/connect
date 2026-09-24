package connect

import "testing"

// Separate the short-root shard lock cost from the large-packet fast return.
// The full sender and carrier A/B benchmarks measure their end-to-end impact.
func BenchmarkMessagePoolSmallUnorderedClassification(b *testing.B) {
	for _, size := range []int{120, 1280} {
		wire := MessagePoolGet(size)
		name := "small"
		if size > smallPacketPoolSize {
			name = "large"
		}
		b.Run("mark/"+name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				messagePoolMarkSmallUnordered(wire)
			}
		})
		b.Run("read/"+name, func(b *testing.B) {
			b.ReportAllocs()
			want := size <= smallPacketPoolSize
			for b.Loop() {
				if messagePoolIsSmallUnordered(wire) != want {
					b.Fatal("wrong scheduling classification")
				}
			}
		})
		MessagePoolReturn(wire)
	}
}

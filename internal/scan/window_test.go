package scan

import (
	"math/rand"
	"sort"
	"testing"
)

func TestWindowBoundedAndEvictsSmallest(t *testing.T) {
	w := NewSmallestFirstWindow(3)
	var evicted []int64
	for _, size := range []int64{50, 10, 40, 20, 30} {
		if d, ok := w.Offer(ObjectDescriptor{Size: size}); ok {
			evicted = append(evicted, d.Size)
		}
	}
	// After 4th offer (window full at 3): smallest of {50,10,40,20} = 10.
	// After 5th: smallest of {50,40,20,30} = 20.
	if len(evicted) != 2 || evicted[0] != 10 || evicted[1] != 20 {
		t.Fatalf("evictions: %v", evicted)
	}
	if w.Len() != 3 {
		t.Fatalf("window must stay bounded at 3, got %d", w.Len())
	}
}

func TestWindowDrainSmallestFirst(t *testing.T) {
	w := NewSmallestFirstWindow(100)
	r := rand.New(rand.NewSource(42))
	for i := 0; i < 100; i++ {
		w.Offer(ObjectDescriptor{Size: r.Int63n(1 << 30)})
	}
	var drained []int64
	w.Drain(func(d ObjectDescriptor) bool {
		drained = append(drained, d.Size)
		return true
	})
	if len(drained) != 100 {
		t.Fatalf("drained %d, want 100", len(drained))
	}
	if !sort.SliceIsSorted(drained, func(i, j int) bool { return drained[i] < drained[j] }) {
		t.Fatal("drain must be smallest-first")
	}
}

func TestWindowDrainStopsOnCancel(t *testing.T) {
	w := NewSmallestFirstWindow(10)
	for i := 0; i < 10; i++ {
		w.Offer(ObjectDescriptor{Size: int64(i)})
	}
	n := 0
	w.Drain(func(ObjectDescriptor) bool {
		n++
		return n < 4
	})
	if n != 4 {
		t.Fatalf("drain should stop when emit returns false, emitted %d", n)
	}
}

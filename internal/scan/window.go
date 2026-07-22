package scan

import "container/heap"

// descriptorHeap is a min-heap of ObjectDescriptors ordered by size.
type descriptorHeap []ObjectDescriptor

func (h descriptorHeap) Len() int            { return len(h) }
func (h descriptorHeap) Less(i, j int) bool  { return h[i].Size < h[j].Size }
func (h descriptorHeap) Swap(i, j int)       { h[i], h[j] = h[j], h[i] }
func (h *descriptorHeap) Push(x interface{}) { *h = append(*h, x.(ObjectDescriptor)) }
func (h *descriptorHeap) Pop() interface{} {
	old := *h
	n := len(old)
	item := old[n-1]
	*h = old[:n-1]
	return item
}

// SmallestFirstWindow implements the bounded min-heap scheduler (§5.2,
// H-01). Memory is bounded by the window size regardless of prefix
// breadth; ordering is approximate, not global — once the window is
// full each new arrival evicts the current smallest to the workers, and
// the heap drains smallest-first after listing completes.
type SmallestFirstWindow struct {
	limit int
	h     descriptorHeap
}

// NewSmallestFirstWindow returns a window holding at most limit
// descriptors. limit must be >= 1 (validated at flag parsing).
func NewSmallestFirstWindow(limit int) *SmallestFirstWindow {
	return &SmallestFirstWindow{limit: limit, h: make(descriptorHeap, 0, min(limit, 1024))}
}

// Offer adds d to the window. If the window is already full it returns
// the smallest descriptor for immediate dispatch and ok=true.
func (w *SmallestFirstWindow) Offer(d ObjectDescriptor) (evicted ObjectDescriptor, ok bool) {
	heap.Push(&w.h, d)
	if w.h.Len() > w.limit {
		return heap.Pop(&w.h).(ObjectDescriptor), true
	}
	return ObjectDescriptor{}, false
}

// Drain pops the remaining descriptors smallest-first, calling emit for
// each. emit returning false stops the drain (cancellation).
func (w *SmallestFirstWindow) Drain(emit func(ObjectDescriptor) bool) {
	for w.h.Len() > 0 {
		if !emit(heap.Pop(&w.h).(ObjectDescriptor)) {
			return
		}
	}
}

// Len reports how many descriptors are currently buffered.
func (w *SmallestFirstWindow) Len() int { return w.h.Len() }

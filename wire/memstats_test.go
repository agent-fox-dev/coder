package wire_test

import "runtime"

// totalAlloc is CUMULATIVE bytes allocated, which never decreases.
//
// HeapAlloc is the wrong instrument here and the difference is the whole test:
// the buffer under measurement is garbage by the time the measurement is
// taken, so a HeapAlloc reading — especially after a GC — shows nothing at all.
// An earlier version used it and did not notice a 16 MiB pre-allocation.
func totalAlloc() int64 {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return int64(m.TotalAlloc)
}

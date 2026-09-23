//go:build !linux

package runtime

// TreeResources is a best-effort aggregate process-tree measurement.
type TreeResources struct {
	PSSBytes     int64
	RSSBytes     int64
	ProcessCount int
}

// SampleTreeResources reports unavailable on non-Linux platforms —
// runtime inventory and bindings still work; only the memory sample is
// absent. Daemon startup must never depend on it.
func SampleTreeResources(procRoot string, pid int) (TreeResources, bool) {
	return TreeResources{}, false
}

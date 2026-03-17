package status

import "sync"

// statusFileMu serializes status snapshot updates within a single agent process.
//
// The agent runs multiple goroutines (periodic status collector and drift remediation)
// that can update the same JSON file. Writes are atomic on disk, but without a mutex
// we can still get last-writer-wins clobbering and read-modify-write lost updates.
//
// NOTE: This is intentionally an in-process lock only; we don't expect multiple
// agent processes on the same node.
var statusFileMu sync.Mutex

func withStatusFileLock(fn func() error) error {
	statusFileMu.Lock()
	defer statusFileMu.Unlock()
	return fn()
}

// File: recording_logger_test.go

package grcache_test

import (
	"sync"
)

// recordingLogger is a grcache.Logger that records every message it
// receives, for tests verifying a backend actually invokes its configured
// Logger (as opposed to merely accepting one without ever calling it). The
// zero value is ready to use. This was originally exported as
// conformance.RecordingLogger; folded in as an unexported test helper
// alongside the rest of the former conformance package.
type recordingLogger struct {
	mu     sync.Mutex
	infos  []string
	warns  []string
	errors []string
}

func (l *recordingLogger) Debug(msg string, args ...any) { l.record(&l.infos, msg) }
func (l *recordingLogger) Info(msg string, args ...any)  { l.record(&l.infos, msg) }
func (l *recordingLogger) Warn(msg string, args ...any)  { l.record(&l.warns, msg) }
func (l *recordingLogger) Error(msg string, args ...any) { l.record(&l.errors, msg) }

func (l *recordingLogger) record(dst *[]string, msg string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	*dst = append(*dst, msg)
}

// total returns the number of messages recorded across all levels.
func (l *recordingLogger) total() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.infos) + len(l.warns) + len(l.errors)
}

// File: recording_logger_test.go

package grcache_test

import (
	"fmt"
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

func (l *recordingLogger) Infof(format string, args ...interface{}) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.infos = append(l.infos, fmt.Sprintf(format, args...))
}

func (l *recordingLogger) Warnf(format string, args ...interface{}) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.warns = append(l.warns, fmt.Sprintf(format, args...))
}

func (l *recordingLogger) Errorf(format string, args ...interface{}) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.errors = append(l.errors, fmt.Sprintf(format, args...))
}

// total returns the number of messages recorded across all levels.
func (l *recordingLogger) total() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.infos) + len(l.warns) + len(l.errors)
}

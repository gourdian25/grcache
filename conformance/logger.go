// File: conformance/logger.go

package conformance

import (
	"fmt"
	"sync"
)

// RecordingLogger is a grcache.Logger that records every message it
// receives, for tests verifying a backend actually invokes its configured
// Logger (as opposed to merely accepting one without ever calling it).
type RecordingLogger struct {
	mu     sync.Mutex
	Infos  []string
	Warns  []string
	Errors []string
}

func (l *RecordingLogger) Infof(format string, args ...interface{}) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.Infos = append(l.Infos, fmt.Sprintf(format, args...))
}

func (l *RecordingLogger) Warnf(format string, args ...interface{}) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.Warns = append(l.Warns, fmt.Sprintf(format, args...))
}

func (l *RecordingLogger) Errorf(format string, args ...interface{}) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.Errors = append(l.Errors, fmt.Sprintf(format, args...))
}

// Total returns the number of messages recorded across all levels.
func (l *RecordingLogger) Total() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.Infos) + len(l.Warns) + len(l.Errors)
}

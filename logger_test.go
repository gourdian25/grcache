// File: logger_test.go

package grcache_test

// This file proves *grlog.Logger satisfies grcache.Logger structurally,
// without grcache itself importing grlog — grlog is a test-only dependency
// of this module, so it never leaks into consumers who only import
// grcache/memory (or any other backend) and don't want a logging
// dependency at all.

import (
	"testing"

	"github.com/gourdian25/grlog"

	"github.com/gourdian25/grcache"
)

var _ grcache.Logger = (*grlog.Logger)(nil)

func TestGrlogSatisfiesLoggerInterface(t *testing.T) {
	logger := grlog.NewDefaultLogger()

	var l grcache.Logger = logger
	l.Infof("grcache test: %s", "info")
	l.Warnf("grcache test: %s", "warn")
	l.Errorf("grcache test: %s", "error")
}

func TestNopLogger(t *testing.T) {
	l := grcache.NopLogger()
	// Must not panic with no logger installed.
	l.Infof("noop")
	l.Warnf("noop")
	l.Errorf("noop")
}

func TestOrNop(t *testing.T) {
	if grcache.OrNop(nil) == nil {
		t.Fatal("OrNop(nil) = nil, want a non-nil no-op Logger")
	}

	logger := grlog.NewDefaultLogger()
	if grcache.OrNop(logger) != grcache.Logger(logger) {
		t.Fatal("OrNop(non-nil) did not return the given logger unchanged")
	}
}

// File: logger_test.go

package grcache_test

// This file proves *slog.Logger satisfies grcache.Logger structurally,
// without grcache itself importing grlog or log/slog — grlog is a
// test-only dependency of this module, so it never leaks into consumers
// who only import grcache/memory (or any other backend) and don't want a
// logging dependency at all.

import (
	"log/slog"
	"testing"

	"github.com/gourdian25/grcache"
	"github.com/gourdian25/grlog"
)

var _ grcache.Logger = (*slog.Logger)(nil)

func TestGrlogSatisfiesLoggerInterface(t *testing.T) {
	logger := grlog.NewDefaultLogger()
	defer func() { _ = logger.Close() }()

	slogger := slog.New(grlog.NewSlogHandler(logger))
	var l grcache.Logger = slogger

	l.Debug("grcache test", "level", "debug")
	l.Info("grcache test", "level", "info")
	l.Warn("grcache test", "level", "warn")
	l.Error("grcache test", "level", "error")
}

func TestNopLogger(t *testing.T) {
	l := grcache.NopLogger()
	// Must not panic with no logger installed.
	l.Debug("noop")
	l.Info("noop")
	l.Warn("noop")
	l.Error("noop")
}

func TestOrNop(t *testing.T) {
	if grcache.OrNop(nil) == nil {
		t.Fatal("OrNop(nil) = nil, want a non-nil no-op Logger")
	}

	logger := grlog.NewDefaultLogger()
	defer func() { _ = logger.Close() }()

	slogger := slog.New(grlog.NewSlogHandler(logger))
	if grcache.OrNop(slogger) != grcache.Logger(slogger) {
		t.Fatal("OrNop(non-nil) did not return the given logger unchanged")
	}
}

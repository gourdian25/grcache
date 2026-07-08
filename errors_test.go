// File: errors_test.go

package grcache

import (
	"errors"
	"fmt"
	"testing"
)

func TestSentinelErrors_Is(t *testing.T) {
	sentinels := map[string]error{
		"ErrKeyNotFound":      ErrKeyNotFound,
		"ErrCacheUnavailable": ErrCacheUnavailable,
		"ErrInvalidTTL":       ErrInvalidTTL,
		"ErrClosed":           ErrClosed,
	}

	for name, sentinel := range sentinels {
		t.Run(name, func(t *testing.T) {
			wrapped := fmt.Errorf("grcache: get %q: %w", "some-key", sentinel)

			if !errors.Is(wrapped, sentinel) {
				t.Fatalf("errors.Is(wrapped, %s) = false, want true", name)
			}

			for otherName, other := range sentinels {
				if otherName == name {
					continue
				}
				if errors.Is(wrapped, other) {
					t.Fatalf("errors.Is(wrapped %s, %s) = true, want false", name, otherName)
				}
			}
		})
	}
}

func TestSentinelErrors_DistinctMessages(t *testing.T) {
	sentinels := []error{ErrKeyNotFound, ErrCacheUnavailable, ErrInvalidTTL, ErrClosed}

	seen := make(map[string]bool)
	for _, err := range sentinels {
		msg := err.Error()
		if seen[msg] {
			t.Fatalf("duplicate sentinel error message: %q", msg)
		}
		seen[msg] = true
	}
}

//go:build darwin || linux

package lock

import (
	"errors"
	"path/filepath"
	"testing"
)

func TestAcquireIsExclusive(t *testing.T) {
	path := filepath.Join(t.TempDir(), "run.lock")
	first, err := Acquire(path)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Release()
	second, err := Acquire(path)
	if !errors.Is(err, ErrHeld) || second != nil {
		t.Fatalf("second = %#v, %v", second, err)
	}
}

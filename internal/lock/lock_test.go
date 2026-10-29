package lock

import (
	"errors"
	"path/filepath"
	"testing"
)

func TestAcquireTwice(t *testing.T) {
	p := filepath.Join(t.TempDir(), "lock")
	rel, err := Acquire(p)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Acquire(p); !errors.Is(err, ErrBusy) {
		t.Fatalf("second Acquire = %v, want ErrBusy", err)
	}
	rel()
	rel2, err := Acquire(p)
	if err != nil {
		t.Fatalf("after release: %v", err)
	}
	rel2()
}

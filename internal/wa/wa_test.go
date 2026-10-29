package wa

import (
	"context"
	"path/filepath"
	"testing"
)

func TestOpenContainerWithModernc(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.db")
	c, err := OpenContainer(context.Background(), path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	dev, err := c.GetFirstDevice(context.Background())
	if err != nil || dev == nil {
		t.Fatalf("first device: %v %v", dev, err)
	}
	if dev.ID != nil {
		t.Fatalf("fresh store should be unlinked")
	}
}

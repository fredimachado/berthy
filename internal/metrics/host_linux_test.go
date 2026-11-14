//go:build linux

package metrics

import (
	"context"
	"testing"
)

func TestLinuxHostReportsAvailableValues(t *testing.T) {
	h := NewHostReader()
	s := h.Read(context.Background())
	if s.MemoryTotal == nil || *s.MemoryTotal == 0 || len(s.Load) != 3 || s.NetworkRXBytes == nil || len(s.Mounts) == 0 {
		t.Fatalf("incomplete host: %+v", s)
	}
}

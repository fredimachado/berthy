package keycopy

import (
	"encoding/binary"
	"testing"
)

func TestParseMacACLAttributes(t *testing.T) {
	for _, tc := range []struct {
		name  string
		count uint32
		want  bool
	}{{"none", ^uint32(0), false}, {"empty", 0, false}, {"entry", 1, true}} {
		t.Run(tc.name, func(t *testing.T) {
			b := make([]byte, 56)
			binary.LittleEndian.PutUint32(b, 56)
			binary.LittleEndian.PutUint32(b[4:], 8)
			binary.LittleEndian.PutUint32(b[8:], 44)
			binary.LittleEndian.PutUint32(b[12:], 0x012cc16d)
			binary.LittleEndian.PutUint32(b[48:], tc.count)
			got, err := parseACLAttributes(b)
			if err != nil || got != tc.want {
				t.Fatalf("ACL result=%v err=%v", got, err)
			}
		})
	}
	if _, err := parseACLAttributes([]byte{1, 2, 3}); err == nil {
		t.Fatal("accepted truncated attributes")
	}
	empty := make([]byte, 12)
	binary.LittleEndian.PutUint32(empty, 12)
	if has, err := parseACLAttributes(empty); err != nil || has {
		t.Fatalf("empty ACL: %v", err)
	}
	bad := make([]byte, 56)
	binary.LittleEndian.PutUint32(bad, 56)
	binary.LittleEndian.PutUint32(bad[4:], 8)
	binary.LittleEndian.PutUint32(bad[8:], 44)
	if _, err := parseACLAttributes(bad); err == nil {
		t.Fatal("accepted invalid security magic")
	}
	binary.LittleEndian.PutUint32(bad[12:], 0x012cc16d)
	binary.LittleEndian.PutUint32(bad[8:], 45)
	if _, err := parseACLAttributes(bad); err == nil {
		t.Fatal("accepted truncated security record")
	}
}

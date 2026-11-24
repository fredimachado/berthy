package keycopy

import (
	"encoding/binary"
	"errors"
)

func parseACLAttributes(buf []byte) (bool, error) {
	if len(buf) < 12 {
		return false, errors.New("invalid ACL attributes")
	}
	total := int(binary.LittleEndian.Uint32(buf))
	if total < 12 || total > len(buf) {
		return false, errors.New("invalid ACL attributes")
	}
	offset := int(int32(binary.LittleEndian.Uint32(buf[4:])))
	length := int(binary.LittleEndian.Uint32(buf[8:]))
	if length == 0 {
		return false, nil
	}
	start := 4 + offset
	if start < 12 || length < 44 || start > total || length > total-start {
		return false, errors.New("invalid ACL attributes")
	}
	if binary.LittleEndian.Uint32(buf[start:]) != 0x012cc16d {
		return false, errors.New("invalid ACL attributes")
	}
	count := binary.LittleEndian.Uint32(buf[start+36:])
	return count != 0 && count != ^uint32(0), nil
}

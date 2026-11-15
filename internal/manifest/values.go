package manifest

import (
	"fmt"
	"math"
	"path"
	"regexp"
	"strconv"
	"strings"
)

type Mount struct {
	Source, Target string
	ReadOnly       bool
}

func ParseMount(s string) (Mount, error) {
	p := strings.Split(s, ":")
	if len(p) < 2 || len(p) > 3 || !regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]*$`).MatchString(p[0]) || !strings.HasPrefix(p[1], "/") || path.Clean(p[1]) != p[1] || p[1] == "/" || strings.ContainsAny(p[1], "\x00\r\n") {
		return Mount{}, fmt.Errorf("invalid named volume")
	}
	m := Mount{Source: p[0], Target: p[1]}
	if len(p) == 3 {
		if p[2] != "ro" && p[2] != "rw" {
			return Mount{}, fmt.Errorf("invalid mount mode")
		}
		m.ReadOnly = p[2] == "ro"
	}
	return m, nil
}
func ParseCPU(s string) (int64, error) {
	v, err := strconv.ParseFloat(s, 64)
	n := v * 1e9
	if err != nil || math.IsNaN(n) || math.IsInf(n, 0) || n < 1 || n >= float64(math.MaxInt64) || math.Trunc(n) != n {
		return 0, fmt.Errorf("invalid CPU")
	}
	return int64(n), nil
}

var memoryPattern = regexp.MustCompile(`^([0-9]+)([kKmMgGtT]([iI]?[bB])?|[bB])?$`)

func ParseMemory(s string) (int64, error) {
	m := memoryPattern.FindStringSubmatch(s)
	if m == nil {
		return 0, fmt.Errorf("invalid memory")
	}
	v, err := strconv.ParseUint(m[1], 10, 63)
	if err != nil || v == 0 {
		return 0, fmt.Errorf("invalid memory")
	}
	factor := uint64(1)
	if m[2] != "" {
		switch strings.ToLower(m[2][:1]) {
		case "k":
			factor = 1 << 10
		case "m":
			factor = 1 << 20
		case "g":
			factor = 1 << 30
		case "t":
			factor = 1 << 40
		}
	}
	if v > math.MaxInt64/factor {
		return 0, fmt.Errorf("memory overflow")
	}
	return int64(v * factor), nil
}

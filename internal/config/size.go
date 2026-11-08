// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"fmt"
	"math"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// Size is a byte count written in YAML as "2GiB", "256MiB", "10MB" or a plain
// number of bytes.
type Size int64

// Binary and decimal size units.
const (
	B   Size = 1
	KiB      = 1024 * B
	MiB      = 1024 * KiB
	GiB      = 1024 * MiB
)

var units = []struct {
	suffix string
	mult   Size
}{
	{"GiB", GiB}, {"MiB", MiB}, {"KiB", KiB},
	{"GB", 1000 * 1000 * 1000}, {"MB", 1000 * 1000}, {"KB", 1000},
	{"B", B},
}

// ParseSize parses a size such as "2GiB", "1.5GB" or "1048576".
func ParseSize(s string) (Size, error) {
	s = strings.TrimSpace(s)
	for _, u := range units {
		if num, ok := strings.CutSuffix(s, u.suffix); ok {
			f, err := strconv.ParseFloat(strings.TrimSpace(num), 64)
			// !(f >= 0) also refuses NaN; the product must fit an int64.
			if err != nil || !(f >= 0) || f*float64(u.mult) >= math.MaxInt64 {
				return 0, fmt.Errorf("invalid size %q", s)
			}
			return Size(f * float64(u.mult)), nil
		}
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("invalid size %q (use e.g. 256MiB, 2GiB)", s)
	}
	return Size(n), nil
}

// UnmarshalYAML implements yaml.Unmarshaler.
func (z *Size) UnmarshalYAML(node *yaml.Node) error {
	v, err := ParseSize(node.Value)
	if err != nil {
		return err
	}
	*z = v
	return nil
}

// String renders the size with the largest exact binary unit.
func (z Size) String() string {
	switch {
	case z != 0 && z%GiB == 0:
		return fmt.Sprintf("%dGiB", z/GiB)
	case z != 0 && z%MiB == 0:
		return fmt.Sprintf("%dMiB", z/MiB)
	case z != 0 && z%KiB == 0:
		return fmt.Sprintf("%dKiB", z/KiB)
	}
	return fmt.Sprintf("%dB", int64(z))
}

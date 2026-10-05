//go:build !linux

// The mainnet authority read requires the same descriptor guarantees as the
// Linux verifier. Other platforms never fall back to a blocking special file.
package server

import (
	"context"
	"errors"
	"slices"
)

// BytesBoundedE preserves explicit in-memory overrides on other platforms and
// refuses filesystem authority when bounded descriptor admission is unavailable.
func (self *SimpleResource) BytesBoundedE(ctx context.Context, maximum int) ([]byte, error) {
	if ctx == nil || self == nil || maximum <= 0 || maximum > 16*1024*1024 {
		return nil, errors.New("bounded resource requires owner and a 1–16777216 byte limit")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if self.override != nil && len(self.override) <= maximum {
		result := slices.Clone(self.override)
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return result, nil
	}
	return nil, errors.New("bounded filesystem authority requires Linux descriptor admission")
}

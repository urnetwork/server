// Pooling exact originals preserves independent source selection while bounding
// the combined ordinary and live-open receipts before any decoding or signing.
package controller

import (
	"bytes"
	"context"
	"crypto/sha256"
	"slices"

	"github.com/urfoundation/sn/payoutartifact"
)

// Identical retained bytes occupy one slot. A hash collision is a contradiction;
// another receipt for the same semantic identity remains for the verifier to
// reject, rather than being chosen by arrival order or an SQL latest projection.
func mergeProviderWorkOriginalPools(ctx context.Context, pools ...[][]byte) ([][]byte, error) {
	if ctx == nil {
		return nil, payoutartifact.ErrClosedWorkUnavailable
	}
	seen := map[[32]byte][]byte{}
	result := [][]byte{}
	used := 0
	for _, pool := range pools {
		for _, raw := range pool {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			hash := sha256.Sum256(raw)
			if prior, exists := seen[hash]; exists {
				if !bytes.Equal(prior, raw) {
					return nil, payoutartifact.ErrClosedWorkIntegrity
				}
				continue
			}
			if len(result) >= 32768 || len(raw) > 8*1024*1024-used {
				return nil, payoutartifact.ErrClosedWorkCapacity
			}
			owned := bytes.Clone(raw)
			seen[hash] = owned
			result = append(result, owned)
			used += len(owned)
		}
	}
	slices.SortFunc(result, bytes.Compare)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return result, nil
}

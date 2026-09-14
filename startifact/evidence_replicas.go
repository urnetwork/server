// Replica publication owns one temporary file for one authenticated envelope.
// The file never outlives the synchronous operation or replaces a stored read.
package startifact

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"

	"github.com/urnetwork/server"
)

// PublishAndVerifyReplicas performs both immutable route checks and both
// independent direct readbacks at every ordered store. The finite caller
// census shares one synced staging file, without retaining a file or verdict
// between envelopes. Each invocation has separate file ownership.
func (self *PreparedEvidence) PublishAndVerifyReplicas(ctx context.Context, stores []server.BlobStore) (resultErr error) {
	if ctx == nil || len(stores) == 0 || len(stores) > 128 {
		return errors.New("evidence replica ownership is incomplete or unbounded")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	// Store callbacks may mutate their caller's slice, but cannot redirect
	// later admitted routes or supply another envelope's receipt.
	stores = slices.Clone(stores)
	publications := make([]*Published, len(stores))
	for index, store := range stores {
		published, err := self.publication(store)
		if err != nil {
			return fmt.Errorf("replica %d evidence route: %w", index+1, err)
		}
		publications[index] = published
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	file, err := os.CreateTemp("", "urnetwork-st-artifact-*.json")
	if err != nil {
		return err
	}
	path := file.Name()
	defer func() {
		if file != nil {
			resultErr = errors.Join(resultErr, file.Close())
		}
		resultErr = errors.Join(resultErr, os.Remove(path), ctx.Err())
	}()
	if err := file.Chmod(0o600); err != nil {
		return err
	}
	if _, err := file.Write(self.encoded); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	closeErr := file.Close()
	file = nil
	if closeErr != nil {
		return closeErr
	}
	for index, store := range stores {
		published := publications[index]
		for _, key := range []string{published.ContentKey, published.HistoryKey} {
			if err := ctx.Err(); err != nil {
				return err
			}
			if err := putImmutableFromFile(ctx, store, key, path, self.encoded); err != nil {
				return fmt.Errorf("replica %d evidence publication: %w", index+1, err)
			}
		}
		if err := self.VerifyPublished(ctx, store, published); err != nil {
			return fmt.Errorf("replica %d evidence verification: %w", index+1, err)
		}
	}
	return nil
}

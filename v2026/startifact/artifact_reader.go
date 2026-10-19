// Immutable readback compares every accepted byte against its owned wire with
// fixed storage, then requires end of stream. No replica-sized copy is built.
package startifact

import (
	"bytes"
	"context"
	"errors"
	"io"
)

// The caller owns and closes reader on every result, joining Close failures.
// A mismatched body can be refused early; matching bodies require exact Eof.
func compareArtifactReader(ctx context.Context, reader io.Reader, expected []byte) (matches bool, resultErr error) {
	if ctx == nil || reader == nil {
		return false, errors.New("immutable artifact read owner is missing")
	}
	// Read can cancel its caller while returning mismatched data or an error.
	// Preserve that cancellation at every return, including early refusal.
	defer func() {
		if err := ctx.Err(); err != nil {
			matches = false
			if !errors.Is(resultErr, err) {
				resultErr = errors.Join(resultErr, err)
			}
		}
	}()
	buffer := make([]byte, 64*1024)
	offset, emptyReads := 0, 0
	for {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		remaining := len(expected) - offset
		limit := len(buffer)
		if remaining < limit {
			limit = remaining + 1
		}
		count, readErr := reader.Read(buffer[:limit])
		if count < 0 || count > limit {
			return false, errors.Join(errors.New("immutable artifact reader returned an invalid count"), readErr)
		}
		if count > remaining || !bytes.Equal(buffer[:count], expected[offset:offset+count]) {
			if readErr == io.EOF {
				readErr = nil
			}
			return false, readErr
		}
		offset += count
		if readErr != nil {
			if readErr == io.EOF {
				return offset == len(expected), ctx.Err()
			}
			return false, readErr
		}
		if count == 0 {
			emptyReads++
			if emptyReads >= 100 {
				return false, io.ErrNoProgress
			}
		} else {
			emptyReads = 0
		}
	}
}

// Exact immutable readback is a byte comparison plus an observed terminal
// Eof, with fixed read requests and caller-owned reader lifetime.
package startifact

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
)

type artifactReaderTestFunc func([]byte) (int, error)

func (self artifactReaderTestFunc) Read(value []byte) (int, error) {
	return self(value)
}

func artifactReaderTestBytes(size int) []byte {
	value := make([]byte, size)
	for index := range value {
		value[index] = byte(index*31 + 7)
	}
	return value
}

// Matching a prefix is never sufficient, including exactly at each retained
// buffer edge. A mismatch may stop early, after at most one surplus byte.
func TestCompareArtifactReaderRequiresExactBytesAndEof(t *testing.T) {
	t.Parallel()
	const chunkBytes = 64 * 1024
	for _, size := range []int{0, 1, chunkBytes - 1, chunkBytes, chunkBytes + 1, 2*chunkBytes + 3} {
		for _, step := range []int{1, 31, chunkBytes, chunkBytes + 1} {
			for _, change := range []string{"exact", "truncated", "extra", "different"} {
				if size == 0 && (change == "truncated" || change == "different") {
					continue
				}
				expected := artifactReaderTestBytes(size)
				body := bytes.Clone(expected)
				switch change {
				case "truncated":
					body = body[:len(body)-1]
				case "extra":
					body = append(body, 0xa5, 0x5a)
				case "different":
					body[len(body)-1] ^= 0xff
				}
				source := bytes.NewReader(body)
				readBytes, calls, maximumRequest := 0, 0, 0
				observedEof := false
				reader := artifactReaderTestFunc(func(value []byte) (int, error) {
					calls++
					maximumRequest = max(maximumRequest, len(value))
					count, err := source.Read(value[:min(len(value), step)])
					readBytes += count
					observedEof = observedEof || err == io.EOF
					return count, err
				})
				matches, err := compareArtifactReader(t.Context(), reader, expected)
				if err != nil || matches != (change == "exact") {
					t.Fatalf("immutable bytes comparison changed: size=%d step=%d change=%s matches=%t error=%v", size, step, change, matches, err)
				}
				if matches && !observedEof {
					t.Fatal("matching bytes were accepted without actual end of stream")
				}
				if calls == 0 || maximumRequest > chunkBytes || readBytes > len(expected)+1 {
					t.Fatalf("immutable read exceeded its bounded owner: calls=%d request=%d consumed=%d expected=%d", calls, maximumRequest, readBytes, len(expected))
				}
			}
		}
	}
}

// Reader is allowed to return its last data together with Eof. It still may
// not turn a short or extended body into an exact immutable readback.
func TestCompareArtifactReaderAcceptsFinalDataWithEof(t *testing.T) {
	t.Parallel()
	for _, size := range []int{0, 1, 64*1024 - 1, 64 * 1024, 64*1024 + 1} {
		for _, change := range []string{"exact", "truncated", "extra"} {
			if size == 0 && change == "truncated" {
				continue
			}
			expected := artifactReaderTestBytes(size)
			body := bytes.Clone(expected)
			if change == "truncated" {
				body = body[:len(body)-1]
			} else if change == "extra" {
				body = append(body, 0)
			}
			source := bytes.NewReader(body)
			finalCalls := 0
			reader := artifactReaderTestFunc(func(value []byte) (int, error) {
				count, err := source.Read(value)
				if source.Len() == 0 {
					finalCalls++
					return count, io.EOF
				}
				return count, err
			})
			matches, err := compareArtifactReader(t.Context(), reader, expected)
			if err != nil || matches != (change == "exact") || finalCalls != 1 {
				t.Fatalf("data plus Eof changed exactness: size=%d change=%s matches=%t final=%d error=%v", size, change, matches, finalCalls, err)
			}
		}
	}
}

// Even complete matching data is unauthenticated when its terminal read has
// another error. A wrapped Eof is preserved as an error, as in io.ReadAll.
func TestCompareArtifactReaderPreservesDataAndTerminalErrors(t *testing.T) {
	t.Parallel()
	expected := []byte("owned")
	refused := errors.New("synthetic immutable read refusal")
	for _, readFailure := range []error{refused, io.ErrUnexpectedEOF, fmt.Errorf("wrapped Eof: %w", io.EOF)} {
		for _, body := range [][]byte{nil, []byte("ow"), []byte("owned"), []byte("wrong"), []byte("owned!")} {
			calls := 0
			reader := artifactReaderTestFunc(func(value []byte) (int, error) {
				calls++
				return copy(value, body), readFailure
			})
			matches, err := compareArtifactReader(t.Context(), reader, expected)
			if matches || !errors.Is(err, readFailure) || calls != 1 {
				t.Fatalf("immutable read discarded a terminal error: matches=%t calls=%d error=%v", matches, calls, err)
			}
		}
	}
}

// A broken reader cannot spin forever. The fixture itself terminates at 101
// calls, so removing the production progress guard fails deterministically.
func TestCompareArtifactReaderBoundsAndResetsEmptyProgress(t *testing.T) {
	t.Parallel()
	calls := 0
	exceeded := errors.New("synthetic reader exceeded its allowed empty reads")
	stalled := artifactReaderTestFunc(func([]byte) (int, error) {
		calls++
		if calls > 100 {
			return 0, exceeded
		}
		return 0, nil
	})
	if matches, err := compareArtifactReader(t.Context(), stalled, []byte("a")); matches || !errors.Is(err, io.ErrNoProgress) || calls != 100 {
		t.Fatalf("empty reader escaped the fixed progress bound: matches=%t calls=%d error=%v", matches, calls, err)
	}
	expected := []byte("ab")
	position, emptyReads := 0, 0
	calls = 0
	intermittent := artifactReaderTestFunc(func(value []byte) (int, error) {
		calls++
		if position == len(expected) {
			return 0, io.EOF
		}
		if emptyReads < 99 {
			emptyReads++
			return 0, nil
		}
		value[0] = expected[position]
		position++
		emptyReads = 0
		return 1, nil
	})
	if matches, err := compareArtifactReader(t.Context(), intermittent, expected); !matches || err != nil || calls != 201 {
		t.Fatalf("actual byte progress did not reset the empty-read bound: matches=%t calls=%d error=%v", matches, calls, err)
	}
}

func TestCompareArtifactReaderRejectsMalformedCounts(t *testing.T) {
	t.Parallel()
	refused := errors.New("synthetic invalid-count read refusal")
	for _, expected := range [][]byte{nil, []byte("owned"), artifactReaderTestBytes(64 * 1024)} {
		for _, invalidCount := range []func(int) int{
			func(int) int { return -1 },
			func(request int) int { return request + 1 },
		} {
			for _, readFailure := range []error{nil, refused, io.EOF} {
				calls := 0
				reader := artifactReaderTestFunc(func(value []byte) (int, error) {
					calls++
					return invalidCount(len(value)), readFailure
				})
				matches, err := compareArtifactReader(t.Context(), reader, expected)
				if matches || err == nil || !strings.Contains(err.Error(), "invalid count") || calls != 1 {
					t.Fatalf("malformed reader count acquired authority: matches=%t calls=%d error=%v", matches, calls, err)
				}
				if readFailure != nil && !errors.Is(err, readFailure) {
					t.Fatal("malformed reader count discarded its independent error", err)
				}
			}
		}
	}
}

// Cancellation can occur inside Read, including on an early mismatch or a
// simultaneous physical failure. Neither terminal condition may erase it.
func TestCompareArtifactReaderPreservesCancellationAtEveryReturn(t *testing.T) {
	t.Parallel()
	expected := []byte("owned")
	refused := errors.New("synthetic canceled read refusal")
	for _, value := range []struct {
		name      string
		body      []byte
		terminal  error
		malformed bool
	}{
		{name: "matching Eof", body: expected, terminal: io.EOF},
		{name: "partial Eof", body: []byte("ow"), terminal: io.EOF},
		{name: "matching bytes", body: expected},
		{name: "empty read"},
		{name: "mismatched bytes", body: []byte("wrong")},
		{name: "surplus bytes", body: []byte("owned!")},
		{name: "physical error", body: expected, terminal: refused},
		{name: "mismatched physical error", body: []byte("wrong"), terminal: refused},
		{name: "invalid count", terminal: refused, malformed: true},
	} {
		ctx, cancel := context.WithCancel(t.Context())
		calls := 0
		reader := artifactReaderTestFunc(func(destination []byte) (int, error) {
			calls++
			cancel()
			if value.malformed {
				return -1, value.terminal
			}
			return copy(destination, value.body), value.terminal
		})
		matches, err := compareArtifactReader(ctx, reader, expected)
		cancel()
		if matches || !errors.Is(err, context.Canceled) || calls != 1 {
			t.Fatalf("canceled %s lost its terminal cancellation: matches=%t calls=%d error=%v", value.name, matches, calls, err)
		}
		if value.terminal != nil && value.terminal != io.EOF && !errors.Is(err, value.terminal) {
			t.Fatalf("canceled %s lost the physical read error: %v", value.name, err)
		}
	}
}

// The reader is handed only scratch storage. Even a retained destination
// slice cannot mutate either original wire owner after comparison returns.
func TestCompareArtifactReaderDoesNotAliasBorrowedInputs(t *testing.T) {
	t.Parallel()
	expected := artifactReaderTestBytes(2*64*1024 + 7)
	original := bytes.Clone(expected)
	body := bytes.Clone(expected)
	source := bytes.NewReader(body)
	var destinations [][]byte
	reader := artifactReaderTestFunc(func(value []byte) (int, error) {
		destinations = append(destinations, value)
		return source.Read(value)
	})
	if matches, err := compareArtifactReader(t.Context(), reader, expected); !matches || err != nil {
		t.Fatal("exact borrowed wire did not compare", err)
	}
	if len(destinations) < 3 {
		t.Fatal("borrowed input control did not cross multiple read boundaries")
	}
	for _, destination := range destinations {
		for index := range destination {
			destination[index] = 0
		}
	}
	if !bytes.Equal(expected, original) || !bytes.Equal(body, original) {
		t.Fatal("reader scratch aliases an original immutable wire owner")
	}
}

func TestCompareArtifactReaderRejectsAbsentOrCanceledOwnersBeforeRead(t *testing.T) {
	t.Parallel()
	calls := 0
	reader := artifactReaderTestFunc(func([]byte) (int, error) {
		calls++
		return 0, io.EOF
	})
	if matches, err := compareArtifactReader(nil, reader, nil); matches || err == nil || calls != 0 {
		t.Fatal("missing context reached immutable reader", err)
	}
	if matches, err := compareArtifactReader(t.Context(), nil, nil); matches || err == nil {
		t.Fatal("missing reader acquired immutable authority", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if matches, err := compareArtifactReader(ctx, reader, nil); matches || !errors.Is(err, context.Canceled) || calls != 0 {
		t.Fatal("already canceled owner reached immutable reader", err)
	}
}

type artifactReadCloserTest struct {
	reader io.Reader
	closes int
}

func (self *artifactReadCloserTest) Read(value []byte) (int, error) {
	return self.reader.Read(value)
}

func (self *artifactReadCloserTest) Close() error {
	self.closes++
	return nil
}

func TestCompareArtifactReaderLeavesCloseWithCaller(t *testing.T) {
	t.Parallel()
	expected := []byte("owned")
	reader := &artifactReadCloserTest{reader: bytes.NewReader(expected)}
	if matches, err := compareArtifactReader(t.Context(), reader, expected); !matches || err != nil || reader.closes != 0 {
		t.Fatal("comparison consumed its caller's Close ownership", err)
	}
	if err := reader.Close(); err != nil || reader.closes != 1 {
		t.Fatal("caller could not discharge its exact reader owner", err)
	}
}

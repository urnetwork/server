package sidecart3

// The part reader CaptureStores' Redis keys and Postgres tables stream through (capture_stores.go), with no store: a
// part is made only once the scan has read the one before, a part's error ends the stream with it, and closing a
// stream part way ends its producer.

import (
	"errors"
	"io"
	"iter"
	"testing"
)

// Parts "a", "bb", "ccc", counting how many were made.
func countedParts(made *int, err error) iter.Seq2[[]byte, error] {
	return func(yield func([]byte, error) bool) {
		for _, part := range []string{"a", "bb", "ccc"} {
			*made += 1
			if !yield([]byte(part), nil) {
				return
			}
		}
		if err != nil {
			yield(nil, err)
		}
	}
}

func TestCapturePartReaderMakesAPartOnlyWhenTheScanReachesIt(t *testing.T) {
	made := 0
	reader := newCapturePartReader(countedParts(&made, nil))
	defer reader.Close()
	if made != 0 {
		t.Fatalf("%d parts made before a read", made)
	}
	buffer := make([]byte, 1)
	if n, err := reader.Read(buffer); n != 1 || err != nil || string(buffer[:n]) != "a" {
		t.Fatalf("first read: %d %v %q", n, err, buffer[:n])
	}
	if made != 1 {
		t.Fatalf("%d parts made after the first part's byte", made)
	}
	rest, err := io.ReadAll(reader)
	if err != nil || string(rest) != "bbccc" {
		t.Fatalf("the rest: %q %v", rest, err)
	}
	if made != 3 {
		t.Fatalf("%d parts made", made)
	}
}

func TestCapturePartReaderEndsWithAPartsError(t *testing.T) {
	made := 0
	failure := errors.New("the store did not answer")
	reader := newCapturePartReader(countedParts(&made, failure))
	defer reader.Close()
	read, err := io.ReadAll(reader)
	if !errors.Is(err, failure) || string(read) != "abbccc" {
		t.Fatalf("read %q, %v", read, err)
	}
	// the error stays, so a scan reading again finds the stream failed, never empty
	if n, err := reader.Read(make([]byte, 8)); n != 0 || !errors.Is(err, failure) {
		t.Fatalf("a read after the failure: %d %v", n, err)
	}
}

func TestCapturePartReaderClosedPartWayEndsItsProducer(t *testing.T) {
	made := 0
	reader := newCapturePartReader(countedParts(&made, nil))
	if _, err := reader.Read(make([]byte, 1)); err != nil {
		t.Fatal(err)
	}
	reader.Close()
	if made != 1 {
		t.Fatalf("%d parts made by a stream closed after its first", made)
	}
}

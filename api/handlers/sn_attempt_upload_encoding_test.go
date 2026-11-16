// An upload body's encoding and lengths, decided before any quota, body read or
// storage: the object's own length bounds everything, the wire may not carry
// more, and zstd decodes to exactly the declared length or fails.
package handlers

import (
	"bytes"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/klauspost/compress/zstd"
)

// One object a records upload would carry: compressible NDJSON.
func snAttemptUploadEncodingTestObject(size int) []byte {
	var object bytes.Buffer
	for index := 0; object.Len() < size; index++ {
		object.WriteString(`{"sequence":` + strconv.Itoa(index) + `,"disposition":"complete"}` + "\n")
	}
	return object.Bytes()[:size]
}

func snAttemptUploadEncodingTestZstd(t testing.TB, object []byte) []byte {
	t.Helper()
	encoder, err := zstd.NewWriter(nil, zstd.WithEncoderConcurrency(1))
	if err != nil {
		t.Fatal(err)
	}
	defer encoder.Close()
	return encoder.EncodeAll(object, nil)
}

func snAttemptUploadEncodingTestRequest(wire []byte, encodings []string, decoded []string) *http.Request {
	request := httptest.NewRequest(http.MethodPost, "/sn/attempt-artifact", bytes.NewReader(wire))
	for _, encoding := range encodings {
		request.Header.Add("Content-Encoding", encoding)
	}
	for _, length := range decoded {
		request.Header.Add(snAttemptUploadDecodedLengthHeader, length)
	}
	return request
}

// An object as it is keeps one positive bounded Content-Length, and a zstd
// body names its object's length, within the bound and no shorter than the
// wire. The answer to another encoding is 415.
func TestSnAttemptUploadLengthsDecideBeforeTheBody(t *testing.T) {
	t.Parallel()
	const limit = 1024
	for _, c := range []struct {
		name      string
		wire      int
		encodings []string
		decoded   []string
		size      uint64
		encoded   bool
		status    string
	}{
		{name: "as it is", wire: 100, size: 100},
		{name: "as it is at the bound", wire: limit, size: limit},
		{name: "as it is over the bound", wire: limit + 1, status: "413"},
		{name: "a decoded length without an encoding", wire: 100, decoded: []string{"100"}, status: "400"},
		{name: "zstd", wire: 40, encodings: []string{"zstd"}, decoded: []string{"100"}, size: 100, encoded: true},
		{name: "zstd named in capitals", wire: 40, encodings: []string{"ZSTD"}, decoded: []string{"100"}, size: 100, encoded: true},
		{name: "zstd at the bound", wire: 40, encodings: []string{"zstd"}, decoded: []string{strconv.Itoa(limit)}, size: limit, encoded: true},
		{name: "zstd over the bound", wire: 40, encodings: []string{"zstd"}, decoded: []string{strconv.Itoa(limit + 1)}, status: "413"},
		{name: "zstd larger than its object", wire: 101, encodings: []string{"zstd"}, decoded: []string{"100"}, status: "400"},
		{name: "zstd without its length", wire: 40, encodings: []string{"zstd"}, status: "400"},
		{name: "zstd with two lengths", wire: 40, encodings: []string{"zstd"}, decoded: []string{"100", "100"}, status: "400"},
		{name: "zstd with a zero length", wire: 40, encodings: []string{"zstd"}, decoded: []string{"0"}, status: "400"},
		{name: "zstd with a padded length", wire: 40, encodings: []string{"zstd"}, decoded: []string{"0100"}, status: "400"},
		{name: "zstd with a signed length", wire: 40, encodings: []string{"zstd"}, decoded: []string{"+100"}, status: "400"},
		{name: "zstd with a spaced length", wire: 40, encodings: []string{"zstd"}, decoded: []string{" 100"}, status: "400"},
		{name: "gzip", wire: 40, encodings: []string{"gzip"}, decoded: []string{"100"}, status: "415"},
		{name: "zstd then gzip", wire: 40, encodings: []string{"zstd, gzip"}, decoded: []string{"100"}, status: "415"},
		{name: "two encodings", wire: 40, encodings: []string{"zstd", "zstd"}, decoded: []string{"100"}, status: "415"},
	} {
		request := snAttemptUploadEncodingTestRequest(make([]byte, c.wire), c.encodings, c.decoded)
		size, wireSize, encoded, err := snAttemptUploadLengths(request, limit)
		if c.status != "" {
			if err == nil || !strings.HasPrefix(err.Error(), c.status+" ") {
				t.Fatalf("%s: answered %v, want %s", c.name, err, c.status)
			}
			if c.status == "415" && err != errSnAttemptUploadEncodingUnsupported {
				t.Fatalf("%s: the refusal lost the advertisement", c.name)
			}
			continue
		}
		if err != nil || size != c.size || wireSize != uint64(c.wire) || encoded != c.encoded {
			t.Fatalf("%s: size=%d wire=%d encoded=%t err=%v", c.name, size, wireSize, encoded, err)
		}
	}
	empty := snAttemptUploadEncodingTestRequest(nil, nil, nil)
	empty.ContentLength = 0
	if _, _, _, err := snAttemptUploadLengths(empty, limit); err == nil || !strings.HasPrefix(err.Error(), "413 ") {
		t.Fatalf("an empty body: %v", err)
	}
}

// A zstd body decodes to exactly its declared object. One that would decode to
// more stops at the declared length instead of growing, and one that decodes
// to less, or is not zstd, fails.
func TestSnAttemptUploadDecodesExactlyTheDeclaredObject(t *testing.T) {
	t.Parallel()
	object := snAttemptUploadEncodingTestObject(256 * 1024)
	wire := snAttemptUploadEncodingTestZstd(t, object)
	if 4*len(wire) > len(object) {
		t.Fatalf("the test object did not compress: %d of %d", len(wire), len(object))
	}
	decoded, err := decodeSnAttemptUploadBody(wire, uint64(len(object)))
	if err != nil || !bytes.Equal(decoded, object) {
		t.Fatalf("an exact body did not decode: %v", err)
	}
	if _, err := decodeSnAttemptUploadBody(wire, uint64(len(object)-1)); err == nil {
		t.Fatal("a body longer than its declared object decoded")
	}
	if _, err := decodeSnAttemptUploadBody(wire, uint64(len(object)+1)); err == nil {
		t.Fatal("a body shorter than its declared object decoded")
	}
	if _, err := decodeSnAttemptUploadBody(object[:1024], 1024); err == nil {
		t.Fatal("bytes that are not zstd decoded")
	}
	if _, err := decodeSnAttemptUploadBody(append(bytes.Clone(wire), 0x00), uint64(len(object))); err == nil {
		t.Fatal("a body with trailing bytes decoded")
	}
	for _, size := range []uint64{0, max(maximumSnAttemptMetadataBytes, maximumSnAttemptRecordBytes, maximumSnAttemptProofBytes) + 1} {
		if _, err := decodeSnAttemptUploadBody(wire, size); err == nil {
			t.Fatalf("a declared length of %d decoded", size)
		}
	}
}

// A small frame that declares far more than its object, the shape of a
// decompression bomb, fails at the declared length.
func TestSnAttemptUploadDecodeStopsAtTheDeclaredLength(t *testing.T) {
	t.Parallel()
	bomb := snAttemptUploadEncodingTestZstd(t, make([]byte, 4*1024*1024))
	if 64*1024 < len(bomb) {
		t.Fatalf("the bomb is %d bytes", len(bomb))
	}
	_, err := decodeSnAttemptUploadBody(bomb, 1024)
	if err == nil {
		t.Fatal("a bomb decoded")
	}
	if !errors.Is(err, zstd.ErrDecoderSizeExceeded) && !errors.Is(err, zstd.ErrWindowSizeExceeded) && !strings.Contains(err.Error(), "differs") {
		t.Fatalf("the bomb failed for another reason: %v", err)
	}
}

// Objects of every size an uploader sends, from below zstd's smallest window
// to the largest records object, decode when written as one whole frame the
// way the validator's encoder writes them.
func TestSnAttemptUploadDecodesObjectsOfEverySize(t *testing.T) {
	t.Parallel()
	encoder, err := zstd.NewWriter(nil, zstd.WithEncoderConcurrency(1), zstd.WithEncoderLevel(zstd.SpeedDefault))
	if err != nil {
		t.Fatal(err)
	}
	defer encoder.Close()
	for _, size := range []int{1, 100, 1023, 1024, 1025, 64*1024 + 3, 1024*1024 + 7, int(maximumSnAttemptRecordBytes) - 1000, int(maximumSnAttemptRecordBytes)} {
		object := snAttemptUploadEncodingTestObject(size)
		decoded, err := decodeSnAttemptUploadBody(encoder.EncodeAll(object, nil), uint64(size))
		if err != nil || !bytes.Equal(decoded, object) {
			t.Fatalf("a %d byte object did not decode: %v", size, err)
		}
	}
}

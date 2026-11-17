package usagearchive

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"math"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"
)

// The digest sum is exact past int64 in both directions, as PostgreSQL's
// numeric sum is.
func TestDigestSumIsExact(t *testing.T) {
	values := []int64{math.MaxInt64, math.MaxInt64, math.MaxInt64, -1, math.MinInt64, math.MinInt64, 7, math.MinInt64, math.MinInt64, math.MinInt64, math.MinInt64, 3}
	var sum digestSum
	var want big.Int
	for i, value := range values {
		sum.Add(value)
		want.Add(&want, big.NewInt(value))
		if got := sum.String(); got != want.String() {
			t.Fatalf("after %d values the sum is %s, want %s", i+1, got, want.String())
		}
	}
	if (&digestSum{}).String() != "0" {
		t.Fatal("the empty sum is not 0")
	}
}

// A line's digest is the first 8 bytes of its MD5, big-endian and signed.
func TestLineDigest(t *testing.T) {
	// md5("") = d41d8cd98f00b204e9800998ecf8427e
	if got := LineDigest(nil); got != int64(-0x2be2732670ff4dfc) {
		t.Fatalf("the digest of the empty line is %d", got)
	}
}

const testLines = "018f0000-0000-7000-8000-000000000001\tsettled\t2026-06-01 10:00:00\t{\"a\": \"b\\\\\\\\c\"}\n" +
	"018f0000-0000-7000-8000-000000000002\tsettled\t\\N\t\\N\n" +
	"018f0000-0000-7000-8000-000000000003\tdispute_resolved_to_source\t2026-06-01 10:00:00.5\t{\"byte_count\": 3}\n"

func testFigures(t *testing.T, stream string, chunk int) *streamFigures {
	figures := &streamFigures{}
	data := []byte(stream)
	for 0 < len(data) {
		n := min(chunk, len(data))
		figures.Write(data[:n])
		data = data[n:]
	}
	return figures
}

// The figures do not depend on how the stream is cut into writes.
func TestStreamFiguresAcrossWrites(t *testing.T) {
	whole := testFigures(t, testLines, len(testLines))
	if err := whole.Close(); err != nil {
		t.Fatal(err)
	}
	if whole.Rows != 3 || whole.NullCloseRows != 1 ||
		whole.MinContractId != "018f0000-0000-7000-8000-000000000001" || whole.MaxContractId != "018f0000-0000-7000-8000-000000000003" {
		t.Fatalf("figures %+v", whole)
	}
	var want digestSum
	for _, line := range strings.Split(strings.TrimSuffix(testLines, "\n"), "\n") {
		want.Add(LineDigest([]byte(line)))
	}
	if whole.Digest() != want.String() {
		t.Fatalf("digest %s, want %s", whole.Digest(), want.String())
	}
	for _, chunk := range []int{1, 2, 3, 7, 36, 37, 64} {
		cut := testFigures(t, testLines, chunk)
		if err := cut.Close(); err != nil {
			t.Fatal(err)
		}
		if cut.Rows != whole.Rows || cut.Digest() != whole.Digest() || cut.NullCloseRows != whole.NullCloseRows ||
			cut.MinContractId != whole.MinContractId || cut.MaxContractId != whole.MaxContractId {
			t.Fatalf("writes of %d bytes read %+v, not %+v", chunk, cut, whole)
		}
	}
}

// A repeated id, ids out of order, a short line, or a stream cut inside a line
// are refused.
func TestStreamFiguresRefusesMalformedStreams(t *testing.T) {
	lines := strings.Split(strings.TrimSuffix(testLines, "\n"), "\n")
	for name, stream := range map[string]string{
		"repeated id":  lines[0] + "\n" + lines[0] + "\n",
		"out of order": lines[1] + "\n" + lines[0] + "\n",
		"short line":   "018f0000-0000-7000-8000-000000000001\tsettled\n",
		"cut short":    testLines[:len(testLines)-5],
		"no id":        "x\tsettled\t\\N\t\\N\n",
	} {
		figures := testFigures(t, stream, 5)
		if err := figures.Close(); err == nil {
			t.Fatalf("a stream with a %s was accepted", name)
		}
	}
}

func TestWeekLeafNames(t *testing.T) {
	week, ok := WeekOfLeaf("st_provider_usage_archive_w20260601")
	if !ok || !week.Equal(time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("week %s %t", week, ok)
	}
	if LeafOfWeek(week) != "st_provider_usage_archive_w20260601" {
		t.Fatal("the leaf of the week differs")
	}
	for _, name := range []string{"st_provider_usage_archive_w20260602", "st_provider_usage_archive_default", "st_provider_usage_archive_restore", "transfer_contract_w20260601"} {
		if _, ok := WeekOfLeaf(name); ok {
			t.Fatalf("%s is a week leaf", name)
		}
	}
}

// The floor is never later than 70 days before now.
func TestEffectiveCloseTimeFloor(t *testing.T) {
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	if got := EffectiveCloseTimeFloor(DefaultCloseTimeFloor, now); !got.Equal(DefaultCloseTimeFloor) {
		t.Fatalf("the default floor is %s", got)
	}
	if got := EffectiveCloseTimeFloor(now, now); !got.Equal(now.Add(-70 * 24 * time.Hour)) {
		t.Fatalf("a floor of now is %s", got)
	}
	// before 2026-11-10 the 70 days bind the default floor
	early := time.Date(2026, 11, 3, 0, 0, 0, 0, time.UTC)
	if got := EffectiveCloseTimeFloor(DefaultCloseTimeFloor, early); !got.Equal(time.Date(2026, 8, 25, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("on 11-03 the default floor is %s", got)
	}
}

// More than a quarter of the newest contracts live keeps the week; an empty
// week stays.
func TestReaperProgressBlocker(t *testing.T) {
	for _, test := range []struct {
		live, total int64
		stays       bool
	}{
		{0, 0, true}, {0, 1000, false}, {250, 1000, false}, {251, 1000, true}, {1, 4, false}, {2, 6, true}, {1000, 1000, true},
	} {
		if stays := ReaperProgressBlocker(test.live, test.total) != ""; stays != test.stays {
			t.Fatalf("%d live of %d: stays %t", test.live, test.total, stays)
		}
	}
}

// A stored copy written as the export writes it verifies against its manifest;
// one byte changed in the file, a different stream, or a manifest that claims
// other figures do not.
func TestVerifyCopy(t *testing.T) {
	dir := t.TempDir()
	write := func(name string, stream string) (*Manifest, string) {
		var compressed bytes.Buffer
		encoder, err := zstd.NewWriter(&compressed)
		if err != nil {
			t.Fatal(err)
		}
		encoder.Write([]byte(stream))
		encoder.Close()
		path := filepath.Join(dir, name+".copy.zst")
		if err := os.WriteFile(path, compressed.Bytes(), 0o640); err != nil {
			t.Fatal(err)
		}
		figures := testFigures(t, stream, len(stream))
		copySum := sha256.Sum256([]byte(stream))
		fileSum := sha256.Sum256(compressed.Bytes())
		manifest := &Manifest{
			Format:          ManifestFormat,
			Leaf:            "st_provider_usage_archive_w20260601",
			WeekStart:       "2026-06-01",
			LowerContractId: "018f0000-0000-0000-0000-000000000000",
			UpperContractId: "018fffff-ffff-0000-0000-000000000000",
			Rows:            figures.Rows,
			NullCloseRows:   figures.NullCloseRows,
			MinContractId:   figures.MinContractId,
			MaxContractId:   figures.MaxContractId,
			RowDigest:       figures.Digest(),
			CopySha256:      hex.EncodeToString(copySum[:]),
			CopyBytes:       int64(len(stream)),
			File:            name + ".copy.zst",
			FileSha256:      hex.EncodeToString(fileSum[:]),
			FileBytes:       int64(compressed.Len()),
		}
		return manifest, path
	}
	manifest, path := write("a", testLines)
	if err := VerifyCopy(path, manifest); err != nil {
		t.Fatal(err)
	}
	manifestPath := ManifestPathOf(path)
	if err := WriteManifest(manifestPath, manifest); err != nil {
		t.Fatal(err)
	}
	if _, err := VerifyManifest(manifestPath); err != nil {
		t.Fatal(err)
	}

	// a manifest that claims another row count or digest
	other := *manifest
	other.Rows++
	if err := VerifyCopy(path, &other); err == nil {
		t.Fatal("a manifest with another row count verified")
	}
	other = *manifest
	other.RowDigest = "1"
	if err := VerifyCopy(path, &other); err == nil {
		t.Fatal("a manifest with another digest verified")
	}
	// rows outside the week's bounds
	other = *manifest
	other.UpperContractId = "018f0000-0000-7000-8000-000000000002"
	if err := VerifyCopy(path, &other); err == nil {
		t.Fatal("a copy with rows past the week verified")
	}
	// one byte of the file
	data, _ := os.ReadFile(path)
	data[len(data)-3] ^= 0x10
	os.WriteFile(path, data, 0o640)
	if err := VerifyCopy(path, manifest); err == nil {
		t.Fatal("a copy with a changed byte verified")
	}
	// the same name, another stream
	_, otherPath := write("b", strings.Replace(testLines, "settled", "settles", 1))
	renamed := filepath.Join(dir, "a.copy.zst")
	os.Rename(otherPath, renamed)
	if err := VerifyCopy(renamed, manifest); err == nil {
		t.Fatal("another stream verified")
	}
}

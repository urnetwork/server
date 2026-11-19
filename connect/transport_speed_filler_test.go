package connect

import (
	"bytes"
	"compress/flate"
	"sync"
	"testing"
)

// A probe is filler: what matters is that it does not compress and that two
// connections do not send the same bytes.
func TestSpeedTestFillerProbesAreDistinct(t *testing.T) {
	a := newSpeedTestFiller()
	b := newSpeedTestFiller()
	first := make([]byte, speedTestProbeByteCount)
	second := make([]byte, speedTestProbeByteCount)
	other := make([]byte, speedTestProbeByteCount)
	if n, err := a.Read(first); n != len(first) || err != nil {
		t.Fatalf("read = %d, %v; want the whole probe", n, err)
	}
	a.Read(second)
	b.Read(other)
	if bytes.Equal(first, second) {
		t.Fatal("one writer repeated a probe")
	}
	if bytes.Equal(first, other) {
		t.Fatal("two writers sent the same probe")
	}
	// what matters to the test it serves: a compressing link gains nothing
	var packed bytes.Buffer
	packer, err := flate.NewWriter(&packed, flate.BestCompression)
	if err != nil {
		t.Fatal(err)
	}
	packer.Write(first)
	packer.Close()
	if packed.Len() < len(first) {
		t.Fatalf("a %d-byte probe compressed to %d bytes", len(first), packed.Len())
	}
}

// Each writer owns its generator, so writers filling at once share no state.
// Run under the race detector.
func TestSpeedTestFillersShareNoState(t *testing.T) {
	const writerCount = 8
	firsts := make([][]byte, writerCount)
	lasts := make([][]byte, writerCount)
	var wg sync.WaitGroup
	for i := range writerCount {
		wg.Add(1)
		go func() {
			defer wg.Done()
			filler := newSpeedTestFiller()
			chunk := make([]byte, speedTestProbeByteCount)
			for j := range 512 {
				filler.Read(chunk)
				if j == 0 {
					firsts[i] = bytes.Clone(chunk)
				}
			}
			lasts[i] = chunk
		}()
	}
	wg.Wait()
	for i := range writerCount {
		if bytes.Equal(firsts[i], lasts[i]) {
			t.Fatalf("writer %d ended a test on the probe it began with", i)
		}
		for j := range i {
			if bytes.Equal(firsts[i], firsts[j]) || bytes.Equal(lasts[i], lasts[j]) {
				t.Fatalf("writers %d and %d produced the same probe", j, i)
			}
		}
	}
}

package connect

import (
	"testing"

	"github.com/urnetwork/server/model"
)

// A probe of the running test is counted and reported, so the edge reader ends
// it there. Nothing else is: a probe of another test, a Transfer frame of the
// probe's size, a short or long frame, and any frame outside a test all
// continue to the resident uncounted.
func TestCountSpeedTestProbeEchoTakesOnlyTheRunningTestsProbes(t *testing.T) {
	const testId = 7
	probe := make([]byte, speedTestProbeByteCount)
	setSpeedTestProbeHeader(probe, testId)
	otherProbe := make([]byte, speedTestProbeByteCount)
	setSpeedTestProbeHeader(otherProbe, testId+1)
	// a Transfer frame begins with a nonzero protobuf tag
	transferFrame := make([]byte, speedTestProbeByteCount)
	transferFrame[0] = 0x0a
	short := append([]byte(nil), probe[:speedTestProbeByteCount-1]...)
	long := append(append([]byte(nil), probe...), 0)

	speedTest := &SpeedTest{TestId: testId}
	for name, message := range map[string][]byte{
		"probe of another test":        otherProbe,
		"transfer frame of probe size": transferFrame,
		"one byte short of a probe":    short,
		"one byte longer than a probe": long,
		"empty frame":                  nil,
	} {
		if countSpeedTestProbeEcho(speedTest, message) {
			t.Fatalf("%s was taken as a probe of the running test", name)
		}
	}
	if speedTest.TotalByteCount != 0 {
		t.Fatalf("frames that are not probes counted %d bytes", speedTest.TotalByteCount)
	}

	for i := 1; i <= 3; i += 1 {
		if !countSpeedTestProbeEcho(speedTest, probe) {
			t.Fatal("a probe of the running test was not taken")
		}
		if want := model.ByteCount(i * speedTestProbeByteCount); speedTest.TotalByteCount != want {
			t.Fatalf("after %d probes the test counted %d bytes, want %d", i, speedTest.TotalByteCount, want)
		}
	}

	if countSpeedTestProbeEcho(nil, probe) {
		t.Fatal("a probe outside a test was taken")
	}
}

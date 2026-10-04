package server

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/maxmind/mmdbwriter/mmdbtype"
)

// Linux accounts bytes read even for sparse files. This checks that an
// oversized resource is rejected by stat before hashing or copying its bytes,
// without allocating a giant test fixture or depending on wall-clock timing.
func shadowResourceReadBytes(t *testing.T) uint64 {
	t.Helper()
	data, err := os.ReadFile("/proc/self/io")
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[0] == "rchar:" {
			n, err := strconv.ParseUint(fields[1], 10, 64)
			if err != nil {
				t.Fatal(err)
			}
			return n
		}
	}
	t.Fatal("Linux process read counter unavailable")
	return 0
}

func TestArinShadowResourceBudgetsRejectBeforeReading(t *testing.T) {
	data := testExceptionDatabase(t, string(schemaTypeArinDb), map[string]mmdbtype.Map{
		"192.0.2.0/24": {"classifier_version": mmdbtype.Uint32(1), "quality_policy_version": mmdbtype.Uint32(2), "quality_state": mmdbtype.String("subscriber"), "non_quality": mmdbtype.Bool(false), "risk": mmdbtype.Bool(false)},
	})
	dir := t.TempDir()
	valid := filepath.Join(dir, "valid.mmdb")
	if err := os.WriteFile(valid, data, 0600); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(data)
	pin := hex.EncodeToString(digest[:])
	for _, mode := range []struct {
		name   string
		mapped bool
		limit  int64
	}{{"snapshot", false, arinShadowSnapshotFileLimit}, {"capture", true, arinShadowCaptureFileLimit}} {
		t.Run(mode.name, func(t *testing.T) {
			// Exercise successful decoding on the same entry point first.
			recorder, err := openArinShadowRecorder(valid, pin, valid, pin, time.Now(), 1, mode.mapped)
			if err != nil {
				t.Fatal("valid resource rejected", err)
			}
			recorder.Close()
			oversized := filepath.Join(dir, mode.name+"-oversized.mmdb")
			f, err := os.Create(oversized)
			if err != nil {
				t.Fatal(err)
			}
			if err := f.Truncate(mode.limit + 1); err != nil {
				f.Close()
				t.Fatal(err)
			}
			if err := f.Close(); err != nil {
				t.Fatal(err)
			}
			for _, side := range []string{"active", "candidate"} {
				t.Run(side, func(t *testing.T) {
					active, candidate := valid, valid
					if side == "active" {
						active = oversized
					} else {
						candidate = oversized
					}
					before := shadowResourceReadBytes(t)
					recorder, err := openArinShadowRecorder(active, pin, candidate, pin, time.Now(), 1, mode.mapped)
					after := shadowResourceReadBytes(t)
					if recorder != nil {
						recorder.Close()
					}
					if recorder != nil || err != ErrArinShadowInput {
						t.Fatal("oversized resource accepted")
					}
					if after-before > 1<<20 {
						t.Fatal("oversized resource was read before rejection")
					}
				})
			}
		})
	}
}

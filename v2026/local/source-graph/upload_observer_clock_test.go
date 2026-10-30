// Release source selection must execute the staging observation clock controls.
package sourcegraph

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// A compiling older pin can refuse healthy heads minted during observer reads.
// Require the locked dependency's actual refresh and admission regressions.
func TestReleaseSourceGraphChecksUploadObservationClock(t *testing.T) {
	requireReviewedCheckout(t, "sn", "urfoundation/sn")
	command := exec.CommandContext(t.Context(), "go", "test", "-mod=readonly", "-p", "2", "-json", "./validator", "-run", "^TestValidatorUploadAdmission(UsesClockAfterObserverReads|ObserverClockKeepsFreshnessAndCancellation)$", "-count=1", "-timeout=5m")
	command.Dir = filepath.Join(serverDirectory(t), "..", "sn")
	command.Env = append(os.Environ(), "GOWORK=off", "GOTOOLCHAIN=local", "GOENV=off", "CGO_ENABLED=0", "GOFLAGS=-mod=readonly", "GOPROXY=off", "GOSUMDB=off")
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("locked staging observation clock controls failed: %v\n%s", err, output)
	}
	passed := make(map[string]bool)
	decoder := json.NewDecoder(bytes.NewReader(output))
	for {
		var event struct {
			Action string
			Test   string
		}
		if err := decoder.Decode(&event); err != nil {
			if err == io.EOF {
				break
			}
			t.Fatalf("decode locked staging observation clock result: %v\n%s", err, output)
		}
		if event.Action == "pass" {
			passed[event.Test] = true
		}
	}
	for _, name := range []string{"TestValidatorUploadAdmissionUsesClockAfterObserverReads", "TestValidatorUploadAdmissionObserverClockKeepsFreshnessAndCancellation"} {
		if !passed[name] {
			t.Errorf("locked staging dependency must execute and pass %s; missing or skipped controls cannot qualify a release", name)
		}
	}
}

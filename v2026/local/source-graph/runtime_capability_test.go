// Release source selection must retain the staging capability and its refusals.
package sourcegraph

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// A compiling old SN pin can still refuse the current runtime. Run the locked
// dependency's actual capability tests and require their execution explicitly.
func TestReleaseSourceGraphAdmitsReviewedRuntime486(t *testing.T) {
	requireReviewedCheckout(t, "sn", "urfoundation/sn")
	command := exec.CommandContext(t.Context(), "go", "test", "-mod=readonly", "-p", "2", "-json", "./crv4", "-run", "^TestRuntime(481|482|483|484|486)", "-count=1", "-timeout=10m")
	command.Dir = filepath.Join(serverDirectory(t), "..", "sn")
	command.Env = append(os.Environ(), "GOWORK=off", "GOTOOLCHAIN=local", "GOENV=off", "CGO_ENABLED=0", "GOFLAGS=-mod=readonly", "GOPROXY=off", "GOSUMDB=off")
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("locked SN runtime capability tests failed: %v\n%s", err, output)
	}
	passedTestNames := map[string]bool{}
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
			t.Fatalf("decode locked SN capability test result: %v\n%s", err, output)
		}
		if event.Action == "pass" {
			passedTestNames[event.Test] = true
		}
	}
	requiredTestNames := []string{
		"TestRuntime486ValidatorProducerCapability",
		"TestRuntime486SelectiveMetagraphApiRejectsForeignIdentity",
		"TestRuntime486OwnerSourceAndCatalogBoundary",
		"TestRuntime486ConsumedMetadataInterfaces",
	}
	for _, spec := range []uint32{481, 482, 483, 484, 486} {
		requiredTestNames = append(requiredTestNames, fmt.Sprintf("TestRuntime%dSelectiveMetagraphApiAdmission", spec))
	}
	for _, name := range requiredTestNames {
		if !passedTestNames[name] {
			t.Errorf("locked SN must execute and pass %s; absent or skipped tests cannot qualify the release", name)
		}
	}
}

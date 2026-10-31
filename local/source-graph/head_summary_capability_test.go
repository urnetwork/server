// Service consumers require the signed v2 evidence envelope, not merely a
// sibling revision whose older head-summary package still compiles by itself.
package sourcegraph

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Execute the reviewed sibling's real signing, recipient/census refusal and
// empty-prior controls; absent or skipped capability tests cannot pass the lock.
func TestReleaseSourceGraphAdmitsHeadSummaryV2(t *testing.T) {
	requireReviewedCheckout(t, "sn", "urfoundation/sn")
	requiredTestNames := []string{
		"TestEvidenceSummarySignsExactCutAndCensus",
		"TestEvidenceSummaryRejectsMissingDuplicateOrUnscopedEvidence",
		"TestEvidenceSummaryAuthenticatesEmptyPrior",
		"TestEvidenceSummaryAllowsExplicitEmptyEligibleCensus",
	}
	pattern := "^(" + strings.Join(requiredTestNames, "|") + ")$"
	command := exec.CommandContext(t.Context(), "go", "test", "-mod=readonly", "-p", "2", "-json", "./protocol/headsummary", "-run", pattern, "-count=1", "-timeout=5m")
	command.Dir = filepath.Join(serverDirectory(t), "..", "sn")
	command.Env = append(os.Environ(), "GOWORK=off", "GOTOOLCHAIN=local", "GOENV=off", "CGO_ENABLED=0", "GOFLAGS=-mod=readonly", "GOPROXY=off", "GOSUMDB=off")
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("locked head-summary v2 capability tests failed: %v\n%s", err, output)
	}
	passedTestNames := map[string]bool{}
	packagePassed := false
	decoder := json.NewDecoder(bytes.NewReader(output))
	for {
		var event struct {
			Action  string
			Package string
			Test    string
		}
		if err := decoder.Decode(&event); err != nil {
			if err == io.EOF {
				break
			}
			t.Fatalf("decode locked head-summary v2 capability result: %v\n%s", err, output)
		}
		if event.Package == "github.com/urfoundation/sn/protocol/headsummary" && event.Action == "pass" {
			if event.Test == "" {
				packagePassed = true
			} else {
				passedTestNames[event.Test] = true
			}
		}
	}
	if !packagePassed {
		t.Error("locked head-summary package must explicitly pass its capability controls")
	}
	for _, name := range requiredTestNames {
		if !passedTestNames[name] {
			t.Errorf("locked SN must execute and pass %s; missing or skipped controls cannot qualify the release", name)
		}
	}
}

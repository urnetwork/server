// SPDX-License-Identifier: MPL-2.0

package onboarding

import (
	"errors"
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"
)

// The server image (Dockerfile) is alpine with no zoneinfo, so a service
// resolves an IANA zone only from Go's embedded database (server/tzdata.go).
// Without it every LoadLocation falls back to UTC: onboarding and
// re-engagement mail was placed by UTC hours instead of the network's,
// auth-client time zones were never stored, and the monitor could not read
// Play's date-times.
func TestServiceBinariesEmbedTimeZoneDatabase(t *testing.T) {
	mains := []string{
		"github.com/urnetwork/server/cli/all",
		"github.com/urnetwork/server/cli/alt",
		"github.com/urnetwork/server/cli/api",
		"github.com/urnetwork/server/cli/competitionworker",
		"github.com/urnetwork/server/cli/connect",
		"github.com/urnetwork/server/cli/gossip",
		"github.com/urnetwork/server/cli/mcp",
		"github.com/urnetwork/server/cli/monitor",
		"github.com/urnetwork/server/cli/proxy",
		"github.com/urnetwork/server/cli/taskworker",
		"github.com/urnetwork/server/bringyourctl",
	}
	list := exec.Command("go", append([]string{"list", "-f", `{{.ImportPath}} {{join .Deps " "}}`}, mains...)...)
	// the image's build (Dockerfile): linux, no cgo
	list.Env = append(os.Environ(), "GOOS=linux", "CGO_ENABLED=0")
	out, err := list.Output()
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			t.Fatalf("go list: %v\n%s", err, exit.Stderr)
		}
		t.Fatalf("go list: %v", err)
	}
	listed := 0
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		listed++
		if !slices.Contains(fields[1:], "time/tzdata") {
			t.Errorf("%s does not embed time/tzdata: on the image its LoadLocation falls back to UTC", fields[0])
		}
	}
	if listed != len(mains) {
		t.Errorf("go list listed %d binaries, want %d", listed, len(mains))
	}
}

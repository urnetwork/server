// Run the actual generated command with a local argv-faithful curl stub.
// These controls are hermetic and never start SSH, curl networking or Mimir.
package monitor

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// Only the Host seam owns a real local child; all other transports stay fake.
type pgDiskSpaceNativeTestSource struct {
	*syntheticSource
	directory string
	response  string
	status    int
}

// The original caller context reaches the emitted native shell directly.
func (self *pgDiskSpaceNativeTestSource) Host(ctx context.Context, _ HostSettings, command string) (string, error) {
	child := exec.CommandContext(ctx, "/bin/sh", "-c", command)
	child.Env = append(os.Environ(),
		"PATH="+self.directory,
		"PG_DISK_TEST_RESPONSE="+self.response,
		"PG_DISK_TEST_STATUS="+strconv.Itoa(self.status),
		"PG_DISK_TEST_TIME=time="+strconv.FormatInt(syntheticSettings(nil).Now().Unix(), 10),
	)
	output, err := child.CombinedOutput()
	if err != nil {
		return string(output), fmt.Errorf("synthetic command failed: %w", err)
	}
	return string(output), nil
}

// Successful and failing producers emit the same plausible complete low-space
// prefix. Only the successful producer can establish the absolute predicate.
func TestPgDiskSpaceNativeProducerStatusPrecedesValues(t *testing.T) {
	directory := t.TempDir()
	stub := `#!/bin/sh
[ "$#" = 10 ] || exit 97
[ "$1" = '-fsS' ] || exit 97
[ "$2" = '--max-time' ] && [ "$3" = 15 ] || exit 97
[ "$4" = '--max-filesize' ] && [ "$5" = 65536 ] || exit 97
[ "$6" = '--data-urlencode' ] || exit 97
[ "$7" = 'query={__name__=~"node_filesystem_(avail|size)_bytes",env="synthetic",job="node",host="pg-1",mountpoint="/"}[90s]' ] || exit 97
[ "$8" = '--data-urlencode' ] && [ "$9" = "$PG_DISK_TEST_TIME" ] || exit 97
[ "${10}" = 'http://127.0.0.1:3100/prometheus/api/v1/query' ] || exit 97
/bin/cat "$PG_DISK_TEST_RESPONSE" || exit 98
if [ "$PG_DISK_TEST_STATUS" != 0 ]; then printf '%s\n' 'private-native-source-canary' >&2; fi
exit "$PG_DISK_TEST_STATUS"
`
	if err := os.WriteFile(filepath.Join(directory, "curl"), []byte(stub), 0o700); err != nil {
		t.Fatal("could not create native stub")
	}
	response := filepath.Join(directory, "response.private.json")
	for _, test := range []struct {
		status    int
		oversized bool
	}{
		{status: 0}, {status: 1}, {status: 22}, {status: 28}, {status: 63}, {status: 124}, {status: 127}, {status: 137}, {status: 0, oversized: true},
	} {
		body := pgDiskSpaceTestBody(t, "1", nil)
		if test.oversized {
			body += strings.Repeat(" ", pgDiskSpaceMaxBytes)
		}
		if err := os.WriteFile(response, []byte(body), 0o600); err != nil {
			t.Fatal("could not create native response")
		}
		settings := pgDiskSpaceTestSettings(t, "", nil)
		settings.Source = &pgDiskSpaceNativeTestSource{syntheticSource: &syntheticSource{}, directory: directory, response: response, status: test.status}
		alerts, err := NewPgDiskSpaceSignal().Run(context.Background(), settings)
		if err != nil || len(alerts) != 1 {
			t.Fatal("native producer control did not return one bounded observation")
		}
		class := "pg-disk-space-unobservable"
		if test.status == 0 && !test.oversized {
			class = "pg-disk-space-low"
		}
		alert := requireAlertClass(t, alerts, class)
		if alert.unobservable != (class == "pg-disk-space-unobservable") {
			t.Fatal("native status lost its visibility authority")
		}
		requireAlertOmits(t, alert, "private-native-source-canary", "/dev/synthetic-root")
	}
}

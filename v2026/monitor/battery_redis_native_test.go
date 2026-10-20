// Replay the actual optional collector under a closed set of local stubs.
// These tests never reach real Redis, SSH, sockets or production configuration.
package monitor

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Capture status and stdout from the actual generated shell. Only awk and tr
// are admitted utilities; every source, range generator and clock is a stub.
func redisBatteryNativeFixture(t *testing.T, memory, clients, stats, latency, failure string) (string, error) {
	t.Helper()
	directory := t.TempDir()
	for _, name := range []string{"awk", "tr"} {
		path, err := exec.LookPath(name)
		if err != nil {
			t.Fatal("required hermetic fixture utility is unavailable")
		}
		if err := os.Symlink(path, filepath.Join(directory, name)); err != nil {
			t.Fatal("could not install hermetic fixture utility")
		}
	}
	redisScript := `#!/bin/sh
[ "$1" = --raw ] && [ "$2" = -p ] && [ "$3" = 6382 ] || exit 126
shift 3
case "$*" in
  'CLIENT LIST') printf '%s\n' "$BATTERY_FIXTURE_CLIENTS"; [ "$BATTERY_FIXTURE_FAILURE" != clients ] || exit 1 ;;
  'INFO memory') printf '%s\n' "$BATTERY_FIXTURE_MEMORY"; [ "$BATTERY_FIXTURE_FAILURE" != memory ] || exit 1 ;;
  'INFO clients') printf '# Clients\nblocked_clients:0\n'; [ "$BATTERY_FIXTURE_FAILURE" != clients-info ] || exit 1 ;;
  'INFO commandstats') printf '%s\n' "$BATTERY_FIXTURE_STATS"; [ "$BATTERY_FIXTURE_FAILURE" != commandstats ] || exit 1 ;;
  'CLUSTER KEYSLOT client_reliability_stats_blocks') printf '123\n' ;;
  'CLUSTER NODES') printf 'synthetic-node 192.0.2.7:6382@16382 myself,master - 0 1 1 connected 0-16383\n' ;;
  '--latency')
    printf '%s\n' "$BATTERY_FIXTURE_LATENCY"
    [ "$BATTERY_FIXTURE_FAILURE" != latency ] || exit 1
    [ "$BATTERY_FIXTURE_FAILURE" != latency-child124 ] || exit 124
    ;;
  '')
    while IFS= read -r command; do
      case "$command" in 'CLUSTER KEYSLOT client_reliability_stats.'*) printf '123\n' ;; *) exit 126 ;; esac
    done
    ;;
  *) exit 126 ;;
esac
`
	for name, script := range map[string]string{
		"redis-cli": redisScript,
		"seq": `#!/bin/sh
[ "$#" -eq 2 ] || exit 126
case "$BATTERY_FIXTURE_FAILURE:$1" in
  seq-empty:*) exit 0 ;;
  seq-blocks:99|seq-shards:0) exit 1 ;;
  seq-partial:99) printf '99\n'; exit 0 ;;
esac
value=$1
while [ "$value" -le "$2" ]; do
  printf '%s\n' "$value"
  value=$((value + 1))
done
[ "$BATTERY_FIXTURE_FAILURE" != seq-status-after-output ] || exit 1
`,
		"date":    "#!/bin/sh\n[ \"$*\" = '+%s' ] || exit 126\nprintf '6000\\n'\n",
		"timeout": "#!/bin/sh\n[ \"$1\" = 2 ] || exit 126\nshift\nexec \"$@\"\n",
		"ss":      "#!/bin/sh\n[ \"$*\" = '-lnt' ] || exit 126\nprintf 'State Recv-Q Send-Q Local Peer\\nLISTEN 0 128 192.0.2.7:6382 192.0.2.0:*\\n'\n[ \"$BATTERY_FIXTURE_FAILURE\" != queue ]\n",
	} {
		if name == "seq" && failure == "seq-missing" {
			continue
		}
		if err := os.WriteFile(filepath.Join(directory, name), []byte(script), 0700); err != nil {
			t.Fatal("could not install hermetic source stub")
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	command := redisAuthorityNativeCommand(ctx, redisConnectionBatteryCommand(6382))
	defer redisAuthorityKillNativeGroup(command)
	command.Env = []string{
		"PATH=" + directory, "LC_ALL=C", "BATTERY_FIXTURE_MEMORY=" + memory,
		"BATTERY_FIXTURE_CLIENTS=" + clients, "BATTERY_FIXTURE_STATS=" + stats,
		"BATTERY_FIXTURE_LATENCY=" + latency, "BATTERY_FIXTURE_FAILURE=" + failure,
	}
	output, err := command.Output()
	return string(output), err
}

// Source metadata is discarded before crossing the remote boundary, while
// source failures and malformed numeric INFO remain unavailable. The public
// connection warning is retained in every case.
func TestRedisOptionalBatteryNativeSourceAuthority(t *testing.T) {
	memory := "# Memory\nused_memory:1000000\nmem_clients_normal:1000\nused_memory_human:" + redisBatteryPrivateCanary
	clients := "id=1 addr=192.0.2.9:1234 name=" + redisBatteryPrivateCanary + " user=" + redisBatteryPrivateCanary + " lib-name=" + redisBatteryPrivateCanary + " omem=0 age=60 idle=0 cmd=hget"
	stats := "# Commandstats\ncmdstat_hget:calls=100,usec=100,usec_per_call=1.0,rejected_calls=0,failed_calls=0"
	for _, fixture := range []struct {
		memory  string
		clients string
		stats   string
		latency string
		failure string
		valid   bool
	}{
		{memory: memory, clients: clients, stats: stats, latency: "0 1 0.4 10", valid: true},
		{memory: memory, clients: clients, stats: stats, latency: "0 1 0.4 10", failure: "latency-child124", valid: true},
		{memory: memory, clients: clients, stats: stats, latency: "min: 0, max: 1, avg: 0.4 (10 samples)", valid: true},
		{memory: memory + "\nfuture_field:" + redisBatteryPrivateCanary, clients: clients, stats: stats, latency: "0 1 0.4 10", valid: true},
		{memory: memory, clients: clients + " client_list_total=999 hget_calls_per_nominal_second=999", stats: stats, latency: "0 1 0.4 10", valid: true},
		{memory: memory, clients: clients, stats: "# Commandstats", latency: "0 1 0.4 10", valid: true},
		{memory: "# Memory\nused_memory:1000000", clients: clients, stats: stats, latency: "0 1 0.4 10"},
		{memory: memory + "\nmem_clients_normal:0", clients: clients, stats: stats, latency: "0 1 0.4 10"},
		{memory: strings.Replace(memory, "mem_clients_normal:1000", "mem_clients_normal:"+redisBatteryPrivateCanary, 1), clients: clients, stats: stats, latency: "0 1 0.4 10"},
		{memory: memory, clients: strings.Replace(clients, "omem=0", "omem=-1", 1), stats: stats, latency: "0 1 0.4 10"},
		{memory: memory, clients: "", stats: stats, latency: "0 1 0.4 10"},
		{memory: memory, clients: clients, stats: "# Commandstats\ncmdstat_hget:usec=100", latency: "0 1 0.4 10"},
		{memory: memory, clients: clients, stats: stats, latency: "0 1 NaN 10"},
		{memory: memory, clients: clients, stats: stats, latency: "0 1 0.4 10", failure: "memory"},
		{memory: memory, clients: clients, stats: stats, latency: "0 1 0.4 10", failure: "clients"},
		{memory: memory, clients: clients, stats: stats, latency: "0 1 0.4 10", failure: "clients-info"},
		{memory: memory, clients: clients, stats: stats, latency: "0 1 0.4 10", failure: "commandstats"},
		{memory: memory, clients: clients, stats: stats, latency: "0 1 0.4 10", failure: "latency"},
		{memory: memory, clients: clients, stats: stats, latency: "0 1 0.4 10", failure: "queue"},
		{memory: memory, clients: clients, stats: stats, latency: "0 1 0.4 10", failure: "seq-missing"},
		{memory: memory, clients: clients, stats: stats, latency: "0 1 0.4 10", failure: "seq-blocks"},
		{memory: memory, clients: clients, stats: stats, latency: "0 1 0.4 10", failure: "seq-shards"},
		{memory: memory, clients: clients, stats: stats, latency: "0 1 0.4 10", failure: "seq-empty"},
		{memory: memory, clients: clients, stats: stats, latency: "0 1 0.4 10", failure: "seq-partial"},
		{memory: memory, clients: clients, stats: stats, latency: "0 1 0.4 10", failure: "seq-status-after-output"},
	} {
		output, nativeErr := redisBatteryNativeFixture(t, fixture.memory, fixture.clients, fixture.stats, fixture.latency, fixture.failure)
		if (nativeErr == nil) != fixture.valid {
			t.Fatal("native optional-source status violated its completeness contract")
		}
		if strings.Contains(output, redisBatteryPrivateCanary) {
			t.Fatal("native optional projection transferred arbitrary source metadata")
		}
		if fixture.valid {
			diagnostic, valid := parseRedisConnectionDiagnostics(output)
			if !valid || diagnostic.values["client_list_total"] != 1 || diagnostic.values["client_cmd_hget_count"] != 1 || diagnostic.hgetNominalRate != 0 {
				t.Fatal("native healthy source lost its bounded numeric projection")
			}
		}
		source := &syntheticSource{hostFn: func(_ HostSettings, command string) (string, error) {
			if strings.Contains(command, "CLIENT LIST") {
				if nativeErr != nil {
					return output, fmt.Errorf("%s: %w", redisBatteryPrivateCanary, nativeErr)
				}
				return output, nil
			}
			return "6380 10 100 9 0 10\n6381 10 100 9 0 10\n6382 10 100 9 0 55", nil
		}}
		alerts, err := NewRedisConnectionsSignal().Run(context.Background(), syntheticSettings(source))
		if err != nil {
			t.Fatal("optional source failure erased the underlying connection observation")
		}
		redisBatteryAssertPublicPrivate(t, alerts)
		alert := redisBatteryPressureAlert(t, alerts, "clients-spike")
		if !fixture.valid && !strings.Contains(alert.Evidence, "unavailable: error_class=") {
			t.Fatal("failed native optional source was reported as complete")
		}
		if fixture.valid && (!strings.Contains(alert.Evidence, "hget_nominal_sample_seconds=2") ||
			!strings.Contains(alert.Evidence, "hget_calls_per_nominal_second=0") ||
			strings.Contains(alert.Evidence, "hget_sample_seconds=") || strings.Contains(alert.Evidence, "hget_calls_per_second=") ||
			!strings.Contains(alert.Context, "elapsed interval and Redis generation were not measured")) {
			t.Fatal("immediate or child-owned latency exit acquired measured-rate authority")
		}
	}
}

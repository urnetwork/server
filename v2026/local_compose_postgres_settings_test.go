package server

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// The launcher-managed local postgres and the disposable release-gate postgres
// are two fixtures for the same suites. The monitor's native query-sample tests
// refuse to run unless pg_stat_activity keeps at least 16kB of each statement,
// so a launcher started with the 1kB default fails those tests closed on every
// developer machine while the release gate passes them.

// Returns the `-c name=value` server settings from a postgres argv.
func localPostgresServerSettings(t *testing.T, argv []string) map[string]string {
	t.Helper()
	settings := map[string]string{}
	for i := 0; i < len(argv); i++ {
		if argv[i] != "-c" {
			continue
		}
		if i+1 >= len(argv) {
			t.Fatalf("postgres argv ends with a bare -c: %q", argv)
		}
		name, value, ok := strings.Cut(argv[i+1], "=")
		if !ok || name == "" || value == "" {
			t.Fatalf("malformed postgres setting %q", argv[i+1])
		}
		if _, duplicate := settings[name]; duplicate {
			t.Fatalf("duplicate postgres setting %q", name)
		}
		settings[name] = value
		i++
	}
	return settings
}

func localComposePostgresSettings(t *testing.T) map[string]string {
	t.Helper()
	content, err := os.ReadFile(filepath.Join("local", "docker-compose.yml"))
	if err != nil {
		t.Fatal(err)
	}
	var compose struct {
		Services map[string]struct {
			Command []string `yaml:"command"`
		} `yaml:"services"`
	}
	if err := yaml.Unmarshal(content, &compose); err != nil {
		t.Fatal(err)
	}
	postgres, ok := compose.Services["postgres"]
	if !ok || len(postgres.Command) == 0 || postgres.Command[0] != "postgres" {
		t.Fatalf("local compose postgres command = %q; want a postgres argv", postgres.Command)
	}
	return localPostgresServerSettings(t, postgres.Command[1:])
}

func releaseGatePostgresSettings(t *testing.T) map[string]string {
	t.Helper()
	content, err := os.ReadFile(filepath.Join("local", "release-gate-services.sh"))
	if err != nil {
		t.Fatal(err)
	}
	matches := regexp.MustCompile(`"\$image" postgres((?: -c [^ )]+)+)\)`).FindAllStringSubmatch(string(content), -1)
	if len(matches) != 1 {
		t.Fatalf("release gate postgres argv matches = %d; want exactly one", len(matches))
	}
	return localPostgresServerSettings(t, strings.Fields(matches[0][1]))
}

func TestLocalComposePostgresKeepsNativeQuerySampleStatementText(t *testing.T) {
	value := localComposePostgresSettings(t)["track_activity_query_size"]
	if value != "16kB" {
		t.Fatalf("local compose track_activity_query_size = %q; want 16kB, the native query-sample fixture minimum", value)
	}
}

func TestLocalComposePostgresMatchesReleaseGateSettings(t *testing.T) {
	local := localComposePostgresSettings(t)
	gate := releaseGatePostgresSettings(t)
	for name, value := range gate {
		if local[name] != value {
			t.Errorf("local compose %s = %q; release gate uses %q", name, local[name], value)
		}
	}
	for name, value := range local {
		if _, ok := gate[name]; !ok {
			t.Errorf("local compose sets %s=%q, which the release gate does not", name, value)
		}
	}
}

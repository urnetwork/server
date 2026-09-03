package monitor

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

const releaseBuilderTestRevision = "0123456789abcdef0123456789abcdef01234567"

func releaseBuilderFixture(sample releaseBuilderSample) string {
	return fmt.Sprintf(
		"path %s\nrevision %s\nmodified %s\n",
		sample.path, sample.revision, sample.modified,
	)
}

func healthyReleaseBuilderSample(path string) releaseBuilderSample {
	return releaseBuilderSample{
		path: path, revision: releaseBuilderTestRevision, modified: "false",
	}
}

func TestReleaseBuilderSignalSyntheticHealthyFleet(t *testing.T) {
	var lock sync.Mutex
	hostCalls := map[string]int{}
	localCalls := 0
	source := &syntheticSource{
		localFn: func(name string, args ...string) (string, error) {
			localCalls++
			if name != "sh" || len(args) != 2 || args[0] != "-c" || !strings.Contains(args[1], releaseBuilderMarker) {
				return "", fmt.Errorf("unexpected local release-builder command: %s %v", name, args)
			}
			return releaseBuilderFixture(healthyReleaseBuilderSample("/local/warpctl")), nil
		},
		hostFn: func(host HostSettings, command string) (string, error) {
			lock.Lock()
			hostCalls[host.Name]++
			lock.Unlock()
			if !strings.Contains(command, releaseBuilderMarker) ||
				!strings.Contains(command, "release_builder_path=/usr/local/sbin/warpctl") {
				return "", errors.New("unexpected host release-builder command")
			}
			return releaseBuilderFixture(healthyReleaseBuilderSample("/usr/local/sbin/warpctl")), nil
		},
	}
	settings := syntheticSettings(source)
	settings.Hosts = []HostSettings{
		{Name: "edge-0", Roles: []string{"services"}},
		{Name: "edge-1", Roles: []string{"services"}},
		{Name: "redis-only", Roles: []string{"redis-cluster"}},
	}

	alerts, err := NewReleaseBuilderSignal().Run(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	if len(alerts) != 0 {
		t.Fatalf("healthy release builders alerted: %+v", alerts)
	}
	if localCalls != 1 || hostCalls["edge-0"] != 1 || hostCalls["edge-1"] != 1 || hostCalls["redis-only"] != 0 {
		t.Fatalf("wrong release-builder scope: local=%d hosts=%+v", localCalls, hostCalls)
	}
}

func TestReleaseBuilderSignalSyntheticAllowsIntentionalDirtyLocalCheckout(t *testing.T) {
	source := &syntheticSource{
		localFn: func(string, ...string) (string, error) {
			return releaseBuilderFixture(releaseBuilderSample{
				path: "/local/warpctl", revision: strings.Repeat("a", 40), modified: "true",
			}), nil
		},
		hostFn: func(HostSettings, string) (string, error) {
			return releaseBuilderFixture(healthyReleaseBuilderSample("/usr/local/sbin/warpctl")), nil
		},
	}
	settings := syntheticSettings(source)
	settings.Hosts = []HostSettings{{Name: "edge-0", Roles: []string{"services"}}}

	alerts, err := NewReleaseBuilderSignal().Run(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	if len(alerts) != 0 {
		t.Fatalf("intentional dirty local checkout alerted: %+v", alerts)
	}

}

func TestReleaseBuilderSignalSyntheticMalformedIdentity(t *testing.T) {
	source := &syntheticSource{
		localFn: func(string, ...string) (string, error) {
			return releaseBuilderFixture(releaseBuilderSample{
				path: "/local/warpctl", revision: "short", modified: "unknown",
			}), nil
		},
		hostFn: func(HostSettings, string) (string, error) {
			return releaseBuilderFixture(healthyReleaseBuilderSample("/usr/local/sbin/warpctl")), nil
		},
	}
	settings := syntheticSettings(source)
	settings.Hosts = []HostSettings{{Name: "edge-0", Roles: []string{"services"}}}

	alerts, err := NewReleaseBuilderSignal().Run(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	if len(alerts) != 1 {
		t.Fatalf("release-builder alerts = %d, want 1: %+v", len(alerts), alerts)
	}
	invalid := requireAlertClass(t, alerts, "warpctl-provenance-invalid")
	if invalid.SignalNumber != "8.13" || invalid.SignalKey != "release-builder" {
		t.Fatalf("wrong release-builder identity: %+v", invalid)
	}
	for _, want := range []string{
		"monitor-host", "revision=short", "modified=unknown",
		"warp/warpctl/Makefile", "xops/main/ansible/run-edges.sh",
		"intentional local-checkout workflow", "Do not substitute a published or cached Warpctl",
		"modified=true by itself is intentional", "Validate each deployed service artifact independently",
	} {
		if !strings.Contains(invalid.Markdown(), want) {
			t.Errorf("invalid-provenance alert lacks %q: %s", want, invalid.Markdown())
		}
	}
	for _, rejected := range []string{"Stop release builds", "217392e", "release guard"} {
		if strings.Contains(invalid.Markdown(), rejected) {
			t.Errorf("invalid-provenance alert retains withdrawn policy %q: %s", rejected, invalid.Markdown())
		}
	}
}

func TestMonitorGuidanceDoesNotTurnProvenanceIntoBuildGate(t *testing.T) {
	for _, path := range []string{"SIGNALS.md", "task_checks.go", "tailer.go"} {
		contents, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(strings.ToLower(string(contents)), "provenance-gate") {
			t.Errorf("%s describes artifact observation as a build gate", path)
		}
	}
}

func TestReleaseBuilderSignalPreservesPartialObservationFailure(t *testing.T) {
	source := &syntheticSource{
		localFn: func(string, ...string) (string, error) {
			return releaseBuilderFixture(healthyReleaseBuilderSample("/local/warpctl")), nil
		},
		hostFn: func(host HostSettings, _ string) (string, error) {
			if host.Name == "unreachable" {
				return "", errors.New("synthetic SSH timeout")
			}
			return releaseBuilderFixture(healthyReleaseBuilderSample("/usr/local/sbin/warpctl")), nil
		},
	}
	settings := syntheticSettings(source)
	settings.Hosts = []HostSettings{
		{Name: "healthy", Roles: []string{"services"}},
		{Name: "unreachable", Roles: []string{"services"}},
	}

	alerts, err := NewReleaseBuilderSignal().Run(context.Background(), settings)
	if err != nil {
		t.Fatalf("partial host failure discarded results: %v", err)
	}
	if len(alerts) != 1 {
		t.Fatalf("partial release-builder alerts = %d, want 1: %+v", len(alerts), alerts)
	}
	visibility := requireAlertClass(t, alerts, "cannot-observe")
	if visibility.Target != "unreachable/warpctl-provenance" {
		t.Fatalf("visibility target = %q", visibility.Target)
	}
}

func TestReleaseBuilderScriptReadsEmbeddedLocalCheckoutIdentityWithoutGuards(t *testing.T) {
	binDir := t.TempDir()
	warpctlPath := filepath.Join(binDir, "warpctl")
	body := strings.Join([]string{
		"build\tvcs.revision=" + releaseBuilderTestRevision,
		"build\tvcs.modified=true",
	}, "\n") + "\n"
	if err := os.WriteFile(warpctlPath, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}

	command := exec.Command("sh", "-c", "# "+releaseBuilderMarker+"\n"+releaseBuilderScript)
	command.Env = append(os.Environ(), "PATH="+binDir+":"+os.Getenv("PATH"))
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("release-builder script: %v\n%s", err, output)
	}
	sample, err := parseReleaseBuilderSample(string(output))
	if err != nil {
		t.Fatalf("parse release-builder script: %v\n%s", err, output)
	}
	if sample.path != warpctlPath || sample.revision != releaseBuilderTestRevision ||
		sample.modified != "true" {
		t.Fatalf("release-builder script lost embedded values: %+v\n%s", sample, output)
	}
}

func TestParseReleaseBuilderSampleRejectsMissingField(t *testing.T) {
	_, err := parseReleaseBuilderSample("path /usr/local/sbin/warpctl\nrevision abc\n")
	if err == nil || !strings.Contains(err.Error(), "observation omitted modified") {
		t.Fatalf("missing release-builder field error = %v", err)
	}
}

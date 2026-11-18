// Native proof predicates are tested with synthetic manager bytes. These tests
// never install units, inspect real attempts or manufacture cleanup markers.
package competitionagent

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// Property ordering is not authority; completeness and exact values are.
func legacyPrelaunchTestProjection(values map[string]string) []byte {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	var content strings.Builder
	for _, key := range keys {
		content.WriteString(key + "=" + values[key] + "\n")
	}
	return []byte(content.String())
}

func TestLegacyPrelaunchNativeUnitProofRequiresPositiveDistinctDenials(t *testing.T) {
	authorization := LegacyPrelaunchAuthorization{RunId: strings.Repeat("a", 64), OwnerStartDenial: Pin{Path: "/synthetic/owner-denial.conf"}}
	owner := dockerOwnerUnitName(authorization.RunId)
	evaluator := unitName(authorization.RunId)
	control := managementSshUnitName(strings.Repeat("b", 64))
	for _, role := range []string{"owner", "evaluator", "control"} {
		name, load, fragment, dropIns, refuse := control, "not-found", "", "", "no"
		if role == "owner" {
			name, load, fragment, dropIns, refuse = owner, "loaded", "/run/systemd/transient/"+owner, authorization.OwnerStartDenial.Path, "yes"
		} else if role == "evaluator" {
			name, load, fragment = evaluator, "masked", "/etc/systemd/system/"+evaluator
		}
		values := map[string]string{"Id": name, "LoadState": load, "ActiveState": "inactive", "SubState": "dead", "FragmentPath": fragment, "DropInPaths": dropIns,
			"Job": "", "MainPID": "0", "ControlPID": "0", "ControlGroup": "", "RefuseManualStart": refuse, "NeedDaemonReload": "no"}
		if err := validateLegacyPrelaunchUnit(legacyPrelaunchTestProjection(values), name, role, authorization); err != nil {
			t.Fatal("exact positive unit class refused", role, err)
		}
		for _, mutation := range []struct{ key, value string }{{key: "Job", value: "17"}, {key: "MainPID", value: "9"}, {key: "ControlPID", value: "8"},
			{key: "ActiveState", value: "activating"}, {key: "SubState", value: "running"}, {key: "NeedDaemonReload", value: "yes"}, {key: "ControlGroup", value: "/foreign.slice/owned.service"},
			{key: "LoadState", value: "error"}, {key: "FragmentPath", value: "/synthetic/foreign.service"}, {key: "DropInPaths", value: "/synthetic/unapproved.conf"}} {
			prior := values[mutation.key]
			values[mutation.key] = mutation.value
			if validateLegacyPrelaunchUnit(legacyPrelaunchTestProjection(values), name, role, authorization) == nil {
				t.Fatal("uncertain unit class became a closure proof", role, mutation.key)
			}
			values[mutation.key] = prior
		}
		delete(values, "Job")
		if validateLegacyPrelaunchUnit(legacyPrelaunchTestProjection(values), name, role, authorization) == nil {
			t.Fatal("incomplete manager projection became proof", role)
		}
	}
}

func TestLegacyPrelaunchNativeMissingOrSyntheticOwnerCannotStandInForStop(t *testing.T) {
	authorization := LegacyPrelaunchAuthorization{RunId: strings.Repeat("a", 64), OwnerStartDenial: Pin{Path: "/synthetic/owner-denial.conf"}}
	for _, role := range []string{"owner", "evaluator"} {
		name := unitName(authorization.RunId)
		if role == "owner" {
			name = dockerOwnerUnitName(authorization.RunId)
		}
		for _, load := range []string{"not-found", "masked", "loaded"} {
			values := map[string]string{"Id": name, "LoadState": load, "ActiveState": "inactive", "SubState": "dead", "FragmentPath": "", "DropInPaths": "",
				"Job": "", "MainPID": "0", "ControlPID": "0", "ControlGroup": "", "RefuseManualStart": "no", "NeedDaemonReload": "no"}
			if validateLegacyPrelaunchUnit(legacyPrelaunchTestProjection(values), name, role, authorization) == nil {
				t.Fatal("bare absence/mask/loaded state substituted for distinct launch-closure authority", role, load)
			}
		}
	}
}

func TestLegacyPrelaunchClosedRoleTemplateCannotReplaceOrdinaryRecovery(t *testing.T) {
	policy := managementTestPolicy(DefaultManagementBoundarySettings())
	settings := DefaultAgentSettings()
	config := Config{Agent: Pin{Path: "/synthetic/recovery"}, configPath: "/synthetic/authorization.json"}
	legacy, err := RenderLegacyPrelaunchRecoveryService(config.Agent.Path, config.configPath, policy, settings)
	if err != nil || !bytes.Contains(legacy, []byte("--close-legacy-prelaunch")) {
		t.Fatal("legacy role render failed", err)
	}
	if validateRecoveryServiceText(legacy, config, &policy, settings) == nil || validateRecoveryServiceTextForRole(legacy, config, &policy, settings, "--close-legacy-prelaunch") != nil {
		t.Fatal("explicit legacy template broadened ordinary recovery authority")
	}
	ordinary, err := RenderRecoveryService(config.Agent.Path, config.configPath, policy, settings)
	if err != nil || bytes.Contains(ordinary, []byte("--close-legacy-prelaunch")) || validateRecoveryServiceText(ordinary, config, &policy, settings) != nil {
		t.Fatal("ordinary recovery template or guard changed", err)
	}
	if _, err := renderRecoveryServiceForRole(config.Agent.Path, config.configPath, policy, settings, "--cleanup=synthetic"); err == nil {
		t.Fatal("legacy rendering granted an arbitrary executable role")
	}
}

func TestLegacyPrelaunchRecoveryBlocksLateExecuteDuringRealCleanup(t *testing.T) {
	fixture, agent, authorization, path, digest := newLegacyPrelaunchTestFixture(t)
	entered, release := make(chan struct{}), make(chan struct{})
	fixture.containment.before = func(ctx context.Context, operation string, _ cleanupPlan, _ bool) error {
		if operation == "quiesce" {
			close(entered)
			select {
			case <-release:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		return nil
	}
	done := make(chan error, 1)
	go func() {
		done <- agent.closeLegacyPrelaunch(t.Context(), path, authorization, digest, &legacyPrelaunchTestHost{})
	}()
	<-entered
	executeErr := fixture.agent.Execute(t.Context(), authorization.RunId)
	close(release)
	if err := <-done; err != nil || executeErr == nil {
		t.Fatal("late execution crossed retained recovery custody", executeErr, err)
	}
	if _, err := os.Stat(filepath.Join(fixture.config.WorkDirectory, authorization.RunId, "process-invocations.txt")); !errors.Is(err, os.ErrNotExist) || fixture.runner.launchCount() != 1 {
		t.Fatal("legacy closure ran or relaunched candidate work", err)
	}
}

func TestLegacyPrelaunchChangedFenceDuringCleanupCannotPublishClosure(t *testing.T) {
	fixture, agent, authorization, path, digest := newLegacyPrelaunchTestFixture(t)
	entered, release := make(chan struct{}), make(chan struct{})
	fixture.containment.before = func(ctx context.Context, operation string, _ cleanupPlan, _ bool) error {
		if operation == "quiesce" {
			close(entered)
			select {
			case <-release:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		return nil
	}
	done := make(chan error, 1)
	go func() {
		done <- agent.closeLegacyPrelaunch(t.Context(), path, authorization, digest, &legacyPrelaunchTestHost{})
	}()
	<-entered
	_, statusErr := fixture.call(t, "status", 2, nil)
	close(release)
	if err := <-done; err == nil || statusErr != nil {
		t.Fatal("changed custody generation published an old closure", statusErr, err)
	}
	state, err := agent.loadState(authorization.RunId)
	if err != nil || !state.RecoveryActive || state.FencingToken != 2 || state.State != "accepted" || fixture.runner.launchCount() != 1 {
		t.Fatal("fence change erased retained original uncertainty", state, err)
	}
}

func TestLegacyPrelaunchExecutablePinsDoNotInheritDocumentByteLimit(t *testing.T) {
	root := t.TempDir()
	settings := DefaultAgentSettings()
	settings.ConfigBytes = 1024
	large := filepath.Join(root, "synthetic-sshd")
	if err := os.WriteFile(large, bytes.Repeat([]byte{'x'}, int(settings.ConfigBytes)+1), 0500); err != nil {
		t.Fatal(err)
	}
	document := filepath.Join(root, "synthetic-document")
	if err := os.WriteFile(document, []byte("synthetic protected metadata"), 0400); err != nil {
		t.Fatal(err)
	}
	largePin, documentPin := testAgentPin(t, large), testAgentPin(t, document)
	authorization := LegacyPrelaunchAuthorization{Sshd: largePin, OriginalConfig: documentPin, OriginalRecoveryServiceArchive: documentPin, IngressWrapper: documentPin,
		OriginalIngressWrapperArchive: documentPin, AuthorizedKeys: documentPin, OwnerStartDenial: documentPin}
	if err := validateLegacyPrelaunchIngressPins(authorization, settings, false); err != nil {
		t.Fatal("genuine larger executable inherited the document limit", err)
	}
	authorization.OriginalConfig = largePin
	if validateLegacyPrelaunchIngressPins(authorization, settings, false) == nil {
		t.Fatal("executable support silently widened the document limit")
	}
}

func TestLegacyPrelaunchSshProofRejectsCachedControlAndPreauthGenerations(t *testing.T) {
	authorization := LegacyPrelaunchAuthorization{ControlUser: "synthetic-control", Sshd: Pin{Path: "/synthetic/sshd"}}
	for _, description := range []string{"sshd: synthetic-control [priv]", "sshd: synthetic-control@notty", "sshd: unknown [priv]", "sshd: unknown [net]",
		"sshd: synthetic-operator [preauth]", "sshd: [accepted]", "sshd: /synthetic/sshd -D [listener]"} {
		if legacyPrelaunchSshGeneration([]byte(description+"\x00"), authorization.Sshd.Path, nil, 12, 11, authorization) == nil {
			t.Fatal("old or uncertain SSH option generation permitted closure", description)
		}
	}
	for _, description := range []string{"sshd: synthetic-operator [priv]", "sshd: synthetic-operator@notty", "sshd: synthetic-operator@pts/0"} {
		if err := legacyPrelaunchSshGeneration([]byte(description+"\x00"), authorization.Sshd.Path, nil, 12, 11, authorization); err != nil {
			t.Fatal("independent authenticated operator connection was interrupted", err)
		}
	}
	if err := legacyPrelaunchSshGeneration([]byte("sshd: /synthetic/sshd -D [listener]\x00"), authorization.Sshd.Path, nil, 11, 11, authorization); err != nil {
		t.Fatal("exact native listener generation refused", err)
	}
	if legacyPrelaunchSshGeneration([]byte("sshd: synthetic-operator [priv]\x00"), "/synthetic/unknown-sshd", nil, 12, 11, authorization) == nil {
		t.Fatal("unknown SSH executable supplied cached-route closure proof")
	}
}

func TestLegacyPrelaunchLauncherProofIncludesRetiredRecordedAndUnknownGenerations(t *testing.T) {
	targets := map[string]bool{"/synthetic/current/agent": true, "/synthetic/current/config.json": true, "/synthetic/retired/agent": true,
		"/synthetic/retired/config.json": true, "/synthetic/recorded/agent": true, "/synthetic/forced-wrapper": true}
	for target := range targets {
		if validateLegacyPrelaunchLauncher([]byte("systemd-run\x00--unit=synthetic\x00"+target+"\x00"), "/synthetic/systemd-run", targets) == nil ||
			validateLegacyPrelaunchLauncher([]byte("disguised-argv\x00"), target, targets) == nil {
			t.Fatal("prior/recorded/config/wrapper issuer escaped launch-surface closure", target)
		}
	}
	if validateLegacyPrelaunchLauncher([]byte("disguised-argv\x00"), "/synthetic/unknown-generation/competitionsubeval", targets) == nil {
		t.Fatal("unknown agent generation escaped closure through changed argv")
	}
	if err := validateLegacyPrelaunchLauncher([]byte("synthetic healthy sibling\x00"), "/synthetic/independent-operator", targets); err != nil {
		t.Fatal("independent non-launch-capable operator was included in the fence", err)
	}
}

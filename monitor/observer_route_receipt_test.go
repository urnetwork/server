package monitor

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func observerReceiptSettings(t *testing.T, local func(string, ...string) (string, error)) SignalSettings {
	t.Helper()
	s := syntheticSettings(&syntheticSource{localFn: local, hostFn: func(HostSettings, string) (string, error) {
		t.Error("local route receipt contacted a host")
		return "", errors.New("synthetic forbidden contact")
	}})
	s.Environment, s.AddressMode, s.StateDir = "synthetic", AddressModeOverlay, t.TempDir()
	s.Hosts = []HostSettings{{Name: "synthetic-route-a", OverlayAddress: "192.0.2.11"}}
	s.ObserverOverlay = ObserverOverlaySettings{Interfaces: []string{"syntheticVPN0"}, Gateways: []string{"198.51.100.1"}}
	return s
}

func observerReceiptAnswer(device string, args ...string) string {
	return fmt.Sprintf(`[{"dst":%q,"dev":%q}]`, args[len(args)-1], device)
}

func observerReceiptRun(t *testing.T, ctx context.Context, s SignalSettings, options *ObserverRouteSettings) (Alerts, error, runInvocationRecord, observerRouteReceipt, []byte) {
	t.Helper()
	signal := NewObserverRouteSignalWithSettings(options)
	var witnessOutput bytes.Buffer
	witness := invocationControlWitness(t, signal, &witnessOutput, io.Discard)
	alerts, err := runSignalWithWitness(ctx, signal, s, witness, "active", 0)
	rows := invocationControlRecords(t, &witnessOutput)
	if len(rows) != 2 || rows[1].ObserverRouteReceiptStatus != "stored" || !settingsWitnessHex(rows[1].ObserverRouteReceiptSha256, 64) {
		t.Fatal("own invocation did not retain its immutable route receipt locator")
	}
	r, raw := observerReceiptRead(t, s.StateDir, rows[1].ObserverRouteReceiptSha256)
	return alerts, err, rows[1], r, raw
}

func observerReceiptRead(t *testing.T, stateDir, hash string) (observerRouteReceipt, []byte) {
	t.Helper()
	path := filepath.Join(stateDir, "observer-route", "receipt-"+hash+".json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal("immutable route receipt missing")
	}
	sum := sha256.Sum256(raw)
	var r observerRouteReceipt
	if hex.EncodeToString(sum[:]) != hash || json.Unmarshal(raw, &r) != nil {
		t.Fatal("immutable route receipt hash/schema mismatch")
	}
	return r, raw
}

// Before the correction a healthy public run returned no Alert and no own
// receipt. The unchanged empty Alert result now has independently retained data.
func TestObserverRouteReceiptConformingHasOwnPolicyAndLocator(t *testing.T) {
	s := observerReceiptSettings(t, func(_ string, args ...string) (string, error) {
		return observerReceiptAnswer("syntheticVPN0", args...), nil
	})
	s.PostgreSQL.Password = "synthetic-private-password"
	options := DefaultObserverRouteSettings()
	options.Platform = "linux"
	alerts, err, invocation, r, raw := observerReceiptRun(t, context.Background(), s, options)
	if err != nil || len(alerts) != 0 || !r.Complete || !r.SourceAssociationComplete || !r.PolicyAssociationComplete || !r.PolicyUnchanged || r.ContextEnded || r.Reason != "observed" || len(r.Targets) != 1 || r.Targets[0].Outcome != "conforming" || !r.Targets[0].Complete {
		t.Fatal("healthy route lost explicit own authority or changed Alert suppression")
	}
	if r.WatcherRunId != invocation.RunId || r.Invocation != invocation.Invocation || r.CycleId == "" || r.StartedAt.IsZero() || r.FinishedAt.Before(r.StartedAt) || !settingsWitnessHex(r.PolicySha256, 64) {
		t.Fatal("receipt was not associated with its own source/cycle/policy")
	}
	for _, private := range []string{"synthetic-route-a", "192.0.2.11", "198.51.100.1", "syntheticVPN0", "synthetic-private-password"} {
		if bytes.Contains(raw, []byte(private)) {
			t.Fatal("route receipt serialized private policy/target/source values")
		}
	}
	dir := filepath.Join(s.StateDir, "observer-route")
	d, derr := os.Lstat(dir)
	f, ferr := os.Lstat(filepath.Join(dir, "receipt-"+invocation.ObserverRouteReceiptSha256+".json"))
	if derr != nil || ferr != nil || d.Mode().Perm() != 0700 || f.Mode().Perm() != 0600 {
		t.Fatal("receipt publication was not private")
	}
}

func TestObserverRouteReceiptPositiveAndUnavailableStayDistinct(t *testing.T) {
	for _, test := range []struct{ outcome, answer, class string }{
		{outcome: "diverted", answer: observerRouteTestDarwin("syntheticLAN0", ""), class: "observer-route-diverted"},
		{outcome: "absent", answer: "route: writing to routing socket: not in table\n", class: "observer-route-diverted"},
		{outcome: "unknown", answer: "synthetic-private-malformed-route\n", class: "observer-route-unobservable"},
	} {
		s := observerReceiptSettings(t, func(string, ...string) (string, error) { return test.answer, nil })
		options := DefaultObserverRouteSettings()
		options.Platform = "darwin"
		alerts, err, _, r, raw := observerReceiptRun(t, context.Background(), s, options)
		if err != nil || len(r.Targets) != 1 || r.Targets[0].Outcome != test.outcome || r.Targets[0].Complete != (test.outcome != "unknown") || r.Complete != (test.outcome != "unknown") {
			t.Fatalf("route outcome conflated: %s", test.outcome)
		}
		requireAlertClass(t, alerts, test.class)
		if bytes.Contains(raw, []byte("synthetic-private-malformed-route")) {
			t.Fatal("malformed source leaked into receipt")
		}
	}
}

// Every early return is an explicit incomplete cycle, including zero targets.
func TestObserverRouteReceiptEarlyReturnsNeverConform(t *testing.T) {
	for _, mode := range []string{"missing", "invalid-interface", "invalid-gateway", "lan", "zero-targets", "already-canceled"} {
		s := observerReceiptSettings(t, func(string, ...string) (string, error) {
			t.Error("early route return performed a lookup")
			return "", nil
		})
		ctx, cancel := context.WithCancel(context.Background())
		switch mode {
		case "missing":
			s.ObserverOverlay = ObserverOverlaySettings{}
		case "invalid-interface":
			s.ObserverOverlay.Interfaces = []string{"synthetic-invalid/interface"}
		case "invalid-gateway":
			s.ObserverOverlay.Gateways = []string{"synthetic-invalid-address"}
		case "lan":
			s.AddressMode = AddressModeLAN
		case "zero-targets":
			s.Hosts = nil
		case "already-canceled":
			cancel()
		}
		_, _, _, r, _ := observerReceiptRun(t, ctx, s, DefaultObserverRouteSettings())
		cancel()
		if r.Complete || r.Reason == "observed" {
			t.Fatalf("early/zero-target return certified routes: %s", mode)
		}
		for _, target := range r.Targets {
			if target.Complete {
				t.Fatalf("unread target certified: %s", mode)
			}
		}
	}
}

func TestObserverRouteReceiptCancellationDuringLookupIsIncomplete(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := observerReceiptSettings(t, func(_ string, args ...string) (string, error) {
		cancel() // explicit ordering: owner ends before the lookup returns
		return observerReceiptAnswer("syntheticVPN0", args...), nil
	})
	options := DefaultObserverRouteSettings()
	options.Platform = "linux"
	alerts, err, _, r, _ := observerReceiptRun(t, ctx, s, options)
	if !errors.Is(err, context.Canceled) || len(alerts) != 0 || r.Complete || !r.ContextEnded || r.Reason != "context-ended" || r.Targets[0].Complete {
		t.Fatal("canceled owner certified or published a healthy result")
	}
}

func TestObserverRouteReceiptExcludedAndDisabledStayScoped(t *testing.T) {
	lookups := 0
	s := observerReceiptSettings(t, func(_ string, args ...string) (string, error) {
		lookups++
		if args[len(args)-1] != "192.0.2.11" {
			t.Error("excluded or disabled route was looked up")
		}
		return observerReceiptAnswer("syntheticVPN0", args...), nil
	})
	s.Hosts = append(s.Hosts, HostSettings{Name: "synthetic-route-b", OverlayAddress: "192.0.2.12"})
	s.ExcludedHosts = []string{"synthetic-route-b"}
	s.disabledHosts = []HostSettings{{Name: "synthetic-route-disabled", OverlayAddress: "192.0.2.13"}}
	options := DefaultObserverRouteSettings()
	options.Platform = "linux"
	alerts, err, _, r, _ := observerReceiptRun(t, context.Background(), s, options)
	if err != nil || lookups != 1 || r.Complete || !r.PolicyAssociationComplete || len(r.Targets) != 3 || r.Targets[0].Outcome != "conforming" || r.Targets[1].Outcome != "excluded" || r.Targets[2].Outcome != "disabled" || r.Targets[1].Complete || r.Targets[2].Complete {
		t.Fatal("scoped routes were dropped, contacted, cross-associated or certified")
	}
	requireAlertClass(t, alerts, "monitor-host-scope-partial")
}

func TestObserverRouteReceiptUnrelatedDisabledRouterDoesNotChangePolicy(t *testing.T) {
	lookups := 0
	s := observerReceiptSettings(t, func(_ string, args ...string) (string, error) {
		lookups++
		return observerReceiptAnswer("syntheticVPN0", args...), nil
	})
	options := DefaultObserverRouteSettings()
	options.Platform = "linux"
	policy := observerRoutePolicyDigest(observerRouteEffectivePolicy(configFromSignalSettings(s.withDefaults()), nil, options), options.MaxReceiptBytes)
	s.Routers = []RouterSettings{{Name: "synthetic-router-disabled", OverlayAddress: "192.0.2.70", Disabled: true}}
	alerts, err, _, r, _ := observerReceiptRun(t, context.Background(), s, options)
	if err != nil || len(alerts) != 0 || lookups != 1 || !r.Complete || !r.PolicyAssociationComplete || r.PolicySha256 != policy || len(r.Targets) != 1 || r.Targets[0].Outcome != "conforming" {
		t.Fatal("unrelated disabled router changed route-inventory association or was contacted")
	}
}

func TestObserverRouteReceiptStoreFailurePreservesRealFinding(t *testing.T) {
	s := observerReceiptSettings(t, func(_ string, args ...string) (string, error) {
		return observerReceiptAnswer("syntheticLAN0", args...), nil
	})
	if err := os.WriteFile(filepath.Join(s.StateDir, "observer-route"), []byte("synthetic blocker"), 0600); err != nil {
		t.Fatal(err)
	}
	options := DefaultObserverRouteSettings()
	options.Platform = "linux"
	signal := NewObserverRouteSignalWithSettings(options)
	var output bytes.Buffer
	w := invocationControlWitness(t, signal, &output, io.Discard)
	alerts, err := runSignalWithWitness(context.Background(), signal, s, w, "active", 0)
	if err != nil {
		t.Fatal("receipt failure discarded actual route findings")
	}
	requireAlertClass(t, alerts, "observer-route-diverted")
	if !isVisibilityAlert(requireAlertClass(t, alerts, "cannot-observe")) {
		t.Fatal("failed receipt publication granted source completeness")
	}
	rows := invocationControlRecords(t, &output)
	if rows[1].ObserverRouteReceiptSha256 != "" || rows[1].ObserverRouteReceiptStatus != "unavailable" {
		t.Fatal("failed publication advertised a receipt")
	}
}

func TestObserverRouteReceiptTargetBoundNeverCertifiesOmittedScope(t *testing.T) {
	s := observerReceiptSettings(t, func(_ string, args ...string) (string, error) {
		return observerReceiptAnswer("syntheticVPN0", args...), nil
	})
	s.Hosts = append(s.Hosts, HostSettings{Name: "synthetic-route-b", OverlayAddress: "192.0.2.12"})
	options := DefaultObserverRouteSettings()
	options.Platform, options.MaxReceiptTargets = "linux", 1
	_, err, _, r, raw := observerReceiptRun(t, context.Background(), s, options)
	if err != nil || r.Complete || len(r.Targets) != 1 || r.TargetsOmitted != 1 || len(raw) > options.MaxReceiptBytes {
		t.Fatal("receipt target bound lost omitted-scope authority")
	}
}

func TestObserverRouteReceiptMalformedInventoryAddressRemainsUnknown(t *testing.T) {
	s := observerReceiptSettings(t, func(string, ...string) (string, error) {
		t.Error("invalid inventory address reached local lookup")
		return "", nil
	})
	s.Hosts[0].OverlayAddress = "synthetic-invalid-address"
	alerts, err, _, r, raw := observerReceiptRun(t, context.Background(), s, DefaultObserverRouteSettings())
	if err != nil || r.Complete || len(r.Targets) != 1 || r.Targets[0].Outcome != "unknown" || r.Targets[0].Complete || bytes.Contains(raw, []byte("synthetic-invalid-address")) {
		t.Fatal("malformed address was certified or serialized")
	}
	requireAlertClass(t, alerts, "observer-route-unobservable")
}

func TestObserverRouteReceiptByteBoundPublishesNoPrefix(t *testing.T) {
	s := observerReceiptSettings(t, func(_ string, args ...string) (string, error) {
		return observerReceiptAnswer("syntheticVPN0", args...), nil
	})
	options := DefaultObserverRouteSettings()
	options.Platform, options.MaxReceiptBytes = "linux", 128
	signal := NewObserverRouteSignalWithSettings(options)
	var output bytes.Buffer
	w := invocationControlWitness(t, signal, &output, io.Discard)
	alerts, err := runSignalWithWitness(context.Background(), signal, s, w, "active", 0)
	if err != nil {
		t.Fatal(err)
	}
	requireAlertClass(t, alerts, "cannot-observe")
	rows := invocationControlRecords(t, &output)
	if rows[1].ObserverRouteReceiptSha256 != "" || rows[1].ObserverRouteReceiptStatus != "unavailable" {
		t.Fatal("oversized receipt prefix was accepted")
	}
	if _, err := os.Stat(filepath.Join(s.StateDir, "observer-route")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("oversized receipt was materialized")
	}
}

func TestObserverRouteReceiptConcurrentInvocationsCannotCrossAssociate(t *testing.T) {
	entered := make(chan struct{}, 2)
	release := make(chan struct{})
	settings := make([]SignalSettings, 2)
	for i := range settings {
		device := fmt.Sprintf("syntheticVPN%d", i)
		settings[i] = observerReceiptSettings(t, func(_ string, args ...string) (string, error) {
			entered <- struct{}{}
			<-release
			return observerReceiptAnswer(device, args...), nil
		})
		settings[i].ObserverOverlay.Interfaces = []string{device}
	}
	settings[1].StateDir = settings[0].StateDir
	options := DefaultObserverRouteSettings()
	options.Platform = "linux"
	signal := NewObserverRouteSignalWithSettings(options)
	var output bytes.Buffer
	w := invocationControlWitness(t, signal, &output, io.Discard)
	var group sync.WaitGroup
	for i := range settings {
		s := settings[i]
		group.Add(1)
		go func() {
			defer group.Done()
			alerts, err := runSignalWithWitness(context.Background(), signal, s, w, "active", 0)
			if err != nil || len(alerts) != 0 {
				t.Error("concurrent synthetic route failed")
			}
		}()
	}
	for range settings {
		select {
		case <-entered:
		case <-time.After(5 * time.Second):
			close(release)
			group.Wait()
			t.Fatal("synthetic invocation failed to reach its explicit lookup barrier")
		}
	}
	close(release)
	group.Wait()
	seen := map[string]bool{}
	policies := map[string]bool{}
	for _, row := range invocationControlRecords(t, &output) {
		if row.Event != "run_return" {
			continue
		}
		r, _ := observerReceiptRead(t, settings[0].StateDir, row.ObserverRouteReceiptSha256)
		if !r.Complete || r.Invocation != row.Invocation || r.WatcherRunId != row.RunId || seen[r.CycleId] || policies[r.PolicySha256] {
			t.Fatal("concurrent completion crossed an invocation or policy association")
		}
		seen[r.CycleId], policies[r.PolicySha256] = true, true
	}
	if len(seen) != 2 || len(policies) != 2 {
		t.Fatal("concurrent own receipts were lost")
	}
}

func TestObserverRouteReceiptMismatchedPolicyAndUnboundSourceRemainUnknown(t *testing.T) {
	s := observerReceiptSettings(t, func(_ string, args ...string) (string, error) {
		return observerReceiptAnswer("syntheticVPN0", args...), nil
	})
	options := DefaultObserverRouteSettings()
	options.Platform = "linux"
	for _, mode := range []string{"missing-source", "wrong-policy", "wrong-source"} {
		env, err := newProbeEnv(s.withDefaults())
		if err != nil {
			t.Fatal(err)
		}
		ctx := context.Background()
		if mode != "missing-source" {
			identity := settingsWitnessTestIdentity()
			if mode == "wrong-source" {
				identity.Pid = 0
			}
			ctx = context.WithValue(ctx, observerRouteInvocationKey{}, &observerRouteInvocation{runId: strings.Repeat("a", 32), invocation: 7, identity: identity, policySha256: strings.Repeat("0", 64), completion: &observerRouteCompletion{}})
		}
		findings, err := (observerRouteProbe{settings: options}).check(ctx, env)
		if err != nil {
			t.Fatal(err)
		}
		for _, f := range findings {
			if !f.healthy {
				t.Fatal("association-only gap changed ordinary route detections")
			}
		}
	}
	paths, err := filepath.Glob(filepath.Join(s.StateDir, "observer-route", "receipt-*.json"))
	if err != nil || len(paths) != 3 {
		t.Fatal("unbound/mismatched cycles lost their incomplete receipts")
	}
	for _, path := range paths {
		raw, err := os.ReadFile(path)
		var r observerRouteReceipt
		if err != nil || json.Unmarshal(raw, &r) != nil || r.Complete || r.PolicyAssociationComplete {
			t.Fatal("unbound/mismatched route was certified")
		}
	}
}

func TestObserverRouteReceiptPolicyChangeDuringLookupInvalidatesAssociation(t *testing.T) {
	var env *probeEnv
	s := observerReceiptSettings(t, func(_ string, args ...string) (string, error) {
		env.cfg.observerOverlay.Interfaces[0] = "syntheticNew0"
		return observerReceiptAnswer("syntheticVPN0", args...), nil
	})
	options := DefaultObserverRouteSettings()
	options.Platform = "linux"
	var err error
	env, err = newProbeEnv(s.withDefaults())
	if err != nil {
		t.Fatal(err)
	}
	policy := observerRoutePolicyDigest(observerRouteEffectivePolicy(env.cfg, nil, options), options.MaxReceiptBytes)
	completion := &observerRouteCompletion{}
	ctx := context.WithValue(context.Background(), observerRouteInvocationKey{}, &observerRouteInvocation{runId: strings.Repeat("b", 32), invocation: 9, identity: settingsWitnessTestIdentity(), policySha256: policy, completion: completion})
	if _, err := (observerRouteProbe{settings: options}).check(ctx, env); err != nil {
		t.Fatal(err)
	}
	hash, status := completion.result()
	if status != "stored" {
		t.Fatal("changed-policy cycle was not retained")
	}
	r, _ := observerReceiptRead(t, s.StateDir, hash)
	if r.Complete || r.PolicyUnchanged || r.PolicyAssociationComplete || r.PolicySha256 != policy {
		t.Fatal("changed policy borrowed its earlier association")
	}
}

// Deterministic receipt-publication controls use synthetic retained admission
// owners and a source adapter; they never inspect native identity or contact PG.
package monitor

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// Override only the bounded PG source call; no queue or native process runs.
type pgWriterGenerationTestRunner struct {
	probeRunner
	response func(context.Context) (string, error)
}

// The owning probe still parses, encodes, stores and publishes its real state.
func (self pgWriterGenerationTestRunner) sshTimeout(ctx context.Context, _ *host, _, _ string, _ time.Duration) (string, error) {
	return self.response(ctx)
}

// Fixed invalid and unsupported capabilities exercise omission without reads.
type pgWriterGenerationTestBackend struct {
	generation pgSampleWriterGeneration
}

// This backend must never be acquired by the controlled source adapter.
func (self pgWriterGenerationTestBackend) acquire(context.Context, string) (func() error, error) {
	return nil, errors.New("unexpected synthetic admission")
}

// Return a value so retained metadata cannot alias the receipt.
func (self pgWriterGenerationTestBackend) pgSampleWriterGeneration() pgSampleWriterGeneration {
	return self.generation
}

// Lack of the optional interface must leave the sampler usable.
type pgWriterGenerationUnsupportedBackend struct{}

// The source adapter does not acquire this backend either.
func (self pgWriterGenerationUnsupportedBackend) acquire(context.Context, string) (func() error, error) {
	return nil, errors.New("unexpected synthetic admission")
}

// A synthetic externally bound tuple is only a comparison control, not native
// attestation. Copying this tuple could forge correlation, never physical proof.
func TestPgSampleWriterGenerationOwnerCorrelation(t *testing.T) {
	boundOwner := syntheticSshAdmissionOwner(19431)
	backend := &sharedSshAdmission{owner: boundOwner}
	generation := pgSampleWriterGenerationFor(backend)
	if generation == nil || generation.Schema != 1 || generation.Role != sshAdmissionWatch || generation.Owner != boundOwner {
		t.Fatal("retained watcher owner failed full tuple comparison")
	}
	backend.owner.Identity.StartTicks++
	if generation.Owner != boundOwner {
		t.Fatal("retained receipt snapshot aliased backend")
	}
	changes := []func(*sshAdmissionOwner){
		func(owner *sshAdmissionOwner) { owner.Identity.Pid++ },
		func(owner *sshAdmissionOwner) { owner.Identity.StartTicks++ },
		func(owner *sshAdmissionOwner) { owner.Identity.BootId = "00000000-0000-0000-0000-000000000002" },
		func(owner *sshAdmissionOwner) { owner.Unit = "other-owner.service" },
		func(owner *sshAdmissionOwner) { owner.InvocationId = strings.Repeat("a", 32) },
		func(owner *sshAdmissionOwner) { owner.Cgroup = "/other-owner.service" },
		func(owner *sshAdmissionOwner) { owner.Device++ },
		func(owner *sshAdmissionOwner) { owner.Inode++ },
	}
	for i, change := range changes {
		wrongOwner := boundOwner
		change(&wrongOwner)
		wrong := pgSampleWriterGenerationFor(&sharedSshAdmission{owner: wrongOwner})
		if wrong == nil || wrong.Owner == boundOwner {
			t.Fatalf("distinct valid identity component %d was lost", i)
		}
	}
	diagnosticOwner := syntheticSshAdmissionOwner(19432)
	diagnostic := pgSampleWriterGenerationFor(&sharedSshDiagnosticAdmission{owner: diagnosticOwner, watcher: boundOwner.Identity})
	if diagnostic == nil || diagnostic.Role != sshAdmissionDiagnostic || diagnostic.Owner != diagnosticOwner || diagnostic.Owner == boundOwner {
		t.Fatal("diagnostic borrowed watcher identity or role")
	}
	// Even identical owner values retain the diagnostic role discriminator.
	diagnostic = pgSampleWriterGenerationFor(&sharedSshDiagnosticAdmission{owner: boundOwner, watcher: boundOwner.Identity})
	if *diagnostic == *generation {
		t.Fatal("role distinction disappeared")
	}
}

// Unknown provenance is additive metadata absence, never a sampling failure.
func TestPgSampleWriterGenerationMissingAndInvalid(t *testing.T) {
	owner := syntheticSshAdmissionOwner(19431)
	for _, backend := range []sshAdmissionBackend{
		nil, pgWriterGenerationUnsupportedBackend{},
		pgWriterGenerationTestBackend{},
		pgWriterGenerationTestBackend{pgSampleWriterGeneration{Schema: 2, Role: sshAdmissionWatch, Owner: owner}},
		pgWriterGenerationTestBackend{pgSampleWriterGeneration{Schema: 1, Role: "unknown", Owner: owner}},
		pgWriterGenerationTestBackend{pgSampleWriterGeneration{Schema: 1, Role: sshAdmissionWatch}},
	} {
		if pgSampleWriterGenerationFor(backend) != nil {
			t.Fatal("unsupported or invalid provenance became known")
		}
	}
	var legacy pgQuerySampleReceipt
	if json.Unmarshal([]byte(`{"schema":1,"complete":true}`), &legacy) != nil || legacy.WriterGeneration != nil {
		t.Fatal("legacy receipt acquired generation")
	}
}

// Successful parsing replaces the receipt before local provenance is added.
// All paths exercise actual immutable storage and the strict cadence reader.
func TestPgSampleWriterGenerationPublication(t *testing.T) {
	for _, outcome := range []string{"complete", "parse-failure", "canceled-source", "legacy", "unsupported", "diagnostic"} {
		t.Run(outcome, func(t *testing.T) {
			now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
			owner := syntheticSshAdmissionOwner(19431)
			var backend sshAdmissionBackend = &sharedSshAdmission{owner: owner}
			if outcome == "legacy" {
				backend = nil
			} else if outcome == "unsupported" {
				backend = pgWriterGenerationUnsupportedBackend{}
			} else if outcome == "diagnostic" {
				backend = &sharedSshDiagnosticAdmission{owner: owner, watcher: syntheticSshAdmissionOwner(19432).Identity}
			}
			env := pgSampleTestEnv(t, func([]string, string) (string, string, error) { t.Fatal("uncontrolled transport"); return "", "", nil })
			env.cfg.pgQuerySampleContinuous = true
			env.cfg.pgQuerySampleUntil = time.Time{}
			env.cfg.sharedSshAdmission = backend
			env.now = func() time.Time { return now }
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			calls := 0
			env.runner = pgWriterGenerationTestRunner{probeRunner: env.runner, response: func(context.Context) (string, error) {
				calls++
				if outcome == "parse-failure" {
					return "invalid\n", nil
				}
				if outcome == "canceled-source" {
					cancel()
					return "", context.Canceled
				}
				return pgSampleTestEncode(t, pgSampleTestFrames(now)), nil
			}}
			findings, err := (pgQuerySampleProbe{}).check(ctx, env)
			if err != nil || calls != 1 {
				t.Fatal("controlled attempt failed before receipt publication")
			}
			dir := filepath.Join(env.cfg.stateDir, "pg-query-sample")
			state, err := pgSampleReadCadence(dir, now)
			if err != nil || state.Schema != 1 || state.ReceiptSHA256 == "" || !state.NextEligibleAt.Equal(now.Add(pgQuerySampleCadence)) {
				t.Fatal("strict terminal state or cadence changed")
			}
			raw, err := os.ReadFile(filepath.Join(dir, "receipt-"+state.ReceiptSHA256+".json"))
			if err != nil || fmt.Sprintf("%x", sha256.Sum256(raw)) != state.ReceiptSHA256 {
				t.Fatal("state did not select exact immutable receipt bytes")
			}
			var receipt pgQuerySampleReceipt
			if json.Unmarshal(raw, &receipt) != nil {
				t.Fatal("stored receipt malformed")
			}
			wantComplete := outcome != "parse-failure" && outcome != "canceled-source"
			if receipt.Complete != wantComplete || (state.Outcome == "complete") != wantComplete {
				t.Fatal("generation metadata changed collection outcome")
			}
			if !reflect.DeepEqual(receipt.WriterGeneration, pgSampleWriterGenerationFor(backend)) {
				t.Fatal("publication lost or substituted local provenance")
			}
			if (outcome == "legacy" || outcome == "unsupported") && strings.Contains(string(raw), "writer_generation") {
				t.Fatal("unknown generation serialized as known")
			}
			if receipt.Complete {
				withGeneration := pgSampleFindings(receipt, "synthetic")
				receipt.WriterGeneration = nil
				if !reflect.DeepEqual(withGeneration, pgSampleFindings(receipt, "synthetic")) {
					t.Fatal("generation changed public findings")
				}
			}
			alerts := Alerts{}
			for _, finding := range findings {
				alerts = append(alerts, alertFromFinding(syntheticSettings(nil), "2.1a", "pg-query-sample", "sample", finding))
			}
			public, err := json.Marshal(alerts)
			if err != nil {
				t.Fatal(err)
			}
			for _, private := range []string{"writer_generation", owner.Unit, owner.Cgroup, owner.InvocationId, owner.Identity.BootId} {
				if strings.Contains(string(public), private) {
					t.Fatal("private owner escaped public alert projection")
				}
			}
			if outcome != "canceled-source" {
				if admitted, err := pgSampleContinuousAdmission(dir, now.Add(time.Minute), nil); err != nil || admitted {
					t.Fatal("generation metadata bypassed durable floor")
				}
			}
		})
	}
}

// A canceled caller cannot create provenance, a cadence reservation or a receipt.
func TestPgSampleWriterGenerationCanceledBeforeAttempt(t *testing.T) {
	env := pgSampleTestEnv(t, func([]string, string) (string, string, error) {
		t.Fatal("canceled attempt contacted")
		return "", "", nil
	})
	env.cfg.sharedSshAdmission = &sharedSshAdmission{owner: syntheticSshAdmissionOwner(19431)}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := (pgQuerySampleProbe{}).check(ctx, env); err == nil {
		t.Fatal("canceled attempt admitted")
	}
	if _, err := os.Stat(filepath.Join(env.cfg.stateDir, "pg-query-sample")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("canceled attempt published durable evidence")
	}
}

// New provenance is inside the existing content hash and retention ceiling.
func TestPgSampleWriterGenerationImmutableHashAndBound(t *testing.T) {
	dir := t.TempDir()
	owner := syntheticSshAdmissionOwner(19431)
	receipt := pgQuerySampleReceipt{Schema: 1, WriterGeneration: pgSampleWriterGenerationFor(&sharedSshAdmission{owner: owner})}
	firstRaw, _, err := pgSampleEncodeReceipts(&receipt)
	if err != nil {
		t.Fatal(err)
	}
	first, err := pgSampleStoreReceipt(dir, firstRaw)
	if err != nil {
		t.Fatal(err)
	}
	receipt.WriterGeneration.Owner.Identity.StartTicks++
	secondRaw, _, err := pgSampleEncodeReceipts(&receipt)
	if err != nil {
		t.Fatal(err)
	}
	second, err := pgSampleStoreReceipt(dir, secondRaw)
	if err != nil || second == first {
		t.Fatal("generation not bound by immutable digest")
	}
	retained, err := os.ReadFile(filepath.Join(dir, "receipt-"+first+".json"))
	if err != nil || string(retained) != string(firstRaw) {
		t.Fatal("old generation evidence changed")
	}
	receipt.Qualifiers = []string{strings.Repeat("x", pgSampleReceiptBytes)}
	if _, _, err := pgSampleEncodeReceipts(&receipt); err == nil {
		t.Fatal("provenance escaped existing retention ceiling")
	}
}

// A stored receipt does not claim completed publication if terminal state fails.
func TestPgSampleWriterGenerationTerminalPublicationFailure(t *testing.T) {
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	env := pgSampleTestEnv(t, func([]string, string) (string, string, error) { t.Fatal("uncontrolled source"); return "", "", nil })
	env.cfg.pgQuerySampleContinuous = true
	env.cfg.pgQuerySampleUntil = time.Time{}
	env.cfg.sharedSshAdmission = &sharedSshAdmission{owner: syntheticSshAdmissionOwner(19431)}
	env.now = func() time.Time { return now }
	env.runner = pgWriterGenerationTestRunner{probeRunner: env.runner, response: func(context.Context) (string, error) {
		return pgSampleTestEncode(t, pgSampleTestFrames(now)), nil
	}}
	publications := 0
	probe := pgQuerySampleProbe{syncAttemptFile: func(file *os.File) error {
		if strings.HasPrefix(filepath.Base(file.Name()), ".cadence-") {
			publications++
			if publications == 2 {
				return errors.New("synthetic terminal publication failure")
			}
		}
		return nil
	}}
	if _, err := probe.check(context.Background(), env); err == nil || env.pgSampleCompletion != nil {
		t.Fatal("failed terminal state became completed collection proof")
	}
	dir := filepath.Join(env.cfg.stateDir, "pg-query-sample")
	state, err := pgSampleReadCadence(dir, now)
	if err != nil || state.Outcome != "attempting" || state.ReceiptSHA256 != "" || !state.NextEligibleAt.Equal(now.Add(pgQuerySampleBudget+pgQuerySampleCadence)) {
		t.Fatal("publication failure lost crash reservation")
	}
	receipts, err := filepath.Glob(filepath.Join(dir, "receipt-*.json"))
	if err != nil || len(receipts) != 1 {
		t.Fatal("terminal failure erased immutable evidence")
	}
	raw, err := os.ReadFile(receipts[0])
	if err != nil {
		t.Fatal(err)
	}
	var receipt pgQuerySampleReceipt
	if json.Unmarshal(raw, &receipt) != nil || !receipt.Complete || !reflect.DeepEqual(receipt.WriterGeneration, pgSampleWriterGenerationFor(env.cfg.sharedSshAdmission)) {
		t.Fatal("unindexed immutable evidence lost provenance")
	}
}

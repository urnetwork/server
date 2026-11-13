// A baseline-compatible behavioral RED reaches real receipt publication before
// checking its additive JSON field; no new production type is referenced here.
package monitor

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

// The adapter forbids native calls while retaining actual check/parse/storage.
type pgWriterCausalTestRunner struct {
	probeRunner
	response func() string
}

// Only the remote source is synthetic; local receipt publication is unchanged.
func (self pgWriterCausalTestRunner) sshTimeout(context.Context, *host, string, string, time.Duration) (string, error) {
	return self.response(), nil
}

// Baseline must fail only after complete source and selected terminal evidence
// are proved. The expected owner is a synthetic independent comparison control,
// not runtime native attestation or physical file-writer authentication.
func TestPgSampleWriterGenerationPublishedRawJson(t *testing.T) {
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	expectedOwner := syntheticSshAdmissionOwner(19431)
	if !expectedOwner.valid() {
		t.Fatal("causal setup owner invalid")
	}
	env := pgSampleTestEnv(t, func([]string, string) (string, string, error) {
		t.Fatal("causal setup reached uncontrolled source")
		return "", "", nil
	})
	env.cfg.pgQuerySampleContinuous = true
	env.cfg.pgQuerySampleUntil = time.Time{}
	env.cfg.sharedSshAdmission = &sharedSshAdmission{owner: expectedOwner}
	env.now = func() time.Time { return now }
	calls := 0
	env.runner = pgWriterCausalTestRunner{probeRunner: env.runner, response: func() string {
		calls++
		return pgSampleTestEncode(t, pgSampleTestFrames(now))
	}}
	if _, err := (pgQuerySampleProbe{}).check(context.Background(), env); err != nil || calls != 1 {
		t.Fatal("causal setup did not complete exactly one controlled source call")
	}
	dir := filepath.Join(env.cfg.stateDir, "pg-query-sample")
	state, err := pgSampleReadCadence(dir, now)
	if err != nil || state.Schema != 1 || state.Mode != "continuous" || state.Outcome != "complete" || !state.AttemptedAt.Equal(now) || !state.TerminalAt.Equal(now) || !state.CompletedAt.Equal(now) || !state.NextEligibleAt.Equal(now.Add(pgQuerySampleCadence)) || state.ReceiptSHA256 == "" {
		t.Fatal("causal setup terminal state not complete and cadence-qualified")
	}
	receiptPaths, err := filepath.Glob(filepath.Join(dir, "receipt-*.json"))
	expectedPath := filepath.Join(dir, "receipt-"+state.ReceiptSHA256+".json")
	if err != nil || len(receiptPaths) != 1 || receiptPaths[0] != expectedPath {
		t.Fatal("causal setup terminal state did not select sole immutable receipt")
	}
	raw, err := os.ReadFile(expectedPath)
	if err != nil || len(raw) > pgSampleReceiptBytes || fmt.Sprintf("%x", sha256.Sum256(raw)) != state.ReceiptSHA256 {
		t.Fatal("causal setup receipt byte bound or selected digest invalid")
	}
	var receipt struct {
		Schema      int       `json:"schema"`
		Complete    bool      `json:"complete"`
		Samples     int       `json:"samples"`
		RequestedAt time.Time `json:"requested_at"`
		FinishedAt  time.Time `json:"finished_at"`
	}
	if json.Unmarshal(raw, &receipt) != nil || receipt.Schema != 1 || !receipt.Complete || receipt.Samples != pgQuerySampleCount || !receipt.RequestedAt.Equal(now) || !receipt.FinishedAt.Equal(now) {
		t.Fatal("causal setup stored source receipt not complete")
	}
	completion := env.pgSampleCompletion
	if completion == nil || completion.ReceiptSha256 != state.ReceiptSHA256 || completion.Samples != pgQuerySampleCount || !completion.RequestedAt.Equal(now) || !completion.FinishedAt.Equal(now) || !completion.NextEligibleAt.Equal(state.NextEligibleAt) {
		t.Fatal("causal setup same-call completion not bound to published state")
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil {
		t.Fatal("causal setup stored JSON object malformed")
	}
	generationRaw, ok := fields["writer_generation"]
	if !ok || bytes.Equal(bytes.TrimSpace(generationRaw), []byte("null")) {
		t.Fatal("published receipt missing writer generation correlation")
	}
	var generationFields map[string]json.RawMessage
	if json.Unmarshal(generationRaw, &generationFields) != nil || len(generationFields) != 3 || generationFields["schema"] == nil || generationFields["role"] == nil || generationFields["owner"] == nil {
		t.Fatal("published writer generation JSON shape invalid")
	}
	var schema int
	var role string
	if json.Unmarshal(generationFields["schema"], &schema) != nil || schema != 1 || json.Unmarshal(generationFields["role"], &role) != nil || role != sshAdmissionWatch {
		t.Fatal("published writer generation schema or watcher role invalid")
	}
	expectedRaw, err := json.Marshal(expectedOwner)
	if err != nil {
		t.Fatal("causal setup expected owner JSON unavailable")
	}
	var expectedFields, actualFields map[string]any
	if json.Unmarshal(expectedRaw, &expectedFields) != nil || json.Unmarshal(generationFields["owner"], &actualFields) != nil || !reflect.DeepEqual(expectedFields, actualFields) {
		t.Fatal("published writer generation did not preserve full bound owner tuple")
	}
}

// Cached Snow judgments retain their original authenticated input without
// extending the current operation's deadline or sampling another model answer.
package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026/model"
)

// Exercise the native three-member producer with an intercepted transport;
// every response and journal is synthetic and no socket is opened.
func winnerReviewSnowCacheFixture(t *testing.T) (winnerReviewBrokerRequest, func() ([]byte, error), *int) {
	t.Helper()
	request, judgment := winnerModelFixture()
	request.Request.Policy.WinnerReviewPolicy = model.CompetitionSnowWinnerReviewPolicy
	request.Request.ReviewerModel, request.Request.ReviewerReasoning = model.CompetitionSnowWinnerReviewModel, model.CompetitionSnowWinnerReviewReasoning
	request.Template.ReviewerModel, request.Template.ReviewerReasoning = request.Request.ReviewerModel, request.Request.ReviewerReasoning
	request.Request.JobId = request.Template.JobId
	request.Request.StartedAt = time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	request.Request.Deadline = request.Request.StartedAt.Add(30 * time.Minute)
	provider, calls := defaultWinnerSnowProviderSettings(), 0
	output, err := json.Marshal(judgment)
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Transport: winnerReviewTransport(func(message *http.Request) (*http.Response, error) {
		index := calls
		calls++
		if index >= len(provider.Members) {
			t.Fatal("cached Snow review started another provider attempt")
		}
		var payload struct{ Model string }
		if err := json.NewDecoder(message.Body).Decode(&payload); err != nil || payload.Model != provider.Members[index].Model {
			t.Fatal("native Snow member order changed", err)
		}
		body, err := json.Marshal(map[string]any{"id": fmt.Sprintf("synthetic-cache-response-%d", index), "status": "completed", "model": payload.Model,
			"usage":  map[string]int{"input_tokens": 10, "output_tokens": 10},
			"output": []any{map[string]any{"type": "message", "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": string(output)}}}}})
		if err != nil {
			t.Fatal(err)
		}
		header := make(http.Header)
		header.Set("Content-Type", "application/json")
		return &http.Response{StatusCode: http.StatusOK, Header: header, Body: io.NopCloser(bytes.NewReader(body))}, nil
	})}
	state := t.TempDir()
	if err := os.Chmod(state, 0700); err != nil {
		t.Fatal(err)
	}
	run := func() ([]byte, error) {
		result, err := executeWinnerSnow(t.Context(), client, request, defaultWinnerSnowModelSettings(), provider, strings.Repeat("a", 64), state)
		if err != nil {
			return nil, err
		}
		return json.Marshal(result)
	}
	return request, run, &calls
}

// Both completed-only and accepted caches must replay the native Snow result
// using its original full-input hash when only the execution clocks renew.
func TestWinnerReviewSnowCacheReplaysOriginalInputAfterClockRenewal(t *testing.T) {
	request, run, calls := winnerReviewSnowCacheFixture(t)
	path := filepath.Join(t.TempDir(), "judgment.json")
	originalBytes, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	var firstOutput, firstCache []byte
	for index := range 3 {
		if index > 0 {
			request.Request.StartedAt = request.Request.StartedAt.Add(time.Hour)
			request.Request.Deadline = request.Request.Deadline.Add(time.Hour)
		}
		currentDeadline := request.Request.Deadline
		output, original, err := winnerReviewResponse(path, request, run)
		if err != nil {
			t.Fatal("cached Snow response could not recover its original input", err)
		}
		retainedBytes, err := json.Marshal(original)
		if err != nil || !bytes.Equal(retainedBytes, originalBytes) || !request.Request.Deadline.Equal(currentDeadline) {
			t.Fatal("original input or current operation deadline changed", err)
		}
		var result winnerReviewBrokerResult
		if err := decodeStrictJSONBytes(output, &result, "synthetic cached Snow result"); err != nil {
			t.Fatal(err)
		}
		if err := validateWinnerReviewBrokerResult(&result, request.Template, request.Request.Policy, original); err != nil {
			t.Fatal("native Snow validation rejected the exact retained original", err)
		}
		if index > 0 && validateWinnerReviewBrokerResult(&result, request.Template, request.Request.Policy, request) == nil {
			t.Fatal("Snow evidence stopped binding the original full input including clocks")
		}
		cache, err := os.ReadFile(path + ".broker-response")
		if err != nil {
			t.Fatal(err)
		}
		if index == 0 {
			firstOutput, firstCache = output, cache
		} else if !bytes.Equal(output, firstOutput) || !bytes.Equal(cache, firstCache) {
			t.Fatal("clock renewal rewrote a model judgment or its completed cache")
		}
		if index == 1 {
			if err := cacheWinnerReviewJudgment(path, output); err != nil {
				t.Fatal(err)
			}
		}
	}
	if *calls != 3 {
		t.Fatal("clock renewal called the provider again", *calls)
	}
}

// Non-clock changes, including otherwise rehashed score/seed data, must reject
// before transport in either completed-only or accepted replay.
func TestWinnerReviewSnowCacheRejectsChangedNonClockIdentity(t *testing.T) {
	request, run, calls := winnerReviewSnowCacheFixture(t)
	path := filepath.Join(t.TempDir(), "judgment.json")
	output, _, err := winnerReviewResponse(path, request, run)
	if err != nil {
		t.Fatal(err)
	}
	original, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	for _, accepted := range []bool{false, true} {
		if accepted {
			if err := cacheWinnerReviewJudgment(path, output); err != nil {
				t.Fatal(err)
			}
		}
		for _, change := range []struct {
			name   string
			mutate func(*winnerReviewBrokerRequest)
		}{
			{name: "statistics", mutate: func(r *winnerReviewBrokerRequest) {
				r.Packet.Score = []byte(`{"synthetic_score":true,"synthetic_statistics":2}`)
			}},
			{name: "seed", mutate: func(r *winnerReviewBrokerRequest) {
				r.Packet.Score = []byte(`{"synthetic_score":true,"synthetic_seed":2}`)
			}},
			{name: "policy", mutate: func(r *winnerReviewBrokerRequest) { r.Request.Policy.WinnerReviewTimeoutSeconds++ }},
			{name: "job", mutate: func(r *winnerReviewBrokerRequest) { r.Request.JobId[0] ^= 1 }},
			{name: "reward revision", mutate: func(r *winnerReviewBrokerRequest) { r.Request.RewardRevision++ }},
			{name: "seal", mutate: func(r *winnerReviewBrokerRequest) { r.Request.SealSha256 = strings.Repeat("9", 64) }},
			{name: "source", mutate: func(r *winnerReviewBrokerRequest) { r.SourceFiles[0].Content += "// changed synthetic source\n" }},
		} {
			var changed winnerReviewBrokerRequest
			if err := json.Unmarshal(original, &changed); err != nil {
				t.Fatal(err)
			}
			change.mutate(&changed)
			if change.name == "statistics" || change.name == "seed" {
				digest := sha256.Sum256(changed.Packet.Score)
				changed.Template.ScoreSha256 = hex.EncodeToString(digest[:])
			}
			if _, _, err := winnerReviewResponse(path, changed, func() ([]byte, error) {
				t.Fatal("changed cached request invoked transport")
				return nil, nil
			}); err == nil {
				t.Fatalf("accepted=%t changed %s reused original Snow authority", accepted, change.name)
			}
		}
	}
	if *calls != 3 {
		t.Fatal("identity rejection called another provider", *calls)
	}
}

// Missing legacy metadata, missing clocks and substituted original digests
// remain operator holds; an existing accepted judgment never enables reroll.
func TestWinnerReviewSnowCacheRejectsMissingOrChangedOriginalMetadata(t *testing.T) {
	request, run, calls := winnerReviewSnowCacheFixture(t)
	root := t.TempDir()
	path := filepath.Join(root, "original.json")
	output, _, err := winnerReviewResponse(path, request, run)
	if err != nil {
		t.Fatal(err)
	}
	cache, err := os.ReadFile(path + ".broker-response")
	if err != nil {
		t.Fatal(err)
	}
	for index, mutate := range []func(map[string]any){
		func(c map[string]any) { c["schema"] = 1; delete(c, "original_request") },
		func(c map[string]any) { delete(c, "original_request") },
		func(c map[string]any) { delete(c["original_request"].(map[string]any), "started_at") },
		func(c map[string]any) { delete(c["original_request"].(map[string]any), "deadline") },
		func(c map[string]any) { c["original_request"].(map[string]any)["sha256"] = strings.Repeat("f", 64) },
		func(c map[string]any) {
			c["original_request"].(map[string]any)["deadline"] = request.Request.Deadline.Add(time.Hour).Format(time.RFC3339Nano)
		},
	} {
		var envelope map[string]any
		if err := json.Unmarshal(cache, &envelope); err != nil {
			t.Fatal(err)
		}
		mutate(envelope)
		changed, err := json.Marshal(envelope)
		if err != nil {
			t.Fatal(err)
		}
		changedPath := filepath.Join(root, fmt.Sprintf("changed-%d.json", index))
		if err := cacheWinnerReviewJudgment(changedPath, output); err != nil {
			t.Fatal(err)
		}
		if err := cacheWinnerReviewJudgment(changedPath+".broker-response", changed); err != nil {
			t.Fatal(err)
		}
		if _, _, err := winnerReviewResponse(changedPath, request, func() ([]byte, error) {
			t.Fatal("unavailable original metadata rerolled an accepted judgment")
			return nil, nil
		}); err == nil {
			t.Fatalf("original metadata mutation %d accepted", index)
		}
	}
	missingPath := filepath.Join(root, "missing-completed.json")
	if err := cacheWinnerReviewJudgment(missingPath, output); err != nil {
		t.Fatal(err)
	}
	if _, _, err := winnerReviewResponse(missingPath, request, run); err == nil || *calls != 3 {
		t.Fatal("missing completed cache rerolled an accepted Snow judgment", err, *calls)
	}
}

// Even semantically identical JSON bytes cannot substitute another accepted
// output for the exact response bound to the retained original request.
func TestWinnerReviewSnowCacheRejectsChangedAcceptedBytes(t *testing.T) {
	request, run, calls := winnerReviewSnowCacheFixture(t)
	path := filepath.Join(t.TempDir(), "judgment.json")
	output, _, err := winnerReviewResponse(path, request, run)
	if err != nil {
		t.Fatal(err)
	}
	changed := append(append([]byte{}, output...), '\n')
	if err := cacheWinnerReviewJudgment(path, changed); err != nil {
		t.Fatal(err)
	}
	if _, _, err := winnerReviewResponse(path, request, run); err == nil || *calls != 3 {
		t.Fatal("changed accepted output reused original authority or rerolled", err, *calls)
	}
}

// Retaining original clocks and its digest must not duplicate a near-maximum
// input packet into the independently bounded completed-response cache.
func TestWinnerReviewCompletedCacheKeepsOriginalContextCompact(t *testing.T) {
	request := winnerReviewBrokerRequest{Operation: "review_elected_winner", Instructions: strings.Repeat("x", int(defaultWinnerModelSettings().MaximumInputBytes)-4096)}
	original, err := json.Marshal(request)
	if err != nil || int64(len(original)) > defaultWinnerModelSettings().MaximumInputBytes {
		t.Fatal("synthetic request exceeded the actual broker input bound", err)
	}
	path := filepath.Join(t.TempDir(), "completed.json")
	output, recovered, err := winnerReviewCompletedResponse(path, request, func() ([]byte, error) { return []byte("synthetic completed response"), nil })
	if err != nil || string(output) != "synthetic completed response" {
		t.Fatal("bounded original request could not be cached", err)
	}
	recoveredBytes, err := json.Marshal(recovered)
	if err != nil || !bytes.Equal(recoveredBytes, original) {
		t.Fatal("compact metadata lost original request bytes", err)
	}
	cache, err := os.ReadFile(path)
	if err != nil || len(cache) >= 1024 {
		t.Fatal("compact metadata duplicated the request packet", err)
	}
}

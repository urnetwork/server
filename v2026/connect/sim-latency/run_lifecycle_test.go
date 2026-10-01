package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestFleetAccountingProtocolReturnsIndependentSnapshots(t *testing.T) {
	commands := strings.NewReader("snapshot\nsnapshot\n")
	var responses bytes.Buffer
	snapshots := []int64{17, 41}
	index := 0
	err := serveFleetAccounting(commands, &responses, func() int64 {
		value := snapshots[index]
		index += 1
		return value
	}, func() error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	if responses.String() != "17\n41\n" || index != 2 {
		t.Fatalf("fleet accounting responses=%q snapshots=%d", responses.String(), index)
	}
}

func TestFleetAccountingProtocolStartsDynamicsAndReturnsSnapshots(t *testing.T) {
	commands := strings.NewReader("start-dynamics\nsnapshot\n")
	var responses bytes.Buffer
	started := 0
	err := serveFleetAccounting(
		commands,
		&responses,
		func() int64 { return 17 },
		func() error {
			started++
			return nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if responses.String() != "0\n17\n" || started != 1 {
		t.Fatalf("fleet control responses=%q dynamics starts=%d", responses.String(), started)
	}
}

func TestFleetAccountingProtocolRejectsUnknownCommand(t *testing.T) {
	err := serveFleetAccounting(
		strings.NewReader("reset\n"),
		io.Discard,
		func() int64 { return 1 },
		func() error { return nil },
	)
	if err == nil || !strings.Contains(err.Error(), "unknown fleet accounting command") {
		t.Fatalf("unknown command error=%v", err)
	}
}

func TestProviderEgressSnapshotAggregatesShards(t *testing.T) {
	newProcess := func(index int, byteCount int64) *fleetProcess {
		commandReader, commandWriter, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		responseReader, responseWriter, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		proc := &fleetProcess{
			index:                    index,
			done:                     make(chan struct{}),
			accountingCommandWriter:  commandWriter,
			accountingResponseReader: responseReader,
			accountingResponses:      make(chan fleetAccountingResponse, 4),
		}
		go proc.readAccountingResponses()
		go func() {
			_ = serveFleetAccounting(
				commandReader,
				responseWriter,
				func() int64 { return byteCount },
				func() error { return nil },
			)
			commandReader.Close()
			responseWriter.Close()
		}()
		t.Cleanup(func() {
			proc.closeAccountingCommand()
			responseReader.Close()
		})
		return proc
	}
	procs := []*fleetProcess{newProcess(0, 20), newProcess(1, 22)}
	byteCount, err := providerEgressSnapshot(nil, procs)
	if err != nil {
		t.Fatal(err)
	}
	if byteCount != 42 {
		t.Fatalf("provider egress bytes=%d, want 42", byteCount)
	}
}

func TestStartProviderDynamicsCommandsEveryShard(t *testing.T) {
	const shardCount = 2
	started := make(chan int, shardCount)
	procs := make([]*fleetProcess, 0, shardCount)
	for index := 0; index < shardCount; index++ {
		commandReader, commandWriter, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		responseReader, responseWriter, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		proc := &fleetProcess{
			index:                    index,
			done:                     make(chan struct{}),
			accountingCommandWriter:  commandWriter,
			accountingResponseReader: responseReader,
			accountingResponses:      make(chan fleetAccountingResponse, 2),
		}
		go proc.readAccountingResponses()
		go func(index int) {
			_ = serveFleetAccounting(
				commandReader,
				responseWriter,
				func() int64 { return 0 },
				func() error {
					started <- index
					return nil
				},
			)
			commandReader.Close()
			responseWriter.Close()
		}(index)
		procs = append(procs, proc)
		t.Cleanup(func() {
			proc.closeAccountingCommand()
			responseReader.Close()
		})
	}
	if err := startProviderDynamics(context.Background(), nil, procs); err != nil {
		t.Fatal(err)
	}
	startedIndexes := map[int]bool{}
	for index := 0; index < shardCount; index++ {
		startedIndexes[<-started] = true
	}
	if len(startedIndexes) != shardCount {
		t.Fatalf("fleet shards started = %v, want every shard", startedIndexes)
	}
}

func TestWriteAccountingSourceAuthenticatesCounterDelta(t *testing.T) {
	path := filepath.Join(t.TempDir(), "accounting.source.json")
	runStats := &RunStats{
		EvaluationId:   "evaluation-1",
		MeasureStartMs: 100,
		MeasureEndMs:   200,
	}
	digest, byteCount, err := writeAccountingSource(path, runStats, 10, 52)
	if err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	wantDigest := fmt.Sprintf("%x", sha256.Sum256(content))
	if digest != wantDigest || byteCount != int64(len(content)) {
		t.Fatalf("accounting identity=%s/%d, want %s/%d", digest, byteCount, wantDigest, len(content))
	}
	var source providerAccountingSource
	if err := json.Unmarshal(content, &source); err != nil {
		t.Fatal(err)
	}
	if !source.Complete || source.EvaluationId != "evaluation-1" ||
		source.ProviderEgressBytes != 42 || source.CounterStartBytes != 10 ||
		source.CounterEndBytes != 52 {
		t.Fatalf("accounting source=%+v", source)
	}
	if _, _, err := writeAccountingSource(path, runStats, 10, 52); err == nil {
		t.Fatal("accounting source overwrote an existing report")
	}
}

func TestWriteAccountingSourceRejectsCounterRegression(t *testing.T) {
	runStats := &RunStats{
		EvaluationId:   "evaluation-1",
		MeasureStartMs: 100,
		MeasureEndMs:   200,
	}
	if _, _, err := writeAccountingSource(
		filepath.Join(t.TempDir(), "accounting.source.json"),
		runStats,
		52,
		10,
	); err == nil {
		t.Fatal("regressed provider counter was accepted")
	}
}

func TestOfficialResultsSidecarIncludesRunManifest(t *testing.T) {
	path := filepath.Join("artifact", "evaluation-1", "results.csv")
	candidates := sideCarCandidates(path)
	if len(candidates) != 3 ||
		candidates[0] != filepath.Join("artifact", "evaluation-1", "run.json") ||
		candidates[1] != filepath.Join("artifact", "evaluation-1", "results.run.json") ||
		candidates[2] != filepath.Join("artifact", "evaluation-1", "results.csv.run.json") {
		t.Fatalf("sidecar candidates=%v", candidates)
	}
}

func TestWaitPhaseCancelsPromptly(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	err := waitPhase(ctx, time.Hour)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("waitPhase error = %v, want context cancellation", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("canceled phase took %s to return", elapsed)
	}
}

func TestOfficialRunRejectsMissingIdentityWithoutMarker(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "run.complete.json")
	err := Run(&RunOptions{
		Official:       true,
		Reset:          true,
		Duration:       time.Second,
		RequestTimeout: time.Second,
		MetaPath:       filepath.Join(t.TempDir(), "run.json"),
		FinalMarker:    marker,
	})
	var incomplete *EvaluationIncompleteError
	if !errors.As(err, &incomplete) || incomplete.Code != "invalid_options" {
		t.Fatalf("Run error = %v, want invalid_options", err)
	}
	if _, statErr := os.Stat(marker); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("invalid official run created a marker: %v", statErr)
	}
}

func TestEvaluationIdValidation(t *testing.T) {
	for _, value := range []string{"round-1.rep_2", "A", "a_b-c.9"} {
		if !runEvaluationIdPattern.MatchString(value) {
			t.Fatalf("valid evaluation id rejected: %q", value)
		}
	}
	for _, value := range []string{"", "contains space", "line\nbreak", "bad]id", string(make([]byte, 129))} {
		if runEvaluationIdPattern.MatchString(value) {
			t.Fatalf("invalid evaluation id accepted: %q", value)
		}
	}
}

func TestClientDriverStopArrivalsDrainsWithoutCancelingContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	driver := &ClientDriver{
		ctx:          ctx,
		config:       &Config{},
		clients:      []*pooledClient{{}},
		out:          bufio.NewWriter(io.Discard),
		arrivalsStop: make(chan struct{}),
	}
	driver.active.Add(1)
	done := make(chan error, 1)
	go func() { done <- driver.Run() }()

	driver.StopArrivals()
	driver.StopArrivals() // idempotent
	select {
	case err := <-done:
		t.Fatalf("driver returned before admitted crawl drained: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	if ctx.Err() != nil {
		t.Fatalf("stopping arrivals canceled the request context: %v", ctx.Err())
	}

	driver.active.Done()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("driver drain error: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("driver did not return after admitted crawl drained")
	}
}

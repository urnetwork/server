package model

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/urnetwork/server/v2026"
)

type syntheticTimeoutError struct{}

func (syntheticTimeoutError) Error() string   { return "synthetic write: i/o timeout" }
func (syntheticTimeoutError) Timeout() bool   { return true }
func (syntheticTimeoutError) Temporary() bool { return true }

func TestClientScoreExportRetriesOnlyFailedBoundedBatch(t *testing.T) {
	sets := make([]clientScoreRedisSet, 1025)
	for i := range sets {
		sets[i] = clientScoreRedisSet{key: fmt.Sprintf("key-%04d", i), value: []byte{byte(i)}}
	}
	var calls [][]string
	failedOnce := false
	err := runClientScoreExportBatches(
		context.Background(),
		sets,
		512,
		1<<30,
		3,
		func(batch []clientScoreRedisSet) error {
			keys := make([]string, len(batch))
			for i, set := range batch {
				keys[i] = set.key
			}
			calls = append(calls, keys)
			if batch[0].key == "key-0512" && !failedOnce {
				failedOnce = true
				return syntheticTimeoutError{}
			}
			return nil
		},
		func(context.Context, int) error { return nil },
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(calls) != 4 {
		t.Fatalf("batch calls = %d, want first + failed second + retry second + tail", len(calls))
	}
	wantFirstKeys := []string{"key-0000", "key-0512", "key-0512", "key-1024"}
	wantSizes := []int{512, 512, 512, 1}
	for i := range calls {
		if len(calls[i]) != wantSizes[i] || calls[i][0] != wantFirstKeys[i] {
			t.Fatalf("call %d = size %d first %q, want size %d first %q", i, len(calls[i]), calls[i][0], wantSizes[i], wantFirstKeys[i])
		}
	}
	if calls[1][511] != calls[2][511] {
		t.Fatal("retry did not preserve the exact failed batch")
	}
}

func TestClientScoreExportStreamsBeforeProducingWholeOperationList(t *testing.T) {
	const (
		setCount  = 10_000
		batchSize = 512
	)
	produced := 0
	executed := 0
	maxProducedAhead := 0
	batchSizes := []int{}
	err := runClientScoreExportStream(
		context.Background(),
		batchSize,
		1<<30,
		3,
		func(emit func(clientScoreRedisSet) error) error {
			for i := range setCount {
				produced++
				if err := emit(clientScoreRedisSet{key: fmt.Sprintf("key-%05d", i), value: make([]byte, 1024)}); err != nil {
					return err
				}
				maxProducedAhead = max(maxProducedAhead, produced-executed)
			}
			return nil
		},
		func(batch []clientScoreRedisSet) error {
			if len(batch) == 0 || len(batch) > batchSize {
				t.Fatalf("executed batch size %d outside [1,%d]", len(batch), batchSize)
			}
			batchSizes = append(batchSizes, len(batch))
			executed += len(batch)
			return nil
		},
		func(context.Context, int) error { return nil },
	)
	if err != nil {
		t.Fatal(err)
	}
	if executed != setCount || produced != setCount {
		t.Fatalf("produced=%d executed=%d, want %d each", produced, executed, setCount)
	}
	if maxProducedAhead >= batchSize {
		t.Fatalf("producer retained %d unexecuted sets, want at most %d", maxProducedAhead, batchSize-1)
	}
	if got, want := len(batchSizes), 20; got != want {
		t.Fatalf("batch count=%d, want %d", got, want)
	}
	if got, want := batchSizes[len(batchSizes)-1], setCount%batchSize; got != want {
		t.Fatalf("tail batch=%d, want %d", got, want)
	}
}

// A caller's blocked-network set changes only targets that actually contain a
// provider from that network. The production pass has hundreds of caller
// locations, so re-gob-encoding an unchanged target for every caller consumed
// four cores and allocated hundreds of MiB/s. Prove equivalent callers share
// one encoded payload while a genuinely filtered caller gets its own value.
func TestClientScoreTargetFanoutEncodesSharedPayloadOnce(t *testing.T) {
	callerUnfiltered := server.NewId()
	callerIrrelevantBlock := server.NewId()
	callerFiltered := server.NewId()
	networkA := server.NewId()
	networkB := server.NewId()
	networkAbsent := server.NewId()
	clientScores := map[server.Id]*ClientScore{
		server.NewId(): {NetworkId: networkA},
		server.NewId(): {NetworkId: networkB},
	}
	excludes := map[server.Id]map[server.Id]bool{
		callerIrrelevantBlock: {networkAbsent: true},
		callerFiltered:        {networkB: true},
	}

	encodeSizes := []int{}
	sampleEncodeSizes := []int{}
	valuesByKey := map[string]string{}
	err := emitClientScoreTargetFanout(
		[]server.Id{callerUnfiltered, callerIrrelevantBlock, callerFiltered},
		clientScores,
		excludes,
		clientScoreTargetKeys{
			counts: func(callerId server.Id) string { return "counts/" + callerId.String() },
			filter: func(callerId server.Id) string { return "filter/" + callerId.String() },
			sample: func(callerId server.Id, sampleIndex int) string {
				return fmt.Sprintf("sample/%s/%d", callerId, sampleIndex)
			},
			alias: func(callerId server.Id) string { return "alias/" + callerId.String() },
		},
		func(scores map[server.Id]*ClientScore) ([]byte, []byte, []int, func(int) []byte) {
			size := len(scores)
			encodeSizes = append(encodeSizes, size)
			return []byte(fmt.Sprintf("counts=%d", size)),
				[]byte(fmt.Sprintf("filter=%d", size)),
				[]int{size},
				func(int) []byte {
					sampleEncodeSizes = append(sampleEncodeSizes, size)
					return []byte(fmt.Sprintf("sample=%d", size))
				}
		},
		false,
		func(set clientScoreRedisSet) error {
			valuesByKey[set.key] = string(set.value)
			return nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := fmt.Sprint(encodeSizes), "[2 1]"; got != want {
		t.Fatalf("encoded provider-set sizes = %s, want %s (one shared baseline and one real filter)", got, want)
	}
	if got, want := fmt.Sprint(sampleEncodeSizes), "[2 1]"; got != want {
		t.Fatalf("sample encoded provider-set sizes = %s, want %s", got, want)
	}
	if got := valuesByKey[fmt.Sprintf("sample/%s/0", server.Id{})]; got != "sample=2" {
		t.Fatalf("canonical baseline sample = %q, want shared two-provider payload", got)
	}
	for _, callerId := range []server.Id{callerUnfiltered, callerIrrelevantBlock} {
		if _, ok := valuesByKey[fmt.Sprintf("sample/%s/0", callerId)]; ok {
			t.Fatalf("unchanged caller %s retained a duplicate sample payload", callerId)
		}
		if got := valuesByKey["alias/"+callerId.String()]; got != clientScoreAliasBaselineValue {
			t.Fatalf("unchanged caller %s alias = %q, want baseline alias", callerId, got)
		}
	}
	if got := valuesByKey[fmt.Sprintf("sample/%s/0", callerFiltered)]; got != "sample=1" {
		t.Fatalf("filtered caller sample = %q, want one-provider payload", got)
	}
	if got := valuesByKey["alias/"+callerFiltered.String()]; got != clientScoreAliasCallerValue {
		t.Fatalf("filtered caller alias = %q, want caller-specific alias", got)
	}
	if got, want := len(valuesByKey), 9; got != want {
		t.Fatalf("emitted key count = %d, want %d (two payloads plus three aliases)", got, want)
	}
}

// The first alias-aware pass refreshes caller-keyed values before publishing
// the schema-ready marker. This leaves the normal key TTL as a rolling-reader
// grace window; later passes use the sparse form above.
func TestClientScoreTargetFanoutCompatibilityPassKeepsLegacyPayloads(t *testing.T) {
	callerA := server.NewId()
	callerB := server.NewId()
	networkId := server.NewId()
	clientScores := map[server.Id]*ClientScore{
		server.NewId(): {NetworkId: networkId},
	}
	valuesByKey := map[string]string{}
	encodeCalls := 0
	sampleEncodeCalls := 0
	err := emitClientScoreTargetFanout(
		[]server.Id{{}, callerA, callerB},
		clientScores,
		nil,
		clientScoreTargetKeys{
			counts: func(callerId server.Id) string { return "counts/" + callerId.String() },
			filter: func(callerId server.Id) string { return "filter/" + callerId.String() },
			sample: func(callerId server.Id, sampleIndex int) string {
				return fmt.Sprintf("sample/%s/%d", callerId, sampleIndex)
			},
			alias: func(callerId server.Id) string { return "alias/" + callerId.String() },
		},
		func(map[server.Id]*ClientScore) ([]byte, []byte, []int, func(int) []byte) {
			encodeCalls++
			return []byte("counts"), []byte("filter"), []int{1}, func(int) []byte {
				sampleEncodeCalls++
				return []byte("sample")
			}
		},
		true,
		func(set clientScoreRedisSet) error {
			valuesByKey[set.key] = string(set.value)
			return nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if encodeCalls != 1 || sampleEncodeCalls != 1 {
		t.Fatalf("encode calls=%d sample calls=%d, want one shared encoding", encodeCalls, sampleEncodeCalls)
	}
	for _, callerId := range []server.Id{{}, callerA, callerB} {
		if got := valuesByKey[fmt.Sprintf("sample/%s/0", callerId)]; got != "sample" {
			t.Fatalf("compatibility payload for caller %s = %q, want sample", callerId, got)
		}
	}
	for _, callerId := range []server.Id{callerA, callerB} {
		if got := valuesByKey["alias/"+callerId.String()]; got != clientScoreAliasBaselineValue {
			t.Fatalf("compatibility alias for caller %s = %q, want baseline", callerId, got)
		}
	}
	if got, want := len(valuesByKey), 11; got != want {
		t.Fatalf("compatibility key count=%d, want %d", got, want)
	}
}

func TestSelectClientScorePayloadPreservesLegacyAndOverrides(t *testing.T) {
	callerId := server.NewId()
	for _, test := range []struct {
		name        string
		alias       string
		caller      string
		baseline    string
		wantCaller  server.Id
		wantPayload string
	}{
		{name: "legacy missing alias", caller: "legacy", baseline: "shared", wantCaller: callerId, wantPayload: "legacy"},
		{name: "explicit override", alias: clientScoreAliasCallerValue, caller: "override", baseline: "shared", wantCaller: callerId, wantPayload: "override"},
		{name: "shared baseline", alias: clientScoreAliasBaselineValue, caller: "legacy", baseline: "shared", wantCaller: server.Id{}, wantPayload: "shared"},
		{name: "evicted baseline falls back safely", alias: clientScoreAliasBaselineValue, caller: "legacy", wantCaller: callerId, wantPayload: "legacy"},
	} {
		t.Run(test.name, func(t *testing.T) {
			gotCaller, gotPayload := selectClientScorePayload(
				callerId,
				[]byte(test.alias),
				[]byte(test.caller),
				[]byte(test.baseline),
			)
			if gotCaller != test.wantCaller || string(gotPayload) != test.wantPayload {
				t.Fatalf("caller=%s payload=%q, want caller=%s payload=%q", gotCaller, gotPayload, test.wantCaller, test.wantPayload)
			}
		})
	}
}

func TestClientScoreExportFlushesAtByteBudget(t *testing.T) {
	var batchSizes []int
	var batchBytes []int
	err := runClientScoreExportStream(
		context.Background(),
		512,
		10,
		1,
		func(emit func(clientScoreRedisSet) error) error {
			for i := range 5 {
				// key + value = 5 bytes, so the ten-byte budget must flush
				// exactly two values at a time.
				if err := emit(clientScoreRedisSet{key: "k", value: []byte{1, 2, 3, byte(i)}}); err != nil {
					return err
				}
			}
			// An individually oversized value must be isolated and flushed
			// immediately; it cannot make the following tail retain it.
			if err := emit(clientScoreRedisSet{key: "k", value: make([]byte, 20)}); err != nil {
				return err
			}
			return emit(clientScoreRedisSet{key: "k", value: []byte{1}})
		},
		func(batch []clientScoreRedisSet) error {
			total := 0
			for _, set := range batch {
				total += len(set.key) + len(set.value)
			}
			batchSizes = append(batchSizes, len(batch))
			batchBytes = append(batchBytes, total)
			return nil
		},
		func(context.Context, int) error { return nil },
	)
	if err != nil {
		t.Fatal(err)
	}
	wantSizes := []int{2, 2, 1, 1, 1}
	wantBytes := []int{10, 10, 5, 21, 2}
	if fmt.Sprint(batchSizes) != fmt.Sprint(wantSizes) || fmt.Sprint(batchBytes) != fmt.Sprint(wantBytes) {
		t.Fatalf("batches sizes=%v bytes=%v, want sizes=%v bytes=%v", batchSizes, batchBytes, wantSizes, wantBytes)
	}
}

// A bounded batch is still a leak if its backing array keeps the flushed
// []byte values alive. Capture that array from the executor and prove the
// synchronous flush clears every payload slot before returning.
func TestClientScoreExportClearsPayloadReferencesAfterFlush(t *testing.T) {
	var flushedBacking []clientScoreRedisSet
	err := runClientScoreExportStream(
		context.Background(),
		3,
		1<<20,
		1,
		func(emit func(clientScoreRedisSet) error) error {
			for i := range 3 {
				if err := emit(clientScoreRedisSet{
					key:   fmt.Sprintf("key-%d", i),
					value: []byte{byte(i + 1)},
				}); err != nil {
					return err
				}
			}
			return nil
		},
		func(batch []clientScoreRedisSet) error {
			flushedBacking = batch[:cap(batch)]
			return nil
		},
		func(context.Context, int) error { return nil },
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(flushedBacking) != 3 {
		t.Fatalf("captured backing slots = %d, want 3", len(flushedBacking))
	}
	for i, set := range flushedBacking {
		if set.key != "" || set.value != nil {
			t.Fatalf("flushed slot %d retained payload: key=%q value=%v", i, set.key, set.value)
		}
	}
}

func TestClientScoreExportDoesNotRetryPermanentOrPoolErrors(t *testing.T) {
	for _, test := range []struct {
		name string
		err  error
	}{
		{name: "permanent", err: errors.New("WRONGTYPE")},
		{name: "pool backpressure", err: errors.New("redis: connection pool timeout")},
	} {
		t.Run(test.name, func(t *testing.T) {
			calls := 0
			err := runClientScoreExportBatches(
				context.Background(),
				[]clientScoreRedisSet{{key: "key"}},
				512,
				1<<20,
				3,
				func([]clientScoreRedisSet) error {
					calls++
					return test.err
				},
				func(context.Context, int) error {
					t.Fatal("permanent error waited for a retry")
					return nil
				},
			)
			if !errors.Is(err, test.err) || calls != 1 {
				t.Fatalf("err=%v calls=%d, want wrapped original error and one call", err, calls)
			}
		})
	}
}

func TestClientScoreExportCancellationStopsBeforeRetry(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	calls := 0
	err := runClientScoreExportBatches(
		ctx,
		[]clientScoreRedisSet{{key: "key"}},
		512,
		1<<20,
		3,
		func([]clientScoreRedisSet) error {
			calls++
			return syntheticTimeoutError{}
		},
		func(context.Context, int) error {
			cancel()
			return context.Canceled
		},
	)
	if !errors.Is(err, context.Canceled) || calls != 1 {
		t.Fatalf("err=%v calls=%d, want cancellation after first call", err, calls)
	}
}

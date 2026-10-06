package model

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/gob"
	"errors"
	"fmt"
	"hash/fnv"
	"net"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/urnetwork/server/v2026"
)

// These controls exercise the production fanout, native writer, bounded SET
// stream, and go-redis command/pipeline paths. Hooks replace only transport:
// no credentials, SQL, sockets, Redis server, or wall-clock sleeps are used.
// The deterministic RTT ledger is a sensitivity control, not Main latency.
const nativeFirstTTL = 37*time.Minute + 250*time.Millisecond

var nativeFirstFailure = errors.New("synthetic native alias transport failure")

type nativeFirstWork struct {
	Commands, DirectCalls, Pipelines, Sets, Aliases, DirectAliases int
	Begins, PageBatches, Pages, Commits, Encodes, SampleEncodes    int
	MaxPipelineCommands, MaxPipelineBytes                          int
	NativePipelines, ReplaySubmissions, MaxNativePages, MaxNativePageBytes int
	NativeRows int
	NativeSemanticSum uint64
	SetKeySum                                                      uint64
	TransportBudget                                                time.Duration
}

type nativeFirstStage struct {
	generation string
	expected   string
	pages      map[string][sha256.Size]byte
	committed  bool
}

type nativeFirstTransport struct {
	work          nativeFirstWork
	pointers      map[string]string
	stages        map[string]*nativeFirstStage
	failure       string
	census        *ClientScoreNativeCensus
	retryAttempts int
	retryDigest   [sha256.Size]byte
	cancel context.CancelFunc
}

func newNativeFirstTransport(t *testing.T, failure string) (*redis.Client, *nativeFirstTransport) {
	t.Helper()
	transport := &nativeFirstTransport{
		pointers: map[string]string{}, stages: map[string]*nativeFirstStage{}, failure: failure,
		census: &ClientScoreNativeCensus{
			PublicationId:     "native-export-synthetic-control",
			SourceStartedAt:   time.Unix(1_700_000_000, 0).UTC(),
			SourceCompletedAt: time.Unix(1_700_000_010, 0).UTC(),
		},
	}
	client := redis.NewClient(&redis.Options{Addr: "forbidden-synthetic.invalid:0", MaxRetries: -1})
	client.AddHook(transport)
	t.Cleanup(func() { client.Close() })
	return client, transport
}

func (h *nativeFirstTransport) DialHook(redis.DialHook) redis.DialHook {
	return func(context.Context, string, string) (net.Conn, error) {
		return nil, errors.New("native transport control forbids dialing")
	}
}

func (h *nativeFirstTransport) ProcessHook(redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		h.work.DirectCalls++
		h.work.TransportBudget += 2 * time.Millisecond
		return h.execute(ctx, cmd, true)
	}
}

func (h *nativeFirstTransport) ProcessPipelineHook(redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return func(ctx context.Context, cmds []redis.Cmder) error {
		h.work.Pipelines++
		h.work.TransportBudget += 2 * time.Millisecond
		if len(cmds) > 0 && cmds[0].Name() == "eval" {
			h.work.NativePipelines++
			return h.executeFirstBatch(ctx, cmds)
		}
		size := 0
		for _, cmd := range cmds {
			args := cmd.Args()
			if cmd.Name() != "set" {
				return fmt.Errorf("unexpected pipelined native command %s", cmd.Name())
			}
			size += len(fmt.Sprint(args[1])) + len(nativeFirstBytes(args[2]))
		}
		h.work.MaxPipelineCommands = max(h.work.MaxPipelineCommands, len(cmds))
		h.work.MaxPipelineBytes = max(h.work.MaxPipelineBytes, size)
		if len(cmds) > clientScoreExportBatchSize || size > clientScoreExportBatchBytes {
			return fmt.Errorf("SET stream exceeded its existing command/byte budget: %d/%d", len(cmds), size)
		}
		if h.failure == "retry_alias" {
			hasAlias := false
			digest := sha256.New()
			for _, cmd := range cmds {
				for _, arg := range cmd.Args() {
					value := nativeFirstBytes(arg)
					fmt.Fprintf(digest, "%d:", len(value))
					digest.Write(value)
				}
				hasAlias = hasAlias || strings.HasSuffix(fmt.Sprint(cmd.Args()[1]), ":native_v1")
			}
			if hasAlias {
				h.retryAttempts++
				var got [sha256.Size]byte
				copy(got[:], digest.Sum(nil))
				if h.retryAttempts == 1 {
					h.retryDigest = got
				} else if got != h.retryDigest {
					return fmt.Errorf("alias retry changed exact keys, values, order, or TTL")
				}
			}
		}
		var result error
		for _, cmd := range cmds {
			result = errors.Join(result, h.execute(ctx, cmd, false))
		}
		return result
	}
}

func nativeFirstBytes(value any) []byte {
	if raw, ok := value.([]byte); ok {
		return raw
	}
	return []byte(fmt.Sprint(value))
}

func nativeFirstBaselineKey(key string) string {
	parts := strings.Split(key, "_")
	parts[3] = (server.Id{}).String()
	return strings.Join(parts, "_")
}

func (h *nativeFirstTransport) execute(ctx context.Context, cmd redis.Cmder, direct bool) (result error) {
	defer func() { cmd.SetErr(result) }()
	if err := ctx.Err(); err != nil {
		return err
	}
	h.work.Commands++
	h.work.TransportBudget += 5 * time.Microsecond
	args := cmd.Args()
	switch cmd.Name() {
	case "get":
		value, ok := h.pointers[fmt.Sprint(args[1])]
		cmd.(*redis.StringCmd).SetVal(value)
		if !ok {
			return redis.Nil
		}
	case "set":
		key, value := fmt.Sprint(args[1]), nativeFirstBytes(args[2])
		if len(args) != 5 || args[3] != "px" || fmt.Sprint(args[4]) != strconv.FormatInt(nativeFirstTTL.Milliseconds(), 10) {
			return fmt.Errorf("alias or payload lost exact TTL")
		}
		h.work.Sets++
		digest := fnv.New64a()
		digest.Write([]byte(key))
		h.work.SetKeySum += digest.Sum64()
		if strings.HasSuffix(key, ":native_v1") {
			h.work.Aliases++
			if direct {
				h.work.DirectAliases++
			}
			if string(value) != clientScoreNativeBaseline {
				return fmt.Errorf("native alias changed its versioned value")
			}
			baseline := nativeFirstBaselineKey(key)
			pointer, err := parseClientScoreNativePointer(h.pointers[baseline])
			if err != nil || !h.stages[clientScoreNativeSlotKey(baseline, pointer.slot)].committed {
				return fmt.Errorf("native alias became visible before its baseline committed")
			}
			if h.failure == "alias" {
				return nativeFirstFailure
			}
			if h.failure == "retry_alias" && h.retryAttempts == 1 {
				return syntheticTimeoutError{}
			}
		}
		cmd.(*redis.StatusCmd).SetVal("OK")
	case "eval":
		if len(args) < 9 || fmt.Sprint(args[2]) != "2" {
			return fmt.Errorf("native script changed its guarded two-key boundary")
		}
		key, slotKey, script := fmt.Sprint(args[3]), fmt.Sprint(args[4]), fmt.Sprint(args[1])
		values := args[5:]
		if fmt.Sprint(values[2]) != strconv.FormatInt(nativeFirstTTL.Milliseconds(), 10) {
			return fmt.Errorf("native stage or commit lost exact TTL")
		}
		switch script {
		case clientScoreNativeBeginScript:
			h.work.Begins++
			if h.failure == "begin_error" {
				return nativeFirstFailure
			}
			if h.failure == "begin_zero" || h.pointers[key] != fmt.Sprint(values[0]) || strings.HasPrefix(h.pointers[key], fmt.Sprint(values[3])+":") {
				cmd.(*redis.Cmd).SetVal(int64(0))
				return nil
			}
			h.stages[slotKey] = &nativeFirstStage{generation: fmt.Sprint(values[1]), expected: fmt.Sprint(values[0]), pages: map[string][sha256.Size]byte{}}
		case clientScoreNativePagesScript:
			h.work.PageBatches++
			if h.failure == "page_error" || (h.failure == "later_page_error" && h.work.PageBatches == 2) {
				return nativeFirstFailure
			}
			stage := h.stages[slotKey]
			if h.failure == "page_zero" || stage == nil || stage.generation != values[1] || stage.expected != values[0] || h.pointers[key] != stage.expected {
				cmd.(*redis.Cmd).SetVal(int64(0))
				return nil
			}
			pageBytes := 0
			for i := 3; i < len(values); i += 2 {
				pageBytes += len(fmt.Sprint(values[i])) + len(nativeFirstBytes(values[i+1]))
			}
			pageCount := (len(values) - 3) / 2
			h.work.MaxNativePages = max(h.work.MaxNativePages, pageCount)
			h.work.MaxNativePageBytes = max(h.work.MaxNativePageBytes, pageBytes)
			if pageCount > clientScoreExportBatchSize || (pageBytes > clientScoreExportBatchBytes && pageCount != 1) {
				return fmt.Errorf("native PAGE batch exceeded existing count/byte guard")
			}
			for i := 3; i < len(values); i += 2 {
				field, value := fmt.Sprint(values[i]), nativeFirstBytes(values[i+1])
				if len(value) < sha256.Size {
					return fmt.Errorf("native page lost checksum")
				}
				checksum := sha256.Sum256(value[sha256.Size:])
				if !bytes.Equal(checksum[:], value[:sha256.Size]) {
					return fmt.Errorf("native page checksum mismatch")
				}
				if _, duplicate := stage.pages[field]; duplicate {
					return fmt.Errorf("native page was written twice")
				}
				stage.pages[field] = checksum
				h.work.Pages++
				var rows []*ClientScore
				if err := gob.NewDecoder(bytes.NewReader(value[sha256.Size:])).Decode(&rows); err != nil {
					return err
				}
				for _, row := range rows {
					digest := fnv.New64a()
					fmt.Fprintf(digest, "%x:%x:%d:%t:%t:%g:%g:%g:%g:%g:%g:%t:%t:%d:%d:%d:%d", row.ClientId, row.NetworkId, row.IpFamilies, row.Online, row.NetworkOnly, row.ReliabilityWeight, row.IndependentReliabilityWeight, row.UrlProbeSuccessWeight, row.ScaledWeights[RankModeQuality], row.ScaledWeights[RankModeSpeed], float64(row.MinRelativeLatencyMillis), row.PassesMinimums[RankModeQuality], row.PassesMinimums[RankModeSpeed], row.Scores[RankModeQuality], row.Scores[RankModeSpeed], row.Tiers[RankModeQuality], row.Tiers[RankModeSpeed])
					h.work.NativeRows++
					h.work.NativeSemanticSum += digest.Sum64()
				}
			}
		case clientScoreNativeCommitScript:
			h.work.Commits++
			if h.failure == "commit" {
				return nativeFirstFailure
			}
			stage := h.stages[slotKey]
			if stage == nil || stage.generation != values[1] || h.pointers[key] != stage.expected || len(stage.pages) != (len(values)-5)/2 {
				return fmt.Errorf("native commit bypassed complete-generation guard")
			}
			var manifest clientScoreNativeManifest
			if err := gob.NewDecoder(bytes.NewReader(nativeFirstBytes(values[3]))).Decode(&manifest); err != nil {
				return err
			}
			if manifest.Generation != stage.generation || manifest.PublicationId != h.census.PublicationId || !manifest.SourceStartedAt.Equal(h.census.SourceStartedAt) || !manifest.SourceCompletedAt.Equal(h.census.SourceCompletedAt) {
				return fmt.Errorf("native commit changed census provenance")
			}
			for i := 5; i < len(values); i += 2 {
				checksum, exists := stage.pages[fmt.Sprint(values[i])]
				if !exists || !bytes.Equal(checksum[:], nativeFirstBytes(values[i+1])) {
					return fmt.Errorf("native commit lost expected page checksum")
				}
			}
			h.pointers[key] = fmt.Sprint(values[4])
			stage.committed = true
		default:
			return fmt.Errorf("unexpected native script")
		}
		cmd.(*redis.Cmd).SetVal(int64(1))
	default:
		return fmt.Errorf("unexpected transport command %s", cmd.Name())
	}
	return nil
}

func nativeFirstID(n int) server.Id {
	var id server.Id
	binary.BigEndian.PutUint64(id[8:], uint64(n))
	return id
}

func nativeFirstKeys(force bool, mode RankMode, target server.Id) clientScoreTargetKeys {
	return clientScoreTargetKeys{
		counts: func(caller server.Id) string { return clientScoreLocationCountsKey(force, mode, target, caller) },
		filter: func(caller server.Id) string { return clientScoreLocationFilterKey(force, mode, target, caller) },
		sample: func(caller server.Id, index int) string {
			return clientScoreLocationSampleKey(force, mode, target, caller, index)
		},
		alias: func(caller server.Id) string { return clientScoreLocationAliasKey(force, mode, target, caller) },
		facetCounts: func(caller server.Id, facet ipFamilyFacet) string {
			return clientScoreLocationFacetCountsKey(force, mode, target, caller, facet)
		},
		facetSample: func(caller server.Id, facet ipFamilyFacet, index int) string {
			return clientScoreLocationFacetSampleKey(force, mode, target, caller, facet, index)
		},
	}
}

// Stable identities, real gob pages, three family facets, and sparse exclusions
// keep the control representative of fanout work without using the SQL source.
func nativeFirstPayload(h *nativeFirstTransport, scores map[server.Id]*ClientScore, force bool, mode RankMode) clientScoreExportPayload {
	h.work.Encodes++
	payload := clientScoreExportPayload{filterBytes: []byte("synthetic-filter"), facets: map[ipFamilyFacet]clientScoreFacetPayload{}, nativeFacets: map[ipFamilyFacet]clientScoreFacetPayload{}}
	for _, facet := range ipFamilyFacets {
		for _, native := range []bool{false, true} {
			rows := []*ClientScore{}
			for _, score := range scores {
				if score.ipFamilyFacet() == facet && ((!native && (force || score.Online || score.PassesMinimums[mode])) || (native && !force && score.PassesMinimums[mode])) {
					rows = append(rows, score)
				}
			}
			slices.SortFunc(rows, func(a, b *ClientScore) int { return bytes.Compare(a.ClientId[:], b.ClientId[:]) })
			part := clientScoreFacetPayload{countsBytes: []byte("synthetic-counts")}
			for start := 0; start < len(rows); start += ClientScoreSampleCount {
				part.counts = append(part.counts, min(ClientScoreSampleCount, len(rows)-start))
			}
			part.encodeSample = func(index int) []byte {
				h.work.SampleEncodes++
				start := index * ClientScoreSampleCount
				var encoded bytes.Buffer
				if err := gob.NewEncoder(&encoded).Encode(rows[start:min(start+ClientScoreSampleCount, len(rows))]); err != nil {
					panic(err)
				}
				return encoded.Bytes()
			}
			if native {
				payload.nativeFacets[facet] = part
			} else {
				payload.facets[facet] = part
			}
		}
	}
	return payload
}

func nativeFirstRunWorker(t *testing.T, ctx context.Context, baseline bool, failure string, targetOffset, targets, callers int, barrier *sync.WaitGroup) (nativeFirstWork, error) {
	r, h := newNativeFirstTransport(t, failure)
	if barrier != nil {
		barrier.Done()
		barrier.Wait()
	}
	callerIDs := make([]server.Id, callers)
	exclusions := map[server.Id]map[server.Id]bool{}
	for i := range callerIDs {
		callerIDs[i] = nativeFirstID(i + 10_000)
		if i%256 == 0 {
			exclusions[callerIDs[i]] = map[server.Id]bool{nativeFirstID(1): true}
		}
	}
	scores := map[server.Id]*ClientScore{}
	for i := range 60 {
		id := nativeFirstID(i + 100)
		scores[id] = &ClientScore{ClientId: id, NetworkId: nativeFirstID(i%2 + 1), Online: true,
			IpFamilies: uint8(i%3 + 1), ReliabilityWeight: 1, IndependentReliabilityWeight: 1,
			PassesMinimums: map[RankMode]bool{RankModeQuality: i%2 == 0, RankModeSpeed: i%5 != 0},
			Scores:         map[RankMode]int{RankModeQuality: 0, RankModeSpeed: 1},
			Tiers:          map[RankMode]int{RankModeQuality: 0, RankModeSpeed: 1},
			ScaledWeights:  map[RankMode]float32{RankModeQuality: 1, RankModeSpeed: 1},
		}
	}
	for _, force := range []bool{false, true} {
		for _, mode := range []RankMode{RankModeQuality, RankModeSpeed} {
			err := writeClientScoreRedisStream(ctx, r, nativeFirstTTL, func(emit func(clientScoreRedisSet) error) error {
				for target := range targets {
					keys := nativeFirstKeys(force, mode, nativeFirstID(target+targetOffset+20_000))
					var native *clientScoreNativeFanout
					if !force {
						native = newClientScoreNativeFanout(ctx, r, keys.counts, nativeFirstTTL, h.census, emit)
						if baseline {
							// Exact a7 snapshot body, retaining the existing batched aliases.
							native.publish = func(caller server.Id, facets map[ipFamilyFacet]clientScoreFacetPayload) error {
								return nativeFirstBaselineSnapshot(ctx, r, clientScoreNativeKey(keys.counts(caller)), nativeFirstTTL, facets, h.census)
							}
						}
					}
					if err := emitClientScoreTargetFanout(callerIDs, scores, exclusions, keys,
						func(filtered map[server.Id]*ClientScore) clientScoreExportPayload {
							return nativeFirstPayload(h, filtered, force, mode)
						},
						false, false, emit, native); err != nil {
						return err
					}
				}
				return nil
			})
			if err != nil {
				return h.work, err
			}
		}
	}
	return h.work, nil
}

func nativeFirstAdd(total *nativeFirstWork, work nativeFirstWork) {
	total.Commands += work.Commands
	total.DirectCalls += work.DirectCalls
	total.Pipelines += work.Pipelines
	total.Sets += work.Sets
	total.Aliases += work.Aliases
	total.DirectAliases += work.DirectAliases
	total.Begins += work.Begins
	total.PageBatches += work.PageBatches
	total.Pages += work.Pages
	total.Commits += work.Commits
	total.Encodes += work.Encodes
	total.SampleEncodes += work.SampleEncodes
	total.SetKeySum += work.SetKeySum
	total.MaxPipelineCommands = max(total.MaxPipelineCommands, work.MaxPipelineCommands)
	total.MaxPipelineBytes = max(total.MaxPipelineBytes, work.MaxPipelineBytes)
	total.TransportBudget = max(total.TransportBudget, work.TransportBudget)
	total.NativePipelines += work.NativePipelines
	total.ReplaySubmissions += work.ReplaySubmissions
	total.MaxNativePages = max(total.MaxNativePages, work.MaxNativePages)
	total.MaxNativePageBytes = max(total.MaxNativePageBytes, work.MaxNativePageBytes)
	total.NativeRows += work.NativeRows
	total.NativeSemanticSum += work.NativeSemanticSum
}

// Frozen a7 snapshot body for the before/after transport control.
// Source SHA256 5ff82220a01bf6212e7b18e706bd3e4377b68e6bad6ec4209104066ca0ded381.
func nativeFirstBaselineSnapshot(ctx context.Context, r server.RedisClient, key string, ttl time.Duration, facets map[ipFamilyFacet]clientScoreFacetPayload, sources ...*ClientScoreNativeCensus) error {
	publication, err := beginClientScoreNativePublication(ctx, r, key, ttl)
	if err != nil {
		return err
	}
	manifest := clientScoreNativeManifest{Generation: publication.generation, Facets: map[ipFamilyFacet][]clientScoreNativePageInfo{}}
	if 0 < len(sources) && sources[0] != nil {
		manifest.PublicationId = sources[0].PublicationId
		manifest.SourceStartedAt = sources[0].SourceStartedAt
		manifest.SourceCompletedAt = sources[0].SourceCompletedAt
	}
	err = runClientScoreExportStream(ctx, clientScoreExportBatchSize, clientScoreExportBatchBytes, 1,
		func(emit func(clientScoreRedisSet) error) error {
			for _, facet := range ipFamilyFacets {
				payload, ok := facets[facet]
				if !ok {
					return fmt.Errorf("native client score publication omitted a facet")
				}
				manifest.Facets[facet] = []clientScoreNativePageInfo{}
				for index, count := range payload.counts {
					if count <= 0 || ClientScoreSampleCount < count {
						return fmt.Errorf("native client score publication has invalid page size")
					}
					data := payload.encodeSample(index)
					checksum := sha256.Sum256(data)
					manifest.Facets[facet] = append(manifest.Facets[facet], clientScoreNativePageInfo{Count: count, Checksum: checksum})
					value := append(checksum[:sha256.Size:sha256.Size], data...)
					if err := emit(clientScoreRedisSet{key: clientScoreNativePageField(facet, index), value: value}); err != nil {
						return err
					}
				}
			}
			return nil
		},
		func(pages []clientScoreRedisSet) error { return publication.writePages(ctx, r, pages) },
		func(context.Context, int) error { return nil },
	)
	if err != nil {
		return err
	}
	return publication.commit(ctx, r, manifest)
}

const nativeFirstLastGood = "0:last-good:0000000000000000000000000000000000000000000000000000000000000000"
const nativeFirstWinner = "1:winner:0000000000000000000000000000000000000000000000000000000000000000"

// This is an explicit model of the unchanged Lua guards, not Redis execution.
// A replay repeats the same command arguments and rebuilds the inactive slot.
func (h *nativeFirstTransport) executeFirstBatch(ctx context.Context, cmds []redis.Cmder) error {
	if len(cmds) != 2 || cmds[0].Name() != "eval" || cmds[1].Name() != "eval" {
		return fmt.Errorf("native pipeline must contain exactly BEGIN then PAGE")
	}
	begin, page := cmds[0].Args(), cmds[1].Args()
	if len(begin) != 9 || len(page) < 10 || begin[1] != clientScoreNativeBeginScript || page[1] != clientScoreNativePagesScript {
		return fmt.Errorf("native pipeline changed Lua or command order")
	}
	for _, i := range []int{2, 3, 4, 5, 6, 7} {
		if fmt.Sprint(begin[i]) != fmt.Sprint(page[i]) {
			return fmt.Errorf("native pipeline split its keys, expected pointer, generation, or TTL")
		}
	}
	key, slot := fmt.Sprint(begin[3]), fmt.Sprint(begin[4])
	if h.failure == "exec_before" {
		return nativeFirstFailure
	}
	if h.failure == "pointer_before" {
		h.pointers[key] = nativeFirstWinner
	}
	digestCommands := func() [sha256.Size]byte {
		digest := sha256.New()
		for _, command := range cmds {
			for _, arg := range command.Args() {
				value := nativeFirstBytes(arg)
				fmt.Fprintf(digest, "%d:", len(value))
				digest.Write(value)
			}
		}
		var result [sha256.Size]byte
		copy(result[:], digest.Sum(nil))
		return result
	}
	wantDigest := digestCommands()
	attempts := 1
	if h.failure == "replay" {
		attempts = 2
	}
	for attempt := range attempts {
		if digestCommands() != wantDigest {
			return fmt.Errorf("native replay changed exact command arguments")
		}
		if attempt != 0 {
			h.work.Pipelines++
			h.work.NativePipelines++
			h.work.ReplaySubmissions++
			h.work.TransportBudget += 2 * time.Millisecond
		}
		var result error
		for i, command := range cmds {
			if i == 1 && h.failure == "pointer_between" {
				h.pointers[key] = nativeFirstWinner
			}
			if i == 1 && h.failure == "generation_between" && h.stages[slot] != nil {
				h.stages[slot].generation = "competing-stage"
			}
			// Redis executes the second queued command even if the first
			// command has an error. The caller must check Exec and both replies.
			result = errors.Join(result, h.execute(ctx, command, false))
		}
		if result != nil {
			return result
		}
		if attempt == 0 && attempts == 2 {
			// The server accepted both commands, but their reply was lost.
			// Before the transport replays them, no active pointer may move.
			stage := h.stages[slot]
			if h.pointers[key] != fmt.Sprint(begin[5]) || stage == nil || stage.generation != fmt.Sprint(begin[6]) || len(stage.pages) != (len(page)-8)/2 {
				return fmt.Errorf("unacknowledged first pair crossed its staging boundary")
			}
		}
	}
	if h.failure == "exec_after" {
		return nativeFirstFailure // Pair executed; its acknowledgement was lost.
	}
	if h.failure == "missing_begin_reply" {
		cmds[0].(*redis.Cmd).SetVal(nil)
	}
	if h.failure == "missing_page_reply" {
		cmds[1].(*redis.Cmd).SetVal(nil)
	}
	if h.failure == "malformed_begin_reply" {
		cmds[0].(*redis.Cmd).SetVal("not-an-integer")
	}
	if h.failure == "malformed_page_reply" {
		cmds[1].(*redis.Cmd).SetVal("not-an-integer")
	}
	if h.failure == "begin_command_error" {
		cmds[0].SetErr(nativeFirstFailure)
	}
	if h.failure == "page_command_error" {
		cmds[1].SetErr(nativeFirstFailure)
	}
	if h.failure == "cancel_after_pair" && h.cancel != nil {
		h.cancel()
	}
	return nil
}

func nativeFirstFacets(h *nativeFirstTransport, pages, padding int) map[ipFamilyFacet]clientScoreFacetPayload {
	facets := map[ipFamilyFacet]clientScoreFacetPayload{}
	for _, facet := range ipFamilyFacets {
		facets[facet] = clientScoreFacetPayload{}
	}
	counts := make([]int, pages)
	for i := range counts {
		counts[i] = 1
	}
	paddingValue := strings.Repeat("x", padding)
	facets[ipFamilyFacetV4Only] = clientScoreFacetPayload{
		counts: counts,
		encodeSample: func(index int) []byte {
			h.work.SampleEncodes++
			score := &ClientScore{
				ClientId: nativeFirstID(index + 100), NetworkId: nativeFirstID(1),
				IpFamilies: 1, Online: true, ReputationFailedNames: paddingValue,
				PassesMinimums: map[RankMode]bool{RankModeQuality: true, RankModeSpeed: true},
			}
			var encoded bytes.Buffer
			if err := gob.NewEncoder(&encoded).Encode([]*ClientScore{score}); err != nil {
				panic(err)
			}
			return encoded.Bytes()
		},
	}
	return facets
}

func nativeFirstCompletion(ctx context.Context, err error) error {
	workerErrors := make(chan error, 1)
	if err != nil {
		workerErrors <- err
	}
	close(workerErrors)
	return finishClientScoreExport(ctx, workerErrors)
}

func TestNativeFirstPageLoadedTransportControl(t *testing.T) {
	const workers, targetsPerWorker, callers = 48, 2, 1024
	var totals [2]nativeFirstWork
	for variant, baseline := range []bool{true, false} {
		var barrier sync.WaitGroup
		barrier.Add(workers)
		type outcome struct {
			work nativeFirstWork
			err  error
		}
		results := make(chan outcome, workers)
		for workerIndex := range workers {
			go func() {
				work, err := nativeFirstRunWorker(t, t.Context(), baseline, "", workerIndex*targetsPerWorker, targetsPerWorker, callers, &barrier)
				results <- outcome{work, err}
			}()
		}
		for range workers {
			result := <-results
			if result.err != nil {
				t.Fatal(result.err)
			}
			nativeFirstAdd(&totals[variant], result.work)
		}
	}
	before, after := totals[0], totals[1]
	const nonempty = workers * targetsPerWorker * 6
	for _, work := range totals {
		if work.Commands != 605952 || work.Aliases != 195840 || work.Commits != 960 || work.Begins != 960 || work.Encodes != 1920 || work.SampleEncodes != 7488 || work.PageBatches != nonempty || work.DirectAliases != 0 {
			t.Fatalf("loaded logical command work changed: %+v", work)
		}
	}
	if before.Sets != after.Sets || before.SetKeySum != after.SetKeySum || before.Pages != after.Pages || before.NativeRows != after.NativeRows || before.NativeSemanticSum != after.NativeSemanticSum {
		t.Fatalf("native first-page batching changed payload membership or SET keys: before=%+v after=%+v", before, after)
	}
	if before.NativePipelines != 0 || after.NativePipelines != nonempty || before.DirectCalls-after.DirectCalls != 2*nonempty || after.Pipelines-before.Pipelines != nonempty {
		t.Fatalf("native first-page submission reduction differs: before=%+v after=%+v", before, after)
	}
	if before.DirectCalls+before.Pipelines != 5568 || after.DirectCalls+after.Pipelines != 4992 || before.TransportBudget-after.TransportBudget != 24*time.Millisecond {
		t.Fatalf("modeled transport ledger differs: before=%+v after=%+v", before, after)
	}
	t.Logf("native first-page loaded control: commands=%d snapshots=%d nonempty=%d sample_encodes=%d direct_before=%d direct_after=%d pipelines_before=%d pipelines_after=%d submissions_before=%d submissions_after=%d max_SET_commands=%d max_SET_bytes=%d max_PAGE_fields=%d max_PAGE_bytes=%d modeled_max_worker_before=%s modeled_max_worker_after=%s; max fixed-worker chain, 2ms/submission + 5us/command, not Main latency", after.Commands, after.Commits, nonempty, after.SampleEncodes, before.DirectCalls, after.DirectCalls, before.Pipelines, after.Pipelines, before.DirectCalls+before.Pipelines, after.DirectCalls+after.Pipelines, after.MaxPipelineCommands, after.MaxPipelineBytes, after.MaxNativePages, after.MaxNativePageBytes, before.TransportBudget, after.TransportBudget)
}

func TestNativeFirstPageSnapshotShapes(t *testing.T) {
	for _, shape := range []struct {
		name                 string
		pages, padding, batches int
	}{
		{name: "empty"},
		{name: "one", pages: 1, batches: 1},
		{name: "exact_count_bound", pages: 512, batches: 1},
		{name: "multiple_count_batches", pages: 513, batches: 2},
		{name: "multiple_byte_batches", pages: 3, padding: clientScoreExportBatchBytes/2 + 1024, batches: 3},
		{name: "oversized_single_value", pages: 1, padding: clientScoreExportBatchBytes + 4096, batches: 1},
	} {
		t.Run(shape.name, func(t *testing.T) {
			r, h := newNativeFirstTransport(t, "")
			key := clientScoreNativeKey(nativeFirstKeys(false, RankModeQuality, nativeFirstID(20000)).counts(server.Id{}))
			err := writeClientScoreNativeSnapshot(t.Context(), r, key, nativeFirstTTL, nativeFirstFacets(h, shape.pages, shape.padding), h.census)
			if err != nil {
				t.Fatal(err)
			}
			wantPipeline, wantDirect := 0, 3
			if shape.pages != 0 {
				wantPipeline, wantDirect = 1, shape.batches+1
			}
			if h.work.Begins != 1 || h.work.Commits != 1 || h.work.PageBatches != shape.batches || h.work.Pages != shape.pages || h.work.NativePipelines != wantPipeline || h.work.DirectCalls != wantDirect {
				t.Fatalf("snapshot shape changed: %+v", h.work)
			}
			if shape.name == "oversized_single_value" && (h.work.MaxNativePages != 1 || h.work.MaxNativePageBytes <= clientScoreExportBatchBytes) {
				t.Fatal("oversized value control did not exercise the existing exception")
			}
			pointer, err := parseClientScoreNativePointer(h.pointers[key])
			if err != nil || !h.stages[clientScoreNativeSlotKey(key, pointer.slot)].committed {
				t.Fatalf("successful snapshot lacks a committed pointer: %v", err)
			}
		})
	}
}

func TestNativeFirstPageFailureBoundary(t *testing.T) {
	for _, failure := range []string{
		"begin_zero", "begin_error", "page_zero", "page_error", "exec_before", "exec_after",
		"missing_begin_reply", "missing_page_reply", "malformed_begin_reply", "malformed_page_reply", "begin_command_error", "page_command_error",
		"pointer_before", "pointer_between", "generation_between",
		"canceled_before", "cancel_after_pair", "later_page_error", "commit",
	} {
		t.Run(failure, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			r, h := newNativeFirstTransport(t, failure)
			h.cancel = cancel
			keys := nativeFirstKeys(false, RankModeQuality, nativeFirstID(20000))
			key := clientScoreNativeKey(keys.counts(server.Id{}))
			h.pointers[key] = nativeFirstLastGood
			h.stages[clientScoreNativeSlotKey(key, 0)] = &nativeFirstStage{generation: "last-good", committed: true, pages: map[string][sha256.Size]byte{}}
			if failure == "canceled_before" {
				cancel()
			}
			pages := 1
			if failure == "later_page_error" {
				pages = 513
			}
			aliasEnqueues, finishCallbacks := 0, 0
			err := writeClientScoreRedisStream(ctx, r, nativeFirstTTL, func(emit func(clientScoreRedisSet) error) error {
				native := newClientScoreNativeFanout(ctx, r, keys.counts, nativeFirstTTL, h.census, func(set clientScoreRedisSet) error {
					aliasEnqueues++
					return emit(set)
				})
				if err := native.publish(server.Id{}, nativeFirstFacets(h, pages, 0)); err != nil {
					return err
				}
				return native.alias(nativeFirstID(10000))
			})
			completion := nativeFirstCompletion(ctx, err)
			if completion == nil {
				finishCallbacks++ // Sentinel for the existing readiness/census condition.
			}
			wantPointer := nativeFirstLastGood
			if failure == "pointer_before" || failure == "pointer_between" {
				wantPointer = nativeFirstWinner
			}
			wantCommitAttempts := 0
			if failure == "commit" {
				wantCommitAttempts = 1
			}
			if completion == nil || finishCallbacks != 0 || aliasEnqueues != 0 || h.work.Aliases != 0 || h.work.Commits != wantCommitAttempts || h.pointers[key] != wantPointer {
				t.Fatalf("failed native batch crossed publication/alias/completion boundary: failure=%s callbacks=%d aliases=%d work=%+v err=%v", failure, finishCallbacks, aliasEnqueues, h.work, completion)
			}
			if strings.HasPrefix(failure, "cancel") && !errors.Is(completion, context.Canceled) {
				t.Fatalf("native cancellation lost: %v", completion)
			}
		})
	}
}

func TestNativeFirstPageReplayStaging(t *testing.T) {
	r, h := newNativeFirstTransport(t, "replay")
	key := clientScoreNativeKey(nativeFirstKeys(false, RankModeQuality, nativeFirstID(20000)).counts(server.Id{}))
	h.pointers[key] = nativeFirstLastGood
	h.stages[clientScoreNativeSlotKey(key, 0)] = &nativeFirstStage{generation: "last-good", committed: true, pages: map[string][sha256.Size]byte{}}
	if err := writeClientScoreNativeSnapshot(t.Context(), r, key, nativeFirstTTL, nativeFirstFacets(h, 1, 0), h.census); err != nil {
		t.Fatal(err)
	}
	pointer, err := parseClientScoreNativePointer(h.pointers[key])
	if err != nil || pointer.slot != 1 || h.work.ReplaySubmissions != 1 || h.work.Begins != 2 || h.work.PageBatches != 2 || h.work.Pages != 2 || h.work.Commits != 1 || len(h.stages) != 2 || len(h.stages[clientScoreNativeSlotKey(key, 1)].pages) != 1 {
		t.Fatalf("replayed first batch failed to reconstruct one inactive generation: work=%+v slots=%d err=%v", h.work, len(h.stages), err)
	}
}

func TestNativeFirstPageBeginCompatibility(t *testing.T) {
	r, h := newNativeFirstTransport(t, "")
	key := clientScoreNativeKey(nativeFirstKeys(false, RankModeQuality, nativeFirstID(20000)).counts(server.Id{}))
	publication, err := beginClientScoreNativePublication(t.Context(), r, key, nativeFirstTTL)
	if err != nil || publication == nil || h.work.DirectCalls != 2 || h.work.Begins != 1 || h.work.Pipelines != 0 || h.work.Commits != 0 || h.pointers[key] != "" || len(h.stages) != 1 {
		t.Fatalf("existing begin helper behavior changed: work=%+v err=%v", h.work, err)
	}
}

// The final small alias tail must complete before the production completion
// gate permits readiness/census publication. A retry is additional attempts,
// not additional logical aliases, and must retain identical bytes and TTLs.
func TestNativeFirstPageTailFlushCompletionControl(t *testing.T) {
	for _, failure := range []string{"healthy", "alias", "canceled", "retry_alias"} {
		t.Run(failure, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			r, h := newNativeFirstTransport(t, failure)
			keys := nativeFirstKeys(false, RankModeQuality, nativeFirstID(20_000))
			facets := nativeFirstFacets(h, 1, 0)
			emitted, finished := 0, false
			err := writeClientScoreRedisStream(ctx, r, nativeFirstTTL, func(emit func(clientScoreRedisSet) error) error {
				native := newClientScoreNativeFanout(ctx, r, keys.counts, nativeFirstTTL, h.census, func(set clientScoreRedisSet) error {
					if strings.HasSuffix(set.key, ":native_v1") {
						if h.work.Commits != 1 {
							return fmt.Errorf("alias emitted before native baseline commit")
						}
						emitted++
					}
					return emit(set)
				})
				if err := native.publish(server.Id{}, facets); err != nil {
					return err
				}
				for i := range 3 {
					if err := native.alias(nativeFirstID(10_000 + i)); err != nil {
						return err
					}
				}
				if emitted != 3 || h.work.Aliases != 0 {
					return fmt.Errorf("native aliases bypassed bounded final tail")
				}
				if failure == "canceled" {
					cancel()
				}
				return nil
			})
			workerErrors := make(chan error, 1)
			if err != nil {
				workerErrors <- err
			}
			close(workerErrors)
			completion := finishClientScoreExport(ctx, workerErrors)
			if completion == nil {
				finished = true // Same success condition as readiness/census publication.
			}
			wantSuccess := failure == "healthy" || failure == "retry_alias"
			if finished != wantSuccess || emitted != 3 {
				t.Fatalf("tail completion boundary changed: finished=%t emitted=%d err=%v", finished, emitted, completion)
			}
			if wantSuccess {
				wantAttempts := emitted
				if failure == "retry_alias" {
					wantAttempts *= 2
				}
				if h.work.Aliases != wantAttempts || h.work.DirectAliases != 0 {
					t.Fatalf("logical aliases/SET attempts differ: logical=%d attempts=%d", emitted, h.work.Aliases)
				}
			} else if failure == "alias" && !errors.Is(completion, nativeFirstFailure) {
				t.Fatalf("tail write error hidden: %v", completion)
			} else if failure == "canceled" && !errors.Is(completion, context.Canceled) {
				t.Fatalf("tail cancellation hidden: %v", completion)
			}
		})
	}
}

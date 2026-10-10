package model

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/redis/go-redis/v9"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
)

// Installs explicit activation only in tests that exercise the native reader.
// Publication tests and ordinary request tests retain the production default.
func nativeTestEnableReader(t testing.TB) {
	t.Helper()
	pop := server.Config.PushSimpleResource(providerConfigResourceName, []byte("subscriber_quality_policy_version: 2\negress_index:\n  native_reader_enabled: true\n"))
	requestEgressIndexSettingsSnapshot.Store(nil)
	t.Cleanup(func() {
		pop()
		requestEgressIndexSettingsSnapshot.Store(nil)
	})
}

// Counts only synthetic manifest keys. The optional config change happens
// after a real primary read, forcing the alternate load to prove that it uses
// the same request snapshot. No production hook or timing sleep is needed.
type nativeActivationReadHook struct {
	active      atomic.Bool
	nativeKeys  map[string]int
	primaryKeys map[string]bool
	reads       [2]atomic.Int64
	changed     atomic.Bool
	change      func()
}

func (h *nativeActivationReadHook) DialHook(next redis.DialHook) redis.DialHook { return next }
func (h *nativeActivationReadHook) observe(commands []redis.Cmder) {
	if !h.active.Load() {
		return
	}
	primary := false
	for _, command := range commands {
		if command.Name() != "get" || len(command.Args()) < 2 {
			continue
		}
		key, ok := command.Args()[1].(string)
		if !ok {
			continue
		}
		if mode, ok := h.nativeKeys[key]; ok {
			h.reads[mode].Add(1)
		}
		primary = primary || h.primaryKeys[key]
	}
	if primary && h.change != nil && h.changed.CompareAndSwap(false, true) {
		h.change()
	}
}
func (h *nativeActivationReadHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, command redis.Cmder) error {
		err := next(ctx, command)
		h.observe([]redis.Cmder{command})
		return err
	}
}
func (h *nativeActivationReadHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return func(ctx context.Context, commands []redis.Cmder) error {
		err := next(ctx, commands)
		h.observe(commands)
		return err
	}
}

// A complete native publication cannot activate a reader by itself. The
// response stays available through the union keys while disabled; explicit
// activation reads both native tiers, including an intentionally empty one.
func TestNativeReaderActivationBoundary(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		for _, requested := range []RankMode{RankModeQuality, RankModeSpeed} {
			for _, tc := range []struct {
				name, config, flip string
				enabled, forced    bool
			}{
				{name: "missing"},
				{name: "disabled", config: "egress_index:\n  native_reader_enabled: false\n"},
				{name: "malformed", config: "egress_index:\n  native_reader_enabled: [true]\n"},
				{name: "enabled", config: "egress_index:\n  native_reader_enabled: true\n", enabled: true},
				{name: "enable_during_primary", flip: "egress_index:\n  native_reader_enabled: true\n"},
				{name: "disable_during_primary", config: "egress_index:\n  native_reader_enabled: true\n", flip: "egress_index:\n  native_reader_enabled: false\n", enabled: true},
				{name: "forced_minimum", config: "egress_index:\n  native_reader_enabled: true\n", forced: true},
			} {
				func() {
					pop := server.Config.PushSimpleResource(providerConfigResourceName, []byte(tc.config))
					defer pop()
					requestEgressIndexSettingsSnapshot.Store(nil)
					defer requestEgressIndexSettingsSnapshot.Store(nil)
					location := server.NewId()
					scores := map[ipFamilyFacet][]*ClientScore{}
					allowed := map[server.Id]bool{}
					for range 40 {
						score := onlineBackfillScore(true, 1)
						scores[ipFamilyFacetV4Only] = append(scores[ipFamilyFacetV4Only], score)
						allowed[score.ClientId] = true
					}
					hook := &nativeActivationReadHook{nativeKeys: map[string]int{}, primaryKeys: map[string]bool{}}
					for modeIndex, mode := range []RankMode{RankModeQuality, RankModeSpeed} {
						nativeTestPublishLocation(t, location, mode, scores)
						callers := []server.Id{{}}
						for _, caller := range countryCodeLocationIds() {
							callers = append(callers, caller)
						}
						for _, caller := range callers {
							key := clientScoreNativeKey(clientScoreLocationCountsKey(false, mode, location, caller))
							hook.nativeKeys[key] = modeIndex
							if mode == requested {
								hook.primaryKeys[key] = true
								hook.primaryKeys[clientScoreLocationAliasKey(false, mode, location, caller)] = true
								for _, facet := range ipFamilyFacets {
									hook.primaryKeys[clientScoreLocationFacetCountsKey(false, mode, location, caller, facet)] = true
								}
							}
						}
					}
					if tc.forced {
						writeOnlineBackfillSample(t.Context(), t, location, requested, true, scores[ipFamilyFacetV4Only])
					}
					var popFlip func()
					defer func() {
						if popFlip != nil {
							popFlip()
						}
					}()
					if tc.flip != "" {
						hook.change = func() {
							popFlip = server.Config.PushSimpleResource(providerConfigResourceName, []byte(tc.flip))
							requestEgressIndexSettingsSnapshot.Store(nil)
						}
					}
					server.Redis(t.Context(), func(r server.RedisClient) { r.AddHook(hook) })
					hook.active.Store(true)
					defer hook.active.Store(false)
					clientSession := testingCreateProviderSearchSession(t.Context(), session.NewByJwt(server.NewId(), server.NewId(), "native-activation-test", false, false))
					result, err := FindProviders2(&FindProviders2Args{Specs: []*ProviderSpec{{LocationId: &location}}, RankMode: requested, ForceMinimum: tc.forced}, clientSession)
					if err != nil || result == nil || len(result.Providers) != 20 {
						t.Fatalf("case=%s mode=%s activation changed union availability: err=%v", tc.name, requested, err)
					}
					for _, provider := range result.Providers {
						if !allowed[provider.ClientId] {
							t.Fatal("activation returned a provider outside the published union")
						}
					}
					for modeIndex, mode := range []RankMode{RankModeQuality, RankModeSpeed} {
						got := hook.reads[modeIndex].Load()
						wantNative := tc.enabled && !tc.forced
						if (got > 0) != wantNative {
							t.Errorf("case=%s requested=%s loaded=%s native_reads=%d want_native=%t", tc.name, requested, mode, got, wantNative)
						}
					}
					if tc.flip != "" && !hook.changed.Load() {
						t.Fatal("config transition control did not reach the primary-read boundary")
					}
					assertEgressTestNoRepeats(t, result.Providers)
				}()
			}
		}
	})
}

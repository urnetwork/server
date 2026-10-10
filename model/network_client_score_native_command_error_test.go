package model

import (
	"context"
	"errors"
	"io"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
)

// The test owns these exact keys in the disposable Redis database. Successful
// siblings are read by the real driver; selected commands either encounter a
// real WRONGTYPE after a controlled slot change or receive a transport error.
// This models partial shard replies without interrupting any Redis service.
type nativeSiblingCommandFailureHook struct {
	active   atomic.Bool
	matched  atomic.Int64
	keys     map[string]bool
	pageOnly bool
	mutate   func(context.Context)
	failure  error
	cancel   context.CancelFunc
}

func (self *nativeSiblingCommandFailureHook) DialHook(next redis.DialHook) redis.DialHook {
	return next
}

func (self *nativeSiblingCommandFailureHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return next
}

func (self *nativeSiblingCommandFailureHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return func(ctx context.Context, commands []redis.Cmder) error {
		if !self.active.Load() {
			return next(ctx, commands)
		}
		good, failed := []redis.Cmder{}, []redis.Cmder{}
		for _, command := range commands {
			args := command.Args()
			if len(args) < 2 {
				good = append(good, command)
				continue
			}
			key, _ := args[1].(string)
			matches := self.keys[key] && command.Name() == "get" && !self.pageOnly
			if self.pageOnly {
				matches = self.keys[key] && command.Name() == "hmget" && len(args) == 4 && args[3] != "m"
			}
			if matches {
				failed = append(failed, command)
			} else {
				good = append(good, command)
			}
		}
		if len(failed) == 0 {
			return next(ctx, commands)
		}
		self.matched.Add(int64(len(failed)))
		if self.mutate != nil {
			self.mutate(ctx)
			return next(ctx, commands)
		}
		var err error
		if len(good) > 0 {
			err = next(ctx, good)
		}
		for _, command := range failed {
			command.SetErr(self.failure)
		}
		if self.cancel != nil {
			self.cancel()
		}
		return errors.Join(err, self.failure)
	}
}

func nativeSiblingTransportFailure() error {
	return &net.OpError{Op: "read", Net: "tcp", Err: io.ErrUnexpectedEOF}
}

// Remove the compatibility counts, so a retained answer can only originate
// from the independently verified native target rather than a legacy copy.
func nativeSiblingRemoveLegacy(t testing.TB, location server.Id) {
	t.Helper()
	server.Redis(t.Context(), func(r server.RedisClient) {
		keys := []string{clientScoreLocationCountsKey(false, RankModeQuality, location, server.Id{})}
		for _, facet := range ipFamilyFacets {
			keys = append(keys, clientScoreLocationFacetCountsKey(false, RankModeQuality, location, server.Id{}, facet))
		}
		server.Raise(r.Del(t.Context(), keys...).Err())
	})
}

func nativeSiblingSlot(t testing.TB, location server.Id) (key string, slot string) {
	t.Helper()
	key = clientScoreNativeKey(clientScoreLocationCountsKey(false, RankModeQuality, location, server.Id{}))
	server.Redis(t.Context(), func(r server.RedisClient) {
		pointer, err := r.Get(t.Context(), key).Result()
		if err != nil {
			t.Fatal(err)
		}
		_, slot, err = readClientScoreNativeManifest(t.Context(), r, key, pointer)
		if err != nil {
			t.Fatal(err)
		}
	})
	return
}

func TestNativeCommandFailureKeepsVerifiedSiblingsAndRequestGates(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		nativeTestEnableReader(t)
		ctx := t.Context()
		for _, failure := range []string{"pointer_wrongtype", "pointer_transport", "page_wrongtype", "page_transport"} {
			func() {
				goodLocation, failedLocation := server.NewId(), server.NewId()
				good, bad := map[ipFamilyFacet][]*ClientScore{}, map[ipFamilyFacet][]*ClientScore{}
				allowed := map[server.Id]bool{}
				for range 5 {
					score := nativeTestScore(RankModeQuality, ipFamilyFacetV4Only)
					good[ipFamilyFacetV4Only] = append(good[ipFamilyFacetV4Only], score)
					allowed[score.ClientId] = true
					bad[ipFamilyFacetV4Only] = append(bad[ipFamilyFacetV4Only], nativeTestScore(RankModeQuality, ipFamilyFacetV4Only))
				}
				hard, unknown := nativeTestScore(RankModeQuality, ipFamilyFacetV4Only), nativeTestScore(RankModeQuality, ipFamilyFacetV4Only)
				private, explicit := nativeTestScore(RankModeQuality, ipFamilyFacetV4Only), nativeTestScore(RankModeQuality, ipFamilyFacetV4Only)
				private.NetworkOnly = true
				good[ipFamilyFacetV4Only] = append(good[ipFamilyFacetV4Only], hard, unknown, private, explicit)
				nativeTestPublishLocation(t, goodLocation, RankModeQuality, good)
				nativeTestPublishLocation(t, failedLocation, RankModeQuality, bad)
				nativeSiblingRemoveLegacy(t, goodLocation)
				nativeSiblingRemoveLegacy(t, failedLocation)
				failedKey, failedSlot := nativeSiblingSlot(t, failedLocation)
				server.Tx(ctx, func(tx server.PgTx) {
					server.RaisePgResult(tx.Exec(ctx, `DELETE FROM network_client_location WHERE connection_id=$1`, unknown.ClientId))
				})
				hook := &nativeSiblingCommandFailureHook{keys: map[string]bool{failedKey: true}, failure: nativeSiblingTransportFailure()}
				server.Redis(ctx, func(r server.RedisClient) {
					server.Raise(r.SAdd(ctx, providerHardExclusionsKey, hard.ClientId.String()).Err())
					switch failure {
					case "pointer_wrongtype":
						server.Raise(r.Del(ctx, failedKey).Err())
						server.Raise(r.RPush(ctx, failedKey, "synthetic-wrong-type").Err())
						server.Raise(r.Expire(ctx, failedKey, time.Minute).Err())
					case "page_wrongtype":
						hook.keys, hook.pageOnly = map[string]bool{failedSlot: true}, true
						hook.mutate = func(ctx context.Context) {
							server.Raise(r.Set(ctx, failedSlot, "synthetic-wrong-type", time.Minute).Err())
						}
					case "page_transport":
						hook.keys, hook.pageOnly = map[string]bool{failedSlot: true}, true
					}
					if failure != "pointer_wrongtype" {
						r.AddHook(hook)
						hook.active.Store(true)
					}
				})
				defer hook.active.Store(false)
				locations := map[server.Id]bool{goodLocation: true, failedLocation: true}
				scores, cursor, available, err := loadNativeClientScoresWithCursor(ctx, RankModeQuality, locations, nil, server.Id{}, 1000, []ipFamilyFacet{ipFamilyFacetV4Only}, nil)
				if err == nil || !available || cursor == nil || !cursor.sourceIncomplete || len(scores) != len(good[ipFamilyFacetV4Only]) {
					t.Fatalf("%s discarded verified sibling rows or hid native failure: rows=%d available=%t incomplete=%t err=%v", failure, len(scores), available, cursor != nil && cursor.sourceIncomplete, err)
				}
				for _, score := range good[ipFamilyFacetV4Only] {
					if scores[score.ClientId] == nil {
						t.Fatalf("%s retained a failed target instead of the verified target", failure)
					}
				}
				labels := map[string]string{"rank_mode": RankModeQuality, "source": "primary", "outcome": "unavailable"}
				before := selectionMetricCount(t, "urnetwork_findproviders2_native_source_outcomes_total", labels)
				args := &FindProviders2Args{Specs: []*ProviderSpec{{LocationId: &goodLocation}, {LocationId: &failedLocation}}, ForceCount: true, Count: 5, ExcludeClientIds: []server.Id{explicit.ClientId}}
				clientSession := testingCreateProviderSearchSession(ctx, session.NewByJwt(server.NewId(), server.NewId(), "native-sibling-error-test", false, false))
				result, err := FindProviders2(args, clientSession)
				if err != nil || result == nil || len(result.Providers) != 5 {
					t.Fatalf("%s failed to serve verified same-target native supply: err=%v", failure, err)
				}
				for _, provider := range result.Providers {
					if !allowed[provider.ClientId] || provider.Tier != 0 {
						t.Fatalf("%s changed native priority or bypassed a current gate", failure)
					}
				}
				assertEgressTestNoRepeats(t, result.Providers)
				if after := selectionMetricCount(t, "urnetwork_findproviders2_native_source_outcomes_total", labels); after != before+1 {
					t.Fatalf("%s partial command failure was falsely reported as complete", failure)
				}
				if failure != "pointer_wrongtype" && hook.matched.Load() == 0 {
					t.Fatal("failure control did not reach the selected real Redis pipeline")
				}
			}()
		}
	})
}

func TestNativeCommandFailureAllFailedRemainsUnavailable(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		nativeTestEnableReader(t)
		location := server.NewId()
		nativeTestPublishLocation(t, location, RankModeQuality, map[ipFamilyFacet][]*ClientScore{ipFamilyFacetV4Only: {nativeTestScore(RankModeQuality, ipFamilyFacetV4Only)}})
		key, slot := nativeSiblingSlot(t, location)
		for _, stage := range []string{"pointer", "page"} {
			hook := &nativeSiblingCommandFailureHook{keys: map[string]bool{key: true}, failure: nativeSiblingTransportFailure()}
			if stage == "page" {
				hook.keys, hook.pageOnly = map[string]bool{slot: true}, true
			}
			server.Redis(t.Context(), func(r server.RedisClient) { r.AddHook(hook) })
			hook.active.Store(true)
			scores, cursor, available, err := loadNativeClientScoresWithCursor(t.Context(), RankModeQuality, map[server.Id]bool{location: true}, nil, server.Id{}, 1000, []ipFamilyFacet{ipFamilyFacetV4Only}, nil)
			hook.active.Store(false)
			if !errors.Is(err, io.ErrUnexpectedEOF) || len(scores) != 0 || !available || cursor == nil || !cursor.sourceIncomplete || hook.matched.Load() != 1 {
				t.Fatalf("%s whole-read failure became a legacy miss, success or complete empty native source", stage)
			}
		}
	})
}

func TestNativeCommandFailureCanceledReadDiscardsSuccessfulSiblings(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		nativeTestEnableReader(t)
		good, failed := server.NewId(), server.NewId()
		for _, location := range []server.Id{good, failed} {
			nativeTestPublishLocation(t, location, RankModeQuality, map[ipFamilyFacet][]*ClientScore{ipFamilyFacetV4Only: {nativeTestScore(RankModeQuality, ipFamilyFacetV4Only)}})
		}
		_, slot := nativeSiblingSlot(t, failed)
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		hook := &nativeSiblingCommandFailureHook{keys: map[string]bool{slot: true}, pageOnly: true, failure: context.Canceled, cancel: cancel}
		server.Redis(t.Context(), func(r server.RedisClient) { r.AddHook(hook) })
		hook.active.Store(true)
		defer hook.active.Store(false)
		clientSession := testingCreateProviderSearchSession(ctx, session.NewByJwt(server.NewId(), server.NewId(), "native-sibling-cancel-test", false, false))
		result, err := FindProviders2(&FindProviders2Args{Specs: []*ProviderSpec{{LocationId: &good}, {LocationId: &failed}}}, clientSession)
		if !errors.Is(err, context.Canceled) || result != nil || hook.matched.Load() != 1 {
			t.Fatal("canceled native read returned successful sibling rows or entered fallback")
		}
	})
}

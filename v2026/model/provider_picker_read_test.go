// Actual Redis-to-picker controls: a missing key is not a failed GET.
package model

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/session"
)

// The attested TestEnv owns these keys; no outage or shared client hook is used.
func providerPickerReadFixture(t testing.TB, mode string) (context.Context, server.Id, server.Id, error) {
	t.Helper()
	ctx := t.Context()
	locationId, callerId := server.NewId(), server.Id{}
	if strings.HasPrefix(mode, "alias") {
		callerId = server.NewId()
	}
	filterKey := clientScoreLocationFilterKey(false, RankModeQuality, locationId, callerId)
	aliasKey := clientScoreLocationAliasKey(false, RankModeQuality, locationId, callerId)
	baselineKey := clientScoreLocationFilterKey(false, RankModeQuality, locationId, server.Id{})
	keys := []string{initialClientLocationsKey(), filterKey, aliasKey, baselineKey}
	// TestEnv releases this disposable Redis database before outer t.Cleanup.
	// Keep all key access inside its callback; no cleanup in the restored DB.
	var expected error
	server.Redis(ctx, func(r server.RedisClient) {
		for _, key := range keys {
			server.Raise(r.Del(ctx, key).Err())
		}
		initial := &InitialClientLocations{Locations: []*ClientLocation{{LocationId: locationId, LocationType: LocationTypeCountry, Name: "Synthetic country"}}}
		if mode != "missing" {
			server.Raise(r.Set(ctx, initialClientLocationsKey(), gobEncodeForTest(t, initial), time.Minute).Err())
		}
		if mode == "valid" || mode == "alias-baseline" {
			server.Raise(r.Set(ctx, baselineKey, gobEncodeForTest(t, &ClientFilter{Count: 1, NetReliabilityWeight: MinStableNetReliabilityWeight}), time.Minute).Err())
		}
		if mode == "alias-baseline" {
			server.Raise(r.Set(ctx, aliasKey, clientScoreAliasBaselineValue, time.Minute).Err())
		}
		failedKey := ""
		switch mode {
		case "initial-failure":
			failedKey = initialClientLocationsKey()
		case "filter-failure":
			failedKey = filterKey
		case "alias-masked-failure":
			failedKey = baselineKey
		case "missing", "valid", "alias-baseline", "filter-missing":
		default:
			t.Fatal("unknown fixture mode")
		}
		if failedKey != "" {
			server.Raise(r.Del(ctx, failedKey).Err())
			server.Raise(r.RPush(ctx, failedKey, "synthetic-wrong-type").Err())
			server.Raise(r.Expire(ctx, failedKey, time.Minute).Err())
			expected = r.Get(ctx, failedKey).Err()
			if expected == nil || !strings.HasPrefix(expected.Error(), "WRONGTYPE ") {
				t.Fatal("fixture did not establish GET error")
			}
			if mode == "alias-masked-failure" {
				pipe := r.Pipeline()
				missing := pipe.Get(ctx, filterKey)
				failed := pipe.Get(ctx, baselineKey)
				_, err := pipe.Exec(ctx)
				if !errors.Is(err, redis.Nil) || !errors.Is(missing.Err(), redis.Nil) || !errors.Is(failed.Err(), expected) {
					t.Fatal("fixture did not mask later error behind redis.Nil")
				}
			}
		}
	})
	return ctx, locationId, callerId, expected
}

func TestProviderPickerReadInitialFailure(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, _, _, expected := providerPickerReadFixture(t, "initial-failure")
		result, err := loadInitialClientLocations(ctx)
		if !errors.Is(err, expected) || result != nil {
			t.Fatal("initial GET error became successful empty picker cache")
		}
	})
}

func TestProviderPickerReadFilterFailure(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, locationId, callerId, expected := providerPickerReadFixture(t, "filter-failure")
		result, err := loadLocationStables(ctx, []server.Id{locationId}, false, RankModeQuality, callerId)
		if !errors.Is(err, expected) || len(result) != 0 {
			t.Fatal("failed filter GET became successful empty visibility")
		}
	})
}

func TestProviderPickerReadMaskedFilterFailure(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, locationId, callerId, expected := providerPickerReadFixture(t, "alias-masked-failure")
		_, err := loadLocationStables(ctx, []server.Id{locationId}, false, RankModeQuality, callerId)
		if !errors.Is(err, expected) {
			t.Fatal("earlier missing caller key hid failed baseline GET")
		}
	})
}

func TestProviderPickerReadGetInitialErrorPropagates(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, _, _, expected := providerPickerReadFixture(t, "initial-failure")
		result, err := GetProviderLocations(&session.ClientSession{Ctx: ctx})
		if !errors.Is(err, expected) || result != nil {
			t.Fatal("GET picker returned success for failed initial read")
		}
	})
}

func TestProviderPickerReadGetFilterErrorPropagates(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, _, _, expected := providerPickerReadFixture(t, "filter-failure")
		result, err := GetProviderLocations(&session.ClientSession{Ctx: ctx})
		if !errors.Is(err, expected) || result != nil {
			t.Fatal("GET picker returned success after filter read failure")
		}
	})
}

func TestProviderPickerReadMissingInitialControl(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, _, _, _ := providerPickerReadFixture(t, "missing")
		result, err := GetProviderLocations(&session.ClientSession{Ctx: ctx})
		if err != nil || result == nil || len(result.Locations) != 0 {
			t.Fatal("genuine initial cache miss changed")
		}
	})
}

func TestProviderPickerReadMissingFilterControl(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, _, _, _ := providerPickerReadFixture(t, "filter-missing")
		result, err := GetProviderLocations(&session.ClientSession{Ctx: ctx})
		if err != nil || result == nil || len(result.Locations) != 0 {
			t.Fatal("genuine absent filter changed")
		}
	})
}

func TestProviderPickerReadNonemptyControl(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, _, _, _ := providerPickerReadFixture(t, "valid")
		result, err := GetProviderLocations(&session.ClientSession{Ctx: ctx})
		if err != nil || result == nil || result.CountryCount != 1 {
			t.Fatal("valid picker country disappeared")
		}
	})
}

func TestProviderPickerReadAliasBaselineControl(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, locationId, callerId, _ := providerPickerReadFixture(t, "alias-baseline")
		result, err := loadLocationStables(ctx, []server.Id{locationId}, false, RankModeQuality, callerId)
		if err != nil || len(result) != 1 || !result[locationId] {
			t.Fatal("valid shared baseline alias changed")
		}
	})
}

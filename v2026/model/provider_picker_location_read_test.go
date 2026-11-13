// Search metadata preserves the same missing-versus-failed Redis boundary as
// initial metadata, candidate scores and location filters.
package model

import (
	"errors"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/urnetwork/server/v2026"
)

func TestProviderPickerLocationMetadataFailure(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		missingId, failedId := server.NewId(), server.NewId()
		var expected error
		server.Redis(ctx, func(client server.RedisClient) {
			server.Raise(client.RPush(ctx, clientLocationKey(failedId), "synthetic-wrong-type").Err())
			server.Raise(client.Expire(ctx, clientLocationKey(failedId), time.Minute).Err())
			pipeline := client.Pipeline()
			missing := pipeline.Get(ctx, clientLocationKey(missingId))
			failed := pipeline.Get(ctx, clientLocationKey(failedId))
			_, err := pipeline.Exec(ctx)
			expected = failed.Err()
			if !errors.Is(err, redis.Nil) || !errors.Is(missing.Err(), redis.Nil) || expected == nil || errors.Is(expected, redis.Nil) {
				t.Fatal("fixture did not mask a failed command behind a missing key")
			}
		})
		_, err := loadClientLocations(ctx, map[server.Id]bool{missingId: true, failedId: true})
		if !errors.Is(err, expected) {
			t.Fatal("failed search metadata became successful empty lookup")
		}
	})
}

func TestProviderPickerLocationExpandedMetadataFailure(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		locationId, parentId := server.NewId(), server.NewId()
		var expected error
		server.Redis(ctx, func(client server.RedisClient) {
			location := &ClientLocation{LocationId: locationId, CountryLocationId: &parentId, Name: "Synthetic city"}
			server.Raise(client.Set(ctx, clientLocationKey(locationId), gobEncodeForTest(t, location), time.Minute).Err())
			server.Raise(client.RPush(ctx, clientLocationKey(parentId), "synthetic-wrong-type").Err())
			server.Raise(client.Expire(ctx, clientLocationKey(parentId), time.Minute).Err())
			expected = client.Get(ctx, clientLocationKey(parentId)).Err()
			if expected == nil || errors.Is(expected, redis.Nil) {
				t.Fatal("expanded metadata failure was not established")
			}
		})
		_, err := loadClientLocations(ctx, map[server.Id]bool{locationId: true})
		if !errors.Is(err, expected) {
			t.Fatal("failed expanded metadata became successful partial search")
		}
	})
}

func TestProviderPickerLocationMissingAndValidMetadata(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		locationId, missingId := server.NewId(), server.NewId()
		server.Redis(ctx, func(client server.RedisClient) {
			location := &ClientLocation{LocationId: locationId, CountryLocationId: &missingId, Name: "Synthetic retained city"}
			server.Raise(client.Set(ctx, clientLocationKey(locationId), gobEncodeForTest(t, location), time.Minute).Err())
		})
		locations, err := loadClientLocations(ctx, map[server.Id]bool{locationId: true, missingId: true})
		if err != nil || len(locations) != 1 || locations[locationId] == nil || locations[locationId].Name != "Synthetic retained city" {
			t.Fatal("genuine missing metadata changed the valid partial result")
		}
		empty, err := loadClientLocations(ctx, map[server.Id]bool{server.NewId(): true})
		if err != nil || len(empty) != 0 {
			t.Fatal("genuine empty metadata became a read failure")
		}
	})
}

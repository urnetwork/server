package model_test

import (
	"errors"
	"fmt"
	"testing"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/protocol/bridge"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/sidecarhost"
)

// The gates' reader, sidecar.AclGateDirectSource, takes from {acl_}e exactly what api's script writes: the
// entries after a pin when the hash is current and whole, and proj/ otherwise, which with no cluster here
// is an error, so a miss shows (FS-62).
func TestConfigLogCacheServesTheGateSource(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		networkId := server.NewId()
		write := model.NewConfigLogRedisEntryCache(nil)
		for n := uint64(1); n <= 4; n += 1 {
			if err := write(ctx, networkId, n, 1, []byte(fmt.Sprintf("entry %d", n))); err != nil {
				t.Fatal(err)
			}
		}
		source := sidecar.NewAclGateDirectSource(sidecarhost.NewRedis(nil), nil, nil)
		entries, err := source.Entries(ctx, sidecar.Id(networkId), &bridge.AuthPin{Version: 2})
		if err != nil {
			t.Fatal(err)
		}
		connect.AssertEqual(t, len(entries), 2)
		connect.AssertEqual(t, string(entries[0]), "entry 3")
		connect.AssertEqual(t, string(entries[1]), "entry 4")

		// stale: an {acl_}v raised past what the hash records
		server.Redis(ctx, func(r server.RedisClient) {
			server.Raise(r.Incr(ctx, sidecar.AclVersionKey(sidecar.Id(networkId))).Err())
		})
		if _, err := source.Entries(ctx, sidecar.Id(networkId), &bridge.AuthPin{Version: 2}); !errors.Is(err, sidecar.ErrBlobClusterUnknown) {
			t.Fatalf("a stale hash served: %v", err)
		}
		// current again, and lost: a position below the base the hash holds is proj/'s
		if err := write(ctx, networkId, 5, 5, []byte("entry 5")); err != nil {
			t.Fatal(err)
		}
		if _, err := source.Entries(ctx, sidecar.Id(networkId), &bridge.AuthPin{Version: 2}); !errors.Is(err, sidecar.ErrBlobClusterUnknown) {
			t.Fatalf("a position below the base served: %v", err)
		}
		entries, err = source.Entries(ctx, sidecar.Id(networkId), &bridge.AuthPin{Version: 4})
		if err != nil {
			t.Fatal(err)
		}
		connect.AssertEqual(t, len(entries), 1)
		connect.AssertEqual(t, string(entries[0]), "entry 5")
	})
}

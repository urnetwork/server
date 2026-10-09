package session

import (
	"context"
	"sort"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/urnetwork/server"
)

// A reproducible local release profile at both configured limits. Numbers are
// reported as measurements, not a claim about production Redis/SQL hardware.
func TestSessionScaleProfile(t *testing.T) {
	if testing.Short() {
		t.Skip("requires local Redis and Postgres")
	}
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		now := server.NowUtc()
		network := server.NewId()
		first := sessionFixtureCredential(network, now)
		keys := append([]string{}, sessionSharedKeys(network)...)
		for i := 0; i < NetworkSessionLiveLimit; i++ {
			credential := first
			if i > 0 {
				credential = sessionFixtureCredential(network, now)
			}
			if _, err := RegisterNetworkSession(ctx, credential, "password", nil, false, now); err != nil {
				t.Fatal(err)
			}
			keys = append(keys, SessionKey(network, "s:"+credential.SessionId.String()))
		}
		// The live set is full; populate the remaining retained slots with real
		// marker strings and the same retention score used by the revoke script.
		server.Raise(server.RedisAuth(ctx, func(ctx context.Context, r server.RedisClient) error {
			pipeline := r.Pipeline()
			for i := NetworkSessionLiveLimit; i < NetworkSessionRetainedLimit; i++ {
				sid := server.NewId()
				marker := SessionMarkerKey(network, sid)
				keys = append(keys, marker)
				pipeline.Set(ctx, marker, 1, 90*24*time.Hour)
				pipeline.ZAdd(ctx, SessionKey(network, "held"), redis.Z{Score: float64(now.Add(90 * 24 * time.Hour).UnixMilli()), Member: sid.String()})
			}
			_, err := pipeline.Exec(ctx)
			return err
		}))
		var memory int64
		server.Raise(server.RedisAuth(ctx, func(ctx context.Context, r server.RedisClient) error {
			var err error
			memory, err = r.Eval(ctx, `local total=0;for i=1,#KEYS do total=total+(redis.call('MEMORY','USAGE',KEYS[i]) or 0) end;return total`, keys).Int64()
			return err
		}))
		actor := NewLocalClientSession(ctx, "192.0.2.1:1", first)
		defer actor.Cancel()
		listStart := time.Now()
		list, err := GetNetworkSessions(actor)
		listDuration := time.Since(listStart)
		if err != nil || len(list.Sessions) != NetworkSessionLiveLimit {
			t.Fatal("capacity inventory incomplete", err)
		}
		revokeStart := time.Now()
		result, err := enforceSessionRevoke(ctx, network, server.NewId(), "others", nil, first.SessionId)
		revokeDuration := time.Since(revokeStart)
		if err != nil || result.RevokedCount != NetworkSessionLiveLimit-1 {
			t.Fatal("maximum bulk failed", result, err)
		}
		// A thousand connections sharing one sign-in still run fresh authority
		// checks; completed pre-revoke successes are never a positive cache.
		liveActor := recoverySessionActor(t)
		client, device := server.NewId(), server.NewId()
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO device(device_id,network_id,device_name,device_spec,create_time) VALUES($1,$2,'scale','test',$3)`, device, liveActor.ByJwt.NetworkId, now))
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO network_client(client_id,network_id,device_id,description,create_time,auth_time) VALUES($1,$2,$3,'scale',$4,$4)`, client, liveActor.ByJwt.NetworkId, device, now))
		})
		claims := liveActor.ByJwt.Client(device, client)
		durations := make([]time.Duration, 1000)
		start := time.Now()
		for i := range durations {
			begin := time.Now()
			if err := ValidateByJwtState(ctx, claims, true); err != nil {
				t.Fatal(err)
			}
			durations[i] = time.Since(begin)
		}
		total := time.Since(start)
		sort.Slice(durations, func(i, j int) bool { return durations[i] < durations[j] })
		t.Logf("session scale: live=%d retained=%d Redis measured bytes=%d list=%s bulk=%s authoritative SQL+Redis checks=1000 total=%s p50=%s p99=%s", NetworkSessionLiveLimit, NetworkSessionRetainedLimit, memory, listDuration, revokeDuration, total, durations[500], durations[990])
	})
}

package session

import (
	"context"
	"encoding/json"
	"errors"
	gojwt "github.com/golang-jwt/jwt/v5"
	"github.com/redis/go-redis/v9"
	"github.com/urnetwork/server"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestClientInfoUntrustedMetadata(t *testing.T) {
	valid := `{"v":1,"device_type":"android","app_version":"1.2.3","sdk_version":"4.5.6","future":true}`
	for _, test := range []struct {
		raw   string
		valid bool
	}{{valid, true}, {"", false}, {`{"v":1,"v":1,"device_type":"ios"}`, false}, {`{"v":1,"device_type":"ios","future":0,"future":1}`, false}, {`{"v":2,"device_type":"ios"}`, false}, {`{"v":1,"device_type":"unique-device"}`, false}, {`{"v":1,"device_type":"ios","app_version":"` + strings.Repeat("x", 65) + `"}`, false}, {valid + " {}", false}, {strings.Repeat(" ", 513), false}} {
		got := ParseClientInfo(test.raw, "legacy")
		if test.valid {
			if got.DeviceType != "android" || got.AppVersion != "1.2.3" {
				t.Fatal(got)
			}
		} else if got.DeviceType != "unknown" || got.AppVersion != "legacy" {
			t.Fatalf("invalid advisory input accepted: %#v", got)
		}
	}
	header := http.Header{}
	header.Add(ClientInfoHeader, valid)
	header.Add(ClientInfoHeader, valid)
	if got := ClientInfoFromHeader(header); got.DeviceType != "unknown" {
		t.Fatal("duplicate headers accepted")
	}
}
func TestSessionTerminalHorizonAndLegacyId(t *testing.T) {
	now := time.Date(2026, 10, 9, 0, 0, 0, 123456789, time.UTC)
	claims := NewByJwt(server.NewId(), server.NewId(), "test", false, false)
	sid := server.NewId()
	claims.SessionId = &sid
	claims.ExpiresAt = gojwt.NewNumericDate(now)
	end := claims.AcceptUntil()
	if validateSessionHorizon(claims, end.Add(-time.Nanosecond)) != nil || validateSessionHorizon(claims, end) == nil {
		t.Fatal("terminal boundary is not exclusive")
	}
	claims.ExpiresAt = nil
	if validateSessionHorizon(claims, now) == nil {
		t.Fatal("tagged token without expiry accepted")
	}
	claims.CreateTime = now
	a := LegacySessionId(claims, server.NewId())
	copy := *claims
	copy.CreateTime = now.In(time.FixedZone("test", 3600))
	if a != LegacySessionId(&copy, server.NewId()) {
		t.Fatal("timezone changed legacy group")
	}
	copy.CreateTime = now.Add(time.Nanosecond)
	if a == LegacySessionId(&copy, server.NewId()) {
		t.Fatal("nanosecond lineage was lost")
	}
	if DeadlineMillis(now) != now.UnixMilli()+1 {
		t.Fatal("retention rounded down")
	}
}
func sessionFixtureCredential(networkId server.Id, now time.Time) *ByJwt {
	claims := NewByJwt(networkId, server.NewId(), "fixture", false, false)
	sid := server.NewId()
	claims.SessionId = &sid
	claims.ExpiresAt = gojwt.NewNumericDate(now.Add(30 * 24 * time.Hour))
	return claims
}
func TestSessionRedisCapacityHorizonAndAtomicRevoke(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		now := server.NowUtc()
		networkId := server.NewId()
		first := sessionFixtureCredential(networkId, now)
		if _, err := RegisterNetworkSession(ctx, first, "password", nil, false, now); err != nil {
			t.Fatal(err)
		}
		for i := 1; i < NetworkSessionLiveLimit; i++ {
			if _, err := RegisterNetworkSession(ctx, sessionFixtureCredential(networkId, now), "password", nil, false, now); err != nil {
				t.Fatal(i, err)
			}
		}
		if _, err := RegisterNetworkSession(ctx, sessionFixtureCredential(networkId, now), "password", nil, false, now); err == nil {
			t.Fatal("1001st accepted session evicted inventory")
		}
		first.ExpiresAt = gojwt.NewNumericDate(now.Add(31 * 24 * time.Hour))
		long, err := RegisterNetworkSession(ctx, first, "password", nil, false, now)
		if err != nil {
			t.Fatal(err)
		}
		first.ExpiresAt = gojwt.NewNumericDate(now.Add(24 * time.Hour))
		short, err := RegisterNetworkSession(ctx, first, "password", nil, false, now)
		if err != nil || short.AcceptUntil != long.AcceptUntil || short.RetainUntil != long.RetainUntil {
			t.Fatal("retry shortened authority", err)
		}
		inv, err := readSessionInventory(ctx, networkId, now)
		if err != nil || len(inv.Members) != NetworkSessionLiveLimit*2 {
			t.Fatal("accepted inventory truncated", err)
		}
		op := server.NewId()
		result, err := enforceSessionRevoke(ctx, networkId, op, "others", nil, first.SessionId)
		if err != nil || result.RevokedCount != NetworkSessionLiveLimit-1 {
			t.Fatal("bulk cutoff", result, err)
		}
		inv, err = readSessionInventory(ctx, networkId, now)
		if err != nil || len(inv.Members) != 2 || inv.Members[0] != first.SessionId.String() {
			t.Fatal("bulk retained wrong inventory", err)
		}
		replay, err := enforceSessionRevoke(ctx, networkId, op, "others", nil, first.SessionId)
		if err != nil || replay.RevokedCount != result.RevokedCount || replay.EventId != result.EventId {
			t.Fatal("lost response changed replay", err)
		}
		for _, sid := range result.TargetSessionIds {
			if !errors.Is(CheckSession(ctx, networkId, sid), ErrSessionRevoked) {
				t.Fatal("revoke marker missing")
			}
		}
		fresh := sessionFixtureCredential(networkId, now)
		if _, err := RegisterNetworkSession(ctx, fresh, "password", nil, false, now); err != nil {
			t.Fatal("post-cutoff independent login refused", err)
		}
		if _, err := RegisterNetworkSession(ctx, sessionFixtureCredential(networkId, now), "auth_code", &result.TargetSessionIds[0], true, now); err == nil {
			t.Fatal("targeted creator code redeemed after cutoff")
		}
	})
}
func TestSessionLastUseTupleThrottleAndRevocation(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		now := server.NowUtc().Truncate(time.Second)
		networkId := server.NewId()
		claims := sessionFixtureCredential(networkId, now)
		registration, err := RegisterNetworkSession(ctx, claims, "password", nil, false, now)
		if err != nil {
			t.Fatal(err)
		}
		observation := SessionLastUsed{UnixTime: now.Unix(), City: "Chicago", Region: "Illinois", Country: "United States", CountryCode: "us", DeviceType: "android", AppVersion: "1.2.3"}
		write := func(at int64, city string) {
			sample := observation
			sample.UnixTime = at
			sample.City = city
			if err := writeSessionObservation(ctx, networkId, *claims.SessionId, sample, time.Unix(at, 0)); err != nil {
				t.Fatal(err)
			}
		}
		write(now.Unix(), "Chicago")
		write(now.Unix()+59, "suppressed")
		write(now.Unix()+60, "Paris")
		write(now.Unix()+1, "delayed")
		var encoded string
		var ttl time.Duration
		err = server.RedisAuth(ctx, func(ctx context.Context, r server.RedisClient) error {
			var err error
			encoded, err = r.Get(ctx, SessionKey(networkId, "u:"+claims.SessionId.String())).Result()
			if err != nil {
				return err
			}
			ttl, err = r.PExpireTime(ctx, SessionKey(networkId, "u:"+claims.SessionId.String())).Result()
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
		var got SessionLastUsed
		if json.Unmarshal([]byte(encoded), &got) != nil || got.City != "Paris" || got.UnixTime != now.Unix()+60 || got.DeviceType != "android" || got.Region != "Illinois" || got.CountryCode != "us" || got.AppVersion != "1.2.3" || ttl.Milliseconds() != registration.AcceptUntil {
			t.Fatal("typed tuple or TTL changed", got, ttl)
		}
		claims.ExpiresAt = gojwt.NewNumericDate(now.Add(40 * 24 * time.Hour))
		if _, err = RegisterNetworkSession(ctx, claims, "password", nil, false, now); err != nil {
			t.Fatal(err)
		}
		_, err = enforceSessionRevoke(ctx, networkId, server.NewId(), "single", claims.SessionId, nil)
		if err != nil {
			t.Fatal(err)
		}
		write(now.Unix()+120, "revived")
		if err = server.RedisAuth(ctx, func(ctx context.Context, r server.RedisClient) error {
			n, err := r.Exists(ctx, SessionKey(networkId, "u:"+claims.SessionId.String())).Result()
			if n != 0 {
				t.Fatal("in-flight sample recreated revoked observation")
			}
			return err
		}); err != nil {
			t.Fatal(err)
		}
		// Opposite process phases can suppress an observation for almost 120s.
		a := sessionUseThrottle{attempts: map[[2]server.Id]int64{}}
		b := sessionUseThrottle{attempts: map[[2]server.Id]int64{}}
		if !a.admit(networkId, *claims.SessionId, 0) || !b.admit(networkId, *claims.SessionId, 59000) || b.admit(networkId, *claims.SessionId, 60000) || !b.admit(networkId, *claims.SessionId, 119000) {
			t.Fatal("cross-process sample phase changed")
		}
	})
}
func TestSessionAuthorityPrecedenceRetainsCommonChecks(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		networkId, userId, deviceId := server.NewId(), server.NewId(), server.NewId()
		a, b, c := server.NewId(), server.NewId(), server.NewId()
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO network_user(user_id,user_name,auth_type,verified) VALUES($1,'session-regression','password',true)`, userId))
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO network(network_id,network_name,admin_user_id) VALUES($1,'session-regression',$2)`, networkId, userId))
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO device(device_id,network_id,device_name,device_spec) VALUES($1,$2,'test','test')`, deviceId, networkId))
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO network_client(client_id,network_id,device_id,active,source_client_id) VALUES($1,$4,$5,true,NULL),($2,$4,$5,false,$1),($3,$4,$5,true,$2)`, a, b, c, networkId, deviceId))
		})
		claims := NewByJwt(networkId, userId, "test", false, false).Client(deviceId, c)
		claims.RootClientId = nil
		if ValidateByJwtState(ctx, claims, true) == nil {
			t.Fatal("legacy intermediate removal ignored")
		}
		claims.RootClientId = &a
		if err := ValidateByJwtState(ctx, claims, true); err != nil {
			t.Fatal("root branch unexpectedly required intermediate activity", err)
		}
		sid := server.NewId()
		claims.SessionId = &sid
		missing := server.NewId()
		claims.RootClientId = &missing
		if err := ValidateByJwtStateForParent(ctx, claims, b); err != nil {
			t.Fatal("session branch fell back to root", err)
		}
		if ValidateByJwtStateForParent(ctx, claims, a) == nil {
			t.Fatal("parent ownership predicate lost")
		}
		for _, mode := range []string{"session", "root", "recursive"} {
			claims.SessionId = nil
			claims.RootClientId = nil
			if mode == "session" {
				claims.SessionId = &sid
			} else if mode == "root" {
				claims.RootClientId = &a
			}
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(ctx, `UPDATE network_client SET active=true WHERE client_id=$1`, b))
				server.RaisePgResult(tx.Exec(ctx, `UPDATE network_client SET active=false WHERE client_id=$1`, c))
			})
			if ValidateByJwtState(ctx, claims, true) == nil {
				t.Fatal(mode, "accepted inactive own client")
			}
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(ctx, `UPDATE network_client SET active=true WHERE client_id=$1`, c))
				server.RaisePgResult(tx.Exec(ctx, `UPDATE network_user SET credential_change_time=$2 WHERE user_id=$1`, userId, claims.CreateTime.Add(time.Second)))
			})
			if ValidateByJwtState(ctx, claims, true) == nil {
				t.Fatal(mode, "ignored credential rotation")
			}
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(ctx, `UPDATE network_user SET credential_change_time=$2 WHERE user_id=$1`, userId, time.Unix(0, 0).UTC()))
			})
		}
	})
}

func TestSessionResetCutoffKeepsOnlyNewNanosecondLineage(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		now := server.NowUtc()
		cutoff := now.Truncate(time.Millisecond).Add(500 * time.Microsecond)
		network := server.NewId()
		before, after := sessionFixtureCredential(network, now), sessionFixtureCredential(network, now)
		before.CreateTime = cutoff.Add(-time.Nanosecond)
		after.CreateTime = cutoff.Add(time.Nanosecond)
		for _, value := range []*ByJwt{before, after} {
			if _, err := RegisterNetworkSession(ctx, value, "password", nil, false, now); err != nil {
				t.Fatal(err)
			}
		}
		result, err := enforceSessionRevoke(ctx, network, server.NewId(), "reset", nil, nil, cutoff)
		if err != nil || result.RevokedCount != 1 || len(result.TargetSessionIds) != 1 || result.TargetSessionIds[0] != *before.SessionId {
			t.Fatal("reset aliased distinct sub-millisecond sign-ins", result, err)
		}
		if err = CheckSession(ctx, network, *after.SessionId); err != nil {
			t.Fatal("new sign-in retired", err)
		}
	})
}
func TestSessionRetainedCapacityDoesNotEvictAcceptedMarker(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		now := server.NowUtc()
		network := server.NewId()
		existing := sessionFixtureCredential(network, now)
		if _, err := RegisterNetworkSession(ctx, existing, "password", nil, false, now); err != nil {
			t.Fatal(err)
		}
		server.Raise(server.RedisAuth(ctx, func(ctx context.Context, r server.RedisClient) error {
			return r.Eval(ctx, `for i=1,9999 do redis.call('ZADD',KEYS[1],ARGV[1],'retained-'..i) end;return 1`, []string{SessionKey(network, "held")}, DeadlineMillis(existing.AcceptUntil().Add(SessionRetentionMargin))).Err()
		}))
		if _, err := RegisterNetworkSession(ctx, sessionFixtureCredential(network, now), "password", nil, false, now); err == nil {
			t.Fatal("retained cap evicted reservation")
		}
		if _, err := RegisterNetworkSession(ctx, existing, "password", nil, false, now); err != nil {
			t.Fatal("existing refresh refused at retained capacity", err)
		}
		if _, err := enforceSessionRevoke(ctx, network, server.NewId(), "single", existing.SessionId, nil); err != nil {
			t.Fatal(err)
		}
		if !errors.Is(CheckSession(ctx, network, *existing.SessionId), ErrSessionRevoked) {
			t.Fatal("full retained inventory lost acknowledged marker")
		}
	})
}

func TestSessionExpiredReservationIsNotAnActiveRevokeTarget(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		now := server.NowUtc()
		network, sid := server.NewId(), server.NewId()
		server.Raise(server.RedisAuth(ctx, func(ctx context.Context, r server.RedisClient) error {
			// A sweeper/list has pruned live membership while retention correctly
			// continues. That capacity reservation must not turn an expired session
			// into a new successful revocation or consume the action quota.
			return r.ZAdd(ctx, SessionKey(network, "held"), redis.Z{Score: float64(now.Add(time.Minute).UnixMilli()), Member: sid.String()}).Err()
		}))
		_, err := enforceSessionRevoke(ctx, network, server.NewId(), "single", &sid, nil)
		var refusal *SessionError
		if !errors.As(err, &refusal) || refusal.Status != 404 {
			t.Fatal("expired reservation was revoked", err)
		}
		if err = CheckSession(ctx, network, sid); err != nil {
			t.Fatal("failed expired revoke manufactured a marker", err)
		}
	})
}

func TestSessionNotificationIncarnationSurvivesCounterLoss(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		network := server.NewId()
		before, err := readSessionInventory(ctx, network, server.NowUtc())
		if err != nil {
			t.Fatal(err)
		}
		server.Raise(server.RedisAuth(ctx, func(ctx context.Context, r server.RedisClient) error {
			if err := r.Set(ctx, SessionKey(network, "eid"), 10000, time.Hour).Err(); err != nil {
				return err
			}
			return r.Del(ctx, SessionKey(network, "generation")).Err()
		}))
		after, err := readSessionInventory(ctx, network, server.NowUtc())
		if err != nil || after.Generation == before.Generation || after.EventId != 1 {
			t.Fatal("lost incarnation reused high counter", after, err)
		}
	})
}

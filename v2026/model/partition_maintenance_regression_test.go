// Retention and delayed publications must preserve durable session identity
// and the latest committed provide state. Races use explicit commit barriers.
package model

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/urfoundation/sn/v2026/protocol"
	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/session"
)

type providePublicationCommitPauseKey struct{}

// Holds the provide after PostgreSQL commits and before its Redis publication.
type providePublicationCommitPause struct {
	reached chan struct{}
	resume  chan struct{}
	once    sync.Once
}

// Valid original receipts outlive the journal, and must still support the next
// retirement and admission without introducing a permanent unsigned sequence.
func TestProviderWorkSessionRetentionKeepsOpenAdmission(t *testing.T) {
	testProviderWorkSessionRetentionChain(t, providerWorkSessionRetentionTestOptions{})
}

// The pre-cutover journal has the same live-admission dependency as a week.
func TestProviderWorkSessionLegacyRetentionKeepsOpenAdmission(t *testing.T) {
	testProviderWorkSessionRetentionChain(t, providerWorkSessionRetentionTestOptions{legacy: true})
}

// An extender change retires and re-admits the same connected row. Its newer
// admission makes the older partition disposable while the connection is open.
func TestProviderWorkSessionRetentionAllowsSupersededAdmission(t *testing.T) {
	testProviderWorkSessionRetentionChain(t, providerWorkSessionRetentionTestOptions{replaceAdmission: true})
}

// The legacy layout and a replacement admission use the same signed fixture.
type providerWorkSessionRetentionTestOptions struct {
	legacy           bool
	replaceAdmission bool
}

// Seed a historical signed chain and its still-open connection. The drop must
// refuse until the real retirement trigger and signer finish, then reconnect
// must extend the retained receipts after the expired events have gone.
func testProviderWorkSessionRetentionChain(t *testing.T, options providerWorkSessionRetentionTestOptions) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		now := server.NowUtc()
		oldWeek := server.PartitionGranularityWeekly.AddRanges(server.PartitionGranularityWeekly.RangeStart(now), -14)
		admitWeek := oldWeek
		expired := server.PartitionGranularityWeekly.RangeOf(oldWeek)
		if options.legacy {
			admitWeek = oldWeek.Add(-24 * time.Hour)
			expired = server.PartitionRange{Upper: oldWeek, Legacy: true}
		}
		key := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{0x95}, ed25519.SeedSize))
		authority := protocol.ProviderWorkSourceAuthority{
			DomainHash: [32]byte{0x96}, SourceId: server.NewId().String(), Generation: server.NewId().String(),
			PublicKey: [32]byte(key.Public().(ed25519.PublicKey)), FromUnixMicro: admitWeek.Add(-time.Hour).UnixMicro(),
			ThroughUnixMicro: now.Add(time.Hour).UnixMicro(), MaxEndpointEvents: 4096, MaxCohortMembers: 64,
		}
		source, err := NewProviderWorkSessionSource(authority, key)
		if err != nil {
			t.Fatal(err)
		}
		ctx := WithProviderWorkSessionSource(t.Context(), source)
		server.Testing_PartitionProviderWorkSessionEventsAt(ctx, oldWeek)
		var family *server.PartitionFamily
		for _, registered := range server.PartitionFamilies() {
			if registered.Name == "provider_work_session_event" {
				family = registered
			}
		}
		if family == nil {
			t.Fatal("missing event family")
		}
		if _, err := family.EnsureRangeFor(ctx, now); err != nil {
			t.Fatal(err)
		}
		clientId, networkId, connectionId := server.NewId(), server.NewId(), server.NewId()
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO network_client (client_id, network_id, active) VALUES ($1, $2, true)`, clientId, networkId))
			// Historical fixture only: the production trigger always uses now.
			server.RaisePgResult(tx.Exec(ctx, `ALTER TABLE network_client_connection DISABLE TRIGGER provider_work_session_mutation`))
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO network_client_connection
				(client_id,connection_id,connect_time,connection_host,connection_service,connection_block,client_address_hash,client_address_port,handler_id)
				VALUES ($1,$2,$3,'synthetic.example','test','test',$4,10001,$5)`, clientId, connectionId, admitWeek, make([]byte, 32), server.NewId()))
			server.RaisePgResult(tx.Exec(ctx, `ALTER TABLE network_client_connection ENABLE TRIGGER provider_work_session_mutation`))
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO provider_work_session_head (client_id, sequence) VALUES ($1, 2)`, clientId))
			var previous [32]byte
			for index, kind := range []string{"baseline", "admit"} {
				sequence := uint64(index + 1)
				at := admitWeek.Add(time.Duration(index+1) * time.Hour)
				body := protocol.ProviderWorkSessionEvent{ClientId: clientId.String(), NetworkId: networkId.String(), Sequence: sequence, Kind: kind, ObservedAtUnixMicro: at.UnixMicro(), PreviousHash: previous}
				var eventConnectionId *server.Id
				if kind == "admit" {
					body.ConnectionId = connectionId.String()
					eventConnectionId = &connectionId
				}
				_, raw, hash, err := source.sign(ctx, protocol.ProviderWorkReceipt{Session: &body}, at.UnixMicro())
				server.Raise(err)
				server.RaisePgResult(tx.Exec(ctx, `INSERT INTO provider_work_session_event (client_id,sequence,network_id,connection_id,kind,observed_at,transaction_id) VALUES ($1,$2,$3,$4,$5,$6,txid_current())`, clientId, sequence, networkId, eventConnectionId, kind, at))
				server.RaisePgResult(tx.Exec(ctx, `INSERT INTO provider_work_session_receipt (client_id,sequence,receipt_hash,original) VALUES ($1,$2,$3,$4)`, clientId, sequence, hash[:], raw))
				previous = hash
			}
		}, server.TxReadCommitted)
		var blocked *server.PartitionDropBlockedError
		if _, err := family.DropRange(ctx, expired); !errors.As(err, &blocked) {
			t.Fatalf("retention discarded a live admission: %v", err)
		}
		server.Tx(ctx, func(tx server.PgTx) {
			if options.replaceAdmission {
				server.RaisePgResult(tx.Exec(ctx, `UPDATE network_client_connection SET extender_id=$2 WHERE connection_id=$1`, connectionId, server.NewId()))
			} else {
				// The directory is no longer evidence of the admitted identity.
				server.RaisePgResult(tx.Exec(ctx, `UPDATE network_client SET network_id=$2 WHERE client_id=$1`, clientId, server.NewId()))
				server.RaisePgResult(tx.Exec(ctx, `UPDATE network_client_connection SET connected=false,disconnect_time=$2 WHERE connection_id=$1`, connectionId, now))
			}
			server.Raise(providerWorkSignSessionEventsInTx(ctx, tx, []server.Id{clientId}))
		}, server.TxReadCommitted)
		if _, err := family.DropRange(ctx, expired); err != nil {
			t.Fatal("retired admission still blocks retention", err)
		}
		if !options.replaceAdmission {
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(ctx, `UPDATE network_client SET network_id=$2 WHERE client_id=$1`, clientId, networkId))
				server.RaisePgResult(tx.Exec(ctx, `INSERT INTO network_client_connection
				(client_id,connection_id,connect_time,connection_host,connection_service,connection_block,client_address_hash,client_address_port,handler_id)
				VALUES ($1,$2,$3,'synthetic.example','test','test',$4,10001,$5)`, clientId, server.NewId(), now, make([]byte, 32), server.NewId()))
				server.Raise(providerWorkSignSessionEventsInTx(ctx, tx, []server.Id{clientId}))
			}, server.TxReadCommitted)
		}
		server.Db(ctx, func(conn server.PgConn) {
			var receipts, sequence int
			server.Raise(conn.QueryRow(ctx, `SELECT (SELECT count(*) FROM provider_work_session_receipt WHERE client_id=$1), (SELECT sequence FROM provider_work_session_head WHERE client_id=$1)`, clientId).Scan(&receipts, &sequence))
			if receipts != 4 || sequence != 4 {
				t.Fatalf("retention broke signed chain: receipts=%d head=%d, want four signed events", receipts, sequence)
			}
			rows, err := conn.Query(ctx, `SELECT receipt_hash,original FROM provider_work_session_receipt WHERE client_id=$1 ORDER BY sequence`, clientId)
			server.WithPgResult(rows, err, func() {
				var previous [32]byte
				for rows.Next() {
					var hash, raw []byte
					server.Raise(rows.Scan(&hash, &raw))
					receipt, err := protocol.DecodeProviderWorkReceipt(ctx, raw)
					server.Raise(err)
					server.Raise(protocol.VerifyProviderWorkReceiptAuthority(ctx, receipt, source.authority))
					if receipt.Session.NetworkId != networkId.String() || receipt.Session.PreviousHash != previous {
						t.Fatal("retention changed the admitted identity or signed chain", receipt.Session)
					}
					copy(previous[:], hash)
				}
			})
		})
	})
}

func (self *providePublicationCommitPause) TraceQueryStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryStartData) context.Context {
	return ctx
}

func (self *providePublicationCommitPause) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryEndData) {
	if ctx.Value(providePublicationCommitPauseKey{}) != self || data.Err != nil || data.CommandTag.String() != "COMMIT" {
		return
	}
	self.once.Do(func() {
		close(self.reached)
		select {
		case <-self.resume:
		case <-ctx.Done():
		}
	})
}

// Deactivation must win even when an older provide publishes after it commits.
func TestSetProvidePublicationAfterDeactivationCannotRestoreKeys(t *testing.T) {
	testProvidePublicationAfterSuccessor(t, true)
}

// The last committed secret set also wins when two provides publish in reverse.
func TestSetProvidePublicationAfterKeyRotationKeepsLatestKeys(t *testing.T) {
	testProvidePublicationAfterSuccessor(t, false)
}

// Stop the older provide at a successful COMMIT, then finish the newer mutation
// and its Redis writes before releasing the old publication.
func testProvidePublicationAfterSuccessor(t *testing.T, deactivate bool) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
		defer cancel()
		networkId, clientId := server.NewId(), server.NewId()
		Testing_CreateDevice(ctx, networkId, server.NewId(), clientId, "test", "test")
		AddNetworkPeer(ctx, networkId, &NetworkPeer{ClientId: clientId}, server.NewId(), time.Hour)
		pause := &providePublicationCommitPause{reached: make(chan struct{}), resume: make(chan struct{})}
		scope, err := server.NewTestPgQueryScope(ctx, pause)
		if err != nil {
			t.Fatal(err)
		}
		defer func() {
			if err := scope.Close(); err != nil {
				t.Error(err)
			}
		}()
		provideDone := runProvideRetirementCall(func() {
			SetProvide(context.WithValue(ctx, providePublicationCommitPauseKey{}, pause), clientId, map[ProvideMode][]byte{ProvideModePublic: provideRetirementTestKey(0x97)})
		})
		joined := false
		defer func() {
			select {
			case <-pause.resume:
			default:
				close(pause.resume)
			}
			if !joined {
				select {
				case <-provideDone:
				case <-ctx.Done():
				}
			}
		}()
		select {
		case <-pause.reached:
		case err := <-provideDone:
			joined = true
			t.Fatalf("provide ended before commit barrier: %v", err)
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
		wantKeys := map[ProvideMode][]byte{}
		if deactivate {
			removed, err := RemoveNetworkClient(&RemoveNetworkClientArgs{ClientId: clientId}, &session.ClientSession{Ctx: ctx, ByJwt: &session.ByJwt{NetworkId: networkId}})
			if err != nil || removed.Error != nil {
				t.Fatalf("deactivate: %v, %+v", err, removed)
			}
		} else {
			wantKeys = map[ProvideMode][]byte{ProvideModePublic: provideRetirementTestKey(0x98), ProvideModeStream: provideRetirementTestKey(0x99)}
			SetProvide(ctx, clientId, wantKeys)
		}
		assertProvideSecretKeys(t, ctx, clientId, wantKeys)
		close(pause.resume)
		select {
		case err := <-provideDone:
			joined = true
			if err != nil {
				t.Fatal(err)
			}
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
		assertProvideSecretKeys(t, ctx, clientId, wantKeys)
		if deactivate {
			if _, err := GetProvideSecretKey(ctx, clientId, ProvideModePublic); err == nil {
				t.Error("inactive client still returns the revoked public secret")
			}
		}
		peer := GetNetworkPeerMember(ctx, networkId, clientId)
		wantModes := []ProvideMode{}
		for mode := range wantKeys {
			wantModes = append(wantModes, mode)
		}
		slices.Sort(wantModes)
		if peer == nil || !slices.Equal(peer.ProvideModes, wantModes) {
			t.Fatalf("late publication changed peer modes: peer=%+v, want=%v", peer, wantModes)
		}
	})
}

// A failed best-effort hold leaves an older token in Redis. Even then, a late
// successful publication must take back keys that no longer match PostgreSQL.
func TestProvidePublicationConfirmsKeysAfterMissedInvalidation(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		clientId := server.NewId()
		Testing_CreateDevice(ctx, server.NewId(), server.NewId(), clientId, "test", "test")
		oldKeys := map[ProvideMode][]byte{ProvideModePublic: provideRetirementTestKey(0xa1)}
		SetProvide(ctx, clientId, oldKeys)
		token := server.NewId().String()
		server.Redis(ctx, func(r server.RedisClient) {
			keys := append([]string{provideMirrorHoldKey(clientId)}, provideMirrorKeys(clientId)...)
			server.Raise(provideMirrorHoldScript.Run(ctx, r, keys, token, provideMirrorHoldTtl.Milliseconds()).Err())
		})
		// The next commit succeeded while its Redis hold was unavailable.
		newKey := provideRetirementTestKey(0xa2)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE provide_key SET secret_key=$2 WHERE client_id=$1`, clientId, newKey))
		})
		publishProvideMirror(ctx, clientId, token, oldKeys)
		server.Redis(ctx, func(r server.RedisClient) {
			exists, err := r.Exists(ctx, provideMirrorKeys(clientId)...).Result()
			if err != nil || exists != 0 {
				t.Fatalf("unconfirmed publication kept mirror keys: count=%d error=%v", exists, err)
			}
		})
		assertProvideSecretKeys(t, ctx, clientId, map[ProvideMode][]byte{ProvideModePublic: newKey})
	})
}

// A peer update can be delayed after its canonical read too. The deactivation
// publishes while that old read is in flight; its version forces a fresh read.
func TestNetworkPeerProvidePublicationReloadsAfterDeactivation(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		networkId, clientId := server.NewId(), server.NewId()
		Testing_CreateDevice(ctx, networkId, server.NewId(), clientId, "test", "test")
		AddNetworkPeer(ctx, networkId, &NetworkPeer{ClientId: clientId}, server.NewId(), time.Hour)
		SetProvide(ctx, clientId, map[ProvideMode][]byte{ProvideModePublic: provideRetirementTestKey(0xa3)})
		reads := 0
		updated := updateNetworkPeerProvideModesWithLoader(ctx, networkId, clientId, func() map[ProvideMode]bool {
			modes := map[ProvideMode]bool{}
			for mode := range loadProvideSecretKeys(ctx, clientId) {
				modes[mode] = true
			}
			reads++
			if reads == 1 {
				removed, err := RemoveNetworkClient(&RemoveNetworkClientArgs{ClientId: clientId}, &session.ClientSession{Ctx: ctx, ByJwt: &session.ByJwt{NetworkId: networkId}})
				if err != nil || removed.Error != nil {
					t.Fatal("deactivation failed", err, removed)
				}
			}
			return modes
		})
		peer := GetNetworkPeerMember(ctx, networkId, clientId)
		if !updated || reads != 2 || peer == nil || len(peer.ProvideModes) != 0 {
			t.Fatalf("stale canonical read restored peer modes: updated=%t reads=%d peer=%+v", updated, reads, peer)
		}
	})
}

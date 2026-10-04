// Real session, reservation and settlement owners produce the originals used
// by these regressions; fixtures never synthesize a publisher party census.
package model

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/urfoundation/sn/protocol"
	"github.com/urnetwork/connect"
	coreprotocol "github.com/urnetwork/connect/protocol"
	"github.com/urnetwork/server"
	"google.golang.org/protobuf/proto"
)

type providerWorkSessionFixture struct {
	ctx                     context.Context
	source                  *ProviderWorkSessionSource
	sourceId                server.Id
	destinationId           server.Id
	intermediaryId          server.Id
	sourceNetworkId         server.Id
	destinationNetworkId    server.Id
	intermediaryNetworkId   server.Id
	handlerId               server.Id
	sourceConnectionId      server.Id
	destinationConnectionId server.Id
}

// A dedicated synthetic source signs actual model operations independently of
// all artifact/SDK keys. Both endpoint baselines precede their first admission.
func newProviderWorkSessionFixture(t testing.TB) *providerWorkSessionFixture {
	t.Helper()
	Testing_ResetExtenderAddressCache()
	t.Cleanup(Testing_ResetExtenderAddressCache)
	key := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{109}, ed25519.SeedSize))
	now := server.NowUtc()
	authority := protocol.ProviderWorkSourceAuthority{DomainHash: [32]byte{107}, SourceId: server.NewId().String(), Generation: server.NewId().String(), PublicKey: [32]byte(key.Public().(ed25519.PublicKey)), FromUnixMicro: now.Add(-time.Hour).UnixMicro(), ThroughUnixMicro: now.Add(time.Hour).UnixMicro(), MaxEndpointEvents: 4096, MaxCohortMembers: 64, DirectoryPublicKeys: [][32]byte{}}
	source, err := NewProviderWorkSessionSource(authority, key)
	if err != nil {
		t.Fatal(err)
	}
	ctx := WithProviderWorkSessionSource(t.Context(), source)
	f := &providerWorkSessionFixture{ctx: ctx, source: source, sourceId: server.NewId(), destinationId: server.NewId(), intermediaryId: server.NewId(), sourceNetworkId: server.NewId(), destinationNetworkId: server.NewId(), intermediaryNetworkId: server.NewId()}
	addContractPayoutTestClients(ctx, map[server.Id]server.Id{f.sourceId: f.sourceNetworkId, f.destinationId: f.destinationNetworkId, f.intermediaryId: f.intermediaryNetworkId})
	f.handlerId = CreateNetworkClientHandler(ctx)
	f.sourceConnectionId, _, _, _, err = ConnectNetworkClientWithIpFamily(ctx, f.sourceId, "192.0.2.10:10001", f.handlerId, 4)
	if err != nil {
		t.Fatal(err)
	}
	f.destinationConnectionId, _, _, _, err = ConnectNetworkClientWithIpFamily(ctx, f.destinationId, "192.0.2.11:10002", f.handlerId, 4)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func (self *providerWorkSessionFixture) contract(t testing.TB) server.Id {
	t.Helper()
	id, err := CreateContractNoEscrow(self.requestContext(t, nil), self.sourceNetworkId, self.sourceId, self.destinationNetworkId, self.destinationId, 121)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// This models the exact ingress capability; producer unit fixtures do not
// substitute a newly minted hash after SQL admission has already happened.
func (self *providerWorkSessionFixture) requestContext(t testing.TB, intermediaryIds []server.Id) context.Context {
	t.Helper()
	request := &coreprotocol.CreateContract{DestinationId: self.destinationId.Bytes(), TransferByteCount: 121}
	for _, id := range intermediaryIds {
		request.IntermediaryIds = append(request.IntermediaryIds, id.Bytes())
	}
	if len(intermediaryIds) > 0 {
		version := uint32(1)
		request.StreamVersion = &version
	}
	frame, err := connect.ToFrame(request, connect.DefaultProtocolVersion)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := proto.Marshal(frame)
	if err != nil {
		t.Fatal(err)
	}
	return WithProviderWorkRequestFrameHash(self.ctx, sha256.Sum256(raw))
}

func (self *providerWorkSessionFixture) close(t testing.TB, id server.Id) {
	t.Helper()
	if err := CloseContract(self.ctx, id, self.sourceId, 121, false); err != nil {
		t.Fatal(err)
	}
	if err := CloseContract(self.ctx, id, self.destinationId, 121, false); err != nil {
		t.Fatal(err)
	}
}

func providerWorkFixtureReceipts(t testing.TB, ctx context.Context, contractIds ...server.Id) []protocol.ProviderWorkReceipt {
	t.Helper()
	raw, err := ListProviderWorkOriginals(ctx, contractIds)
	if err != nil {
		t.Fatal(err)
	}
	receipts := make([]protocol.ProviderWorkReceipt, 0, len(raw))
	for _, original := range raw {
		receipt, err := protocol.DecodeProviderWorkReceipt(ctx, original)
		if err != nil {
			t.Fatal(err)
		}
		receipts = append(receipts, receipt)
	}
	return receipts
}

func providerWorkFixtureReservation(t testing.TB, receipts []protocol.ProviderWorkReceipt, id server.Id) protocol.ProviderWorkReceipt {
	t.Helper()
	for _, receipt := range receipts {
		if receipt.Reservation != nil && receipt.Reservation.ContractId == id.String() {
			return receipt
		}
	}
	t.Fatal("live reservation original missing")
	return protocol.ProviderWorkReceipt{}
}

func providerWorkFixtureEndpoint(t testing.TB, f *providerWorkSessionFixture, receipts []protocol.ProviderWorkReceipt, id server.Id) []protocol.ProviderWorkEndpointState {
	t.Helper()
	events := []protocol.ProviderWorkReceipt{}
	for _, receipt := range receipts {
		if receipt.Session != nil && receipt.Session.ClientId == id.String() {
			events = append(events, receipt)
		}
	}
	slices.SortFunc(events, func(a, b protocol.ProviderWorkReceipt) int {
		if a.Session.Sequence < b.Session.Sequence {
			return -1
		}
		if a.Session.Sequence > b.Session.Sequence {
			return 1
		}
		return 0
	})
	states, err := protocol.ReplayProviderWorkEndpoint(f.ctx, f.source.authority, events)
	if err != nil {
		t.Fatal(err)
	}
	return states
}

func TestProviderWorkSessionActualAdmissionReservationClose(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		f := newProviderWorkSessionFixture(t)
		id := f.contract(t)
		f.close(t, id)
		receipts := providerWorkFixtureReceipts(t, f.ctx, id)
		reservation := providerWorkFixtureReservation(t, receipts, id)
		if !reservation.Reservation.Complete || reservation.Reservation.SourceId != f.sourceId.String() || reservation.Reservation.DestinationNetworkId != f.destinationNetworkId.String() {
			t.Fatal("actual original admission lost complete endpoint identity", reservation.Reservation)
		}
		for _, clientId := range []server.Id{f.sourceId, f.destinationId} {
			states := providerWorkFixtureEndpoint(t, f, receipts, clientId)
			if len(states) != 2 || states[0].ActiveConnections != 0 || states[1].ActiveConnections != 1 || states[1].ActiveExtenders != 0 {
				t.Fatal("actual first admission did not preserve empty genesis", states)
			}
		}
		reservationHash, err := reservation.ContentHash(f.ctx)
		if err != nil {
			t.Fatal(err)
		}
		var outcome *protocol.ProviderWorkOutcome
		for _, receipt := range receipts {
			if err := protocol.VerifyProviderWorkReceiptAuthority(f.ctx, receipt, f.source.authority); err != nil {
				t.Fatal(err)
			}
			if receipt.Outcome != nil {
				outcome = receipt.Outcome
			}
		}
		if outcome == nil || outcome.ReservationHash != reservationHash || outcome.StreamHash != ([32]byte{}) || outcome.SourceBytes != 121 || outcome.DestinationBytes != 121 || !outcome.SourceComplete || !outcome.DestinationComplete || outcome.Outcome != ContractOutcomeSettled {
			t.Fatal("live terminal decision was not retained", outcome)
		}
		server.Db(f.ctx, func(conn server.PgConn) {
			var at time.Time
			server.Raise(conn.QueryRow(f.ctx, `SELECT close_time FROM transfer_contract WHERE contract_id=$1`, id).Scan(&at))
			if at.UnixMicro() != outcome.ClosedAtUnixMicro {
				t.Fatal("publisher time differs from actual signed outcome")
			}
		})
		changed := reservation
		body := *changed.Reservation
		body.DestinationId = server.NewId().String()
		changed.Reservation = &body
		if err := changed.Verify(f.ctx); !errors.Is(err, protocol.ErrProviderWorkIntegrity) {
			t.Fatal("changed original provider accepted", err)
		}
	})
}

func TestProviderWorkSessionIdleRetirementAndLaterAdmission(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		f := newProviderWorkSessionFixture(t)
		if err := DisconnectNetworkClient(f.ctx, f.sourceConnectionId); err != nil {
			t.Fatal(err)
		}
		if err := DisconnectNetworkClient(f.ctx, f.destinationConnectionId); err != nil {
			t.Fatal(err)
		}
		idle := f.contract(t)
		for _, clientId := range []server.Id{f.sourceId, f.destinationId} {
			states := providerWorkFixtureEndpoint(t, f, providerWorkFixtureReceipts(t, f.ctx, idle), clientId)
			if len(states) != 3 || states[2].ActiveConnections != 0 {
				t.Fatal("original idle census disappeared", states)
			}
		}
		if !providerWorkFixtureReservation(t, providerWorkFixtureReceipts(t, f.ctx, idle), idle).Reservation.Complete {
			t.Fatal("actual retired endpoint lost its complete zero census")
		}
		if _, _, _, _, err := ConnectNetworkClientWithIpFamily(f.ctx, f.sourceId, "192.0.2.12:10003", f.handlerId, 4); err != nil {
			t.Fatal(err)
		}
		later := f.contract(t)
		first := providerWorkFixtureReservation(t, providerWorkFixtureReceipts(t, f.ctx, idle, later), idle)
		next := providerWorkFixtureReservation(t, providerWorkFixtureReceipts(t, f.ctx, idle, later), later)
		if first.Reservation.SourceHead.Sequence != 3 || next.Reservation.SourceHead.Sequence != 4 || !next.Reservation.Complete {
			t.Fatal("new admission rewrote an earlier exact reservation cut")
		}
	})
}

func TestProviderWorkSessionLegacyGapCannotBeSignedAfterCleanup(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		f := newProviderWorkSessionFixture(t)
		unsigned := WithProviderWorkSessionSource(f.ctx, nil)
		if err := DisconnectNetworkClient(unsigned, f.sourceConnectionId); err != nil {
			t.Fatal(err)
		}
		if _, _, _, _, err := ConnectNetworkClientWithIpFamily(f.ctx, f.sourceId, "192.0.2.13:10004", f.handlerId, 4); err != nil {
			t.Fatal(err)
		}
		id := f.contract(t)
		receipt := providerWorkFixtureReservation(t, providerWorkFixtureReceipts(t, f.ctx, id), id)
		if receipt.Reservation.Complete {
			t.Fatal("later signer healed the original unsigned retirement gap")
		}
		server.Db(f.ctx, func(conn server.PgConn) {
			var events, originals int
			server.Raise(conn.QueryRow(f.ctx, `SELECT (SELECT count(*) FROM provider_work_session_event WHERE client_id=$1),(SELECT count(*) FROM provider_work_session_receipt WHERE client_id=$1)`, f.sourceId).Scan(&events, &originals))
			if events != 4 || originals != 2 {
				t.Fatal("original history was backfilled or reset", events, originals)
			}
		})
	})
}

func TestProviderWorkSessionOriginalsSurviveMutableDirectoryAndCleanup(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		f := newProviderWorkSessionFixture(t)
		id := f.contract(t)
		f.close(t, id)
		before, err := ListProviderWorkOriginals(f.ctx, []server.Id{id})
		if err != nil {
			t.Fatal(err)
		}
		if err := DisconnectNetworkClient(f.ctx, f.sourceConnectionId); err != nil {
			t.Fatal(err)
		}
		server.Tx(f.ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(f.ctx, `UPDATE network_client SET network_id=$2 WHERE client_id=$1`, f.destinationId, server.NewId()))
			server.RaisePgResult(tx.Exec(f.ctx, `DELETE FROM network_client_connection WHERE connection_id=$1`, f.sourceConnectionId))
		})
		after, err := ListProviderWorkOriginals(f.ctx, []server.Id{id})
		if err != nil {
			t.Fatal(err)
		}
		if !slices.EqualFunc(before, after, bytes.Equal) {
			t.Fatal("original reservation was rebuilt from mutable session or directory rows")
		}
		var mutationErr error
		server.Db(f.ctx, func(conn server.PgConn) {
			_, mutationErr = conn.Exec(f.ctx, `UPDATE provider_work_reservation_original SET original=$2 WHERE contract_id=$1`, id, []byte("changed"))
		})
		if mutationErr == nil {
			t.Fatal("immutable original party record could be rewritten")
		}
	})
}

func TestProviderWorkSessionActualStreamBirthAndInheritedCohort(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		f := newProviderWorkSessionFixture(t)
		frame, err := connect.ToFrame(&coreprotocol.CreateContract{DestinationId: f.destinationId.Bytes(), TransferByteCount: 121, IntermediaryIds: [][]byte{f.intermediaryId.Bytes()}}, connect.DefaultProtocolVersion)
		if err != nil {
			t.Fatal(err)
		}
		requestFrame, err := proto.Marshal(frame)
		if err != nil {
			t.Fatal(err)
		}
		ctx := WithProviderWorkRequestFrameHash(f.ctx, sha256.Sum256(requestFrame))
		origin, err := CreateContractNoEscrow(ctx, f.sourceNetworkId, f.sourceId, f.destinationNetworkId, f.destinationId, 121)
		if err != nil {
			t.Fatal(err)
		}
		streamId := AddToStream(ctx, origin, f.sourceId, f.destinationId, []server.Id{f.intermediaryId})
		if err := SetContractStream(ctx, origin, streamId, []server.Id{f.intermediaryId}); err != nil {
			t.Fatal(err)
		}
		inherited := f.contract(t)
		joined, ok := AddContractToPairStream(f.ctx, inherited, f.sourceId, f.destinationId)
		if !ok || joined != streamId {
			t.Fatal("actual pair stream inheritance failed", joined, ok)
		}
		if err := SetContractStream(f.ctx, inherited, joined, nil); err != nil {
			t.Fatal(err)
		}
		f.close(t, origin)
		f.close(t, inherited)
		originals := providerWorkFixtureReceipts(t, f.ctx, origin, inherited)
		var cohortHash [32]byte
		cohortCount := 0
		outcomeCount := 0
		for _, receipt := range originals {
			if receipt.Stream != nil {
				cohortCount++
				cohortHash, _ = receipt.ContentHash(f.ctx)
				if receipt.Stream.OriginContractId != origin.String() || receipt.Stream.StreamId != streamId.String() || len(receipt.Stream.Intermediaries) != 1 || receipt.Stream.Intermediaries[0].ClientId != f.intermediaryId.String() || receipt.Stream.Intermediaries[0].NetworkId != f.intermediaryNetworkId.String() {
					t.Fatal("original stream party or provider identity changed", receipt.Stream)
				}
			}
		}
		for _, receipt := range originals {
			if receipt.Outcome != nil {
				outcomeCount++
				if receipt.Outcome.StreamHash != cohortHash {
					t.Fatal("inherited close did not retain original stream cohort")
				}
			}
		}
		if cohortCount != 1 || outcomeCount != 2 || cohortHash == ([32]byte{}) {
			t.Fatal("shared original cohort was missing or regenerated", cohortCount, outcomeCount)
		}
	})
}

func TestProviderWorkSessionPaidAndFreeAdmissionsKeepEqualOriginalWork(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		f := newProviderWorkSessionFixture(t)
		addContractPayoutTestBalance(f.ctx, f.sourceNetworkId, 1000)
		var escrow *TransferEscrow
		var posts []func() any
		server.Tx(f.ctx, func(tx server.PgTx) {
			var err error
			escrow, posts, err = createTransferEscrowInTx(f.requestContext(t, nil), tx, f.sourceNetworkId, f.sourceId, f.destinationNetworkId, f.destinationId, f.sourceNetworkId, 121, nil)
			if err != nil {
				t.Fatal(err)
			}
		})
		server.RunPosts(f.ctx, posts...)
		free := f.contract(t)
		for _, id := range []server.Id{escrow.ContractId, free} {
			f.close(t, id)
		}
		receipts := providerWorkFixtureReceipts(t, f.ctx, escrow.ContractId, free)
		for _, id := range []server.Id{escrow.ContractId, free} {
			r := providerWorkFixtureReservation(t, receipts, id)
			if !r.Reservation.Complete || r.Reservation.Capacity != 121 {
				t.Fatal("funding changed original admission facts", r.Reservation)
			}
		}
		count := 0
		for _, r := range receipts {
			if r.Outcome != nil {
				count++
				if r.Outcome.SourceBytes != 121 || r.Outcome.DestinationBytes != 121 {
					t.Fatal("funding changed equal completed bytes")
				}
			}
		}
		if count != 2 {
			t.Fatal("paid/free original outcomes missing", count)
		}
	})
}

func TestProviderWorkSessionRedisAdmissionRetainsOriginalFence(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		f := newProviderWorkSessionFixture(t)
		addContractPayoutTestBalance(f.ctx, f.sourceNetworkId, 1000)
		escrow, err := CreateTransferEscrow(f.requestContext(t, nil), f.sourceNetworkId, f.sourceId, f.destinationNetworkId, f.destinationId, 121)
		if err != nil {
			t.Fatal(err)
		}
		var redisReserved bool
		server.Db(f.ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(f.ctx, `SELECT bool_and(redis_reserved) FROM transfer_escrow WHERE contract_id=$1`, escrow.ContractId).Scan(&redisReserved))
		})
		if !redisReserved {
			t.Fatal("fixture missed the actual Redis reservation path")
		}
		r := providerWorkFixtureReservation(t, providerWorkFixtureReceipts(t, f.ctx, escrow.ContractId), escrow.ContractId)
		if !r.Reservation.Complete || r.Reservation.SourceHead.Sequence != 2 || r.Reservation.DestinationHead.Sequence != 2 {
			t.Fatal("Redis path omitted the original shared fence", r.Reservation)
		}
	})
}

func TestProviderWorkSessionSourceOwnsOnlyItsExplicitPrivateKey(t *testing.T) {
	key := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{113}, ed25519.SeedSize))
	authority := protocol.ProviderWorkSourceAuthority{DomainHash: [32]byte{114}, SourceId: server.NewId().String(), Generation: server.NewId().String(), PublicKey: [32]byte(key.Public().(ed25519.PublicKey)), FromUnixMicro: 1, ThroughUnixMicro: 1000, MaxEndpointEvents: 16, MaxCohortMembers: 4, DirectoryPublicKeys: [][32]byte{}}
	source, err := NewProviderWorkSessionSource(authority, key)
	if err != nil {
		t.Fatal(err)
	}
	key[0] ^= 1
	if _, err := NewProviderWorkSessionSource(authority, key); err == nil {
		t.Fatal("malformed or borrowed key was accepted")
	}
	body := protocol.ProviderWorkSessionEvent{ClientId: server.NewId().String(), NetworkId: server.NewId().String(), Kind: "baseline", Sequence: 1, ObservedAtUnixMicro: 2}
	original, _, _, err := source.sign(t.Context(), protocol.ProviderWorkReceipt{Session: &body}, 2)
	if err != nil || protocol.VerifyProviderWorkReceiptAuthority(t.Context(), original, authority) != nil {
		t.Fatal("caller mutation changed the live signing owner", err)
	}
	if _, _, _, err := source.sign(t.Context(), protocol.ProviderWorkReceipt{Session: &body}, 1001); err == nil {
		t.Fatal("expired role minted a new original")
	}
	if _, err := NewProviderWorkSessionSource(authority, nil); err == nil {
		t.Fatal("missing independent key selected a fallback")
	}
}

func TestProviderWorkSessionObservedExtenderRetirementRestoresAbsence(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		f := newProviderWorkSessionFixture(t)
		extender := testExtenderCacheExtender(f.ctx, "original-admission.example", true, testExtenderCacheAddress("192.0.2.79", true))
		Testing_RefreshExtenderAddressCache()
		connectionId, _, _, _, err := ConnectNetworkClientWithIpFamily(f.ctx, f.sourceId, "192.0.2.79:14001", f.handlerId, 4)
		if err != nil {
			t.Fatal(err)
		}
		id := f.contract(t)
		receipts := providerWorkFixtureReceipts(t, f.ctx, id)
		if providerWorkFixtureReservation(t, receipts, id).Reservation.Complete {
			t.Fatal("unproved active extender ownership became complete attribution")
		}
		states := providerWorkFixtureEndpoint(t, f, receipts, f.sourceId)
		if len(states) != 3 || states[2].ActiveExtenders != 1 {
			t.Fatal("live source suppressed observed extender presence", states)
		}
		found := false
		for _, r := range receipts {
			if r.Session != nil && r.Session.ConnectionId == connectionId.String() {
				found = true
				if r.Session.ExtenderId != extender.ExtenderId.String() || r.Session.Extender != nil {
					t.Fatal("presence event invented an extender owner", r.Session)
				}
			}
		}
		if !found {
			t.Fatal("actual observed extender admission was left unsigned")
		}
		if err := DisconnectNetworkClient(f.ctx, connectionId); err != nil {
			t.Fatal(err)
		}
		later := f.contract(t)
		receipts = providerWorkFixtureReceipts(t, f.ctx, later)
		if !providerWorkFixtureReservation(t, receipts, later).Reservation.Complete {
			t.Fatal("exact signed retirement did not restore original absence")
		}
		states = providerWorkFixtureEndpoint(t, f, receipts, f.sourceId)
		if len(states) != 4 || states[3].ActiveExtenders != 0 || states[3].ActiveConnections != 1 {
			t.Fatal("original retirement lost the remaining direct connection", states)
		}
	})
}

func TestProviderWorkSessionReservationRollsBackWithItsContract(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		f := newProviderWorkSessionFixture(t)
		var id server.Id
		server.Db(f.ctx, func(conn server.PgConn) {
			tx, err := conn.BeginTx(f.ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(context.WithoutCancel(f.ctx))
			id, err = createContractNoEscrowInTx(f.requestContext(t, nil), tx, f.sourceNetworkId, f.sourceId, f.destinationNetworkId, f.destinationId, 121, true)
			if err != nil {
				t.Fatal(err)
			}
			var present bool
			server.Raise(tx.QueryRow(f.ctx, `SELECT EXISTS(SELECT 1 FROM provider_work_reservation_original WHERE contract_id=$1)`, id).Scan(&present))
			if !present {
				t.Fatal("original was deferred until after the admission transaction")
			}
			server.Raise(tx.Rollback(f.ctx))
		})
		originals, err := ListProviderWorkOriginals(f.ctx, []server.Id{id})
		if err != nil || len(originals) != 0 {
			t.Fatal("rolled-back reservation leaked an original", len(originals), err)
		}
	})
}

func TestProviderWorkSessionRepeatableReadCannotCertifyAnOldAdmissionHead(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		f := newProviderWorkSessionFixture(t)
		testExtenderCacheExtender(f.ctx, "snapshot-admission.example", true, testExtenderCacheAddress("192.0.2.81", true))
		Testing_RefreshExtenderAddressCache()
		server.Tx(f.ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(f.ctx, `UPDATE network_client SET auth_time=$2 WHERE client_id=$1`, f.sourceId, server.NowUtc()))
		})
		var observedErr error
		server.Db(f.ctx, func(conn server.PgConn) {
			tx, err := conn.BeginTx(f.ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead})
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(context.WithoutCancel(f.ctx))
			var originalSequence uint64
			server.Raise(tx.QueryRow(f.ctx, `SELECT sequence FROM provider_work_session_head WHERE client_id=$1`, f.sourceId).Scan(&originalSequence))
			if originalSequence != 2 {
				t.Fatal("fixture did not establish the prior snapshot")
			}
			// The actual admission commits after this transaction's snapshot.
			// Its fresh auth_time makes the throttled directory update a no-op.
			if _, _, _, _, err := ConnectNetworkClientWithIpFamily(f.ctx, f.sourceId, "192.0.2.81:15001", f.handlerId, 4); err != nil {
				t.Fatal(err)
			}
			server.HandleError(func() {
				_, err := createContractNoEscrowInTx(f.requestContext(t, nil), tx, f.sourceNetworkId, f.sourceId, f.destinationNetworkId, f.destinationId, 121, true)
				observedErr = err
			}, func(err error) { observedErr = err })
		})
		var pgErr *pgconn.PgError
		if !errors.As(observedErr, &pgErr) || pgErr.Code != "40001" {
			t.Fatal("old repeatable-read snapshot certified a stale original endpoint set", observedErr)
		}
		id := f.contract(t)
		r := providerWorkFixtureReservation(t, providerWorkFixtureReceipts(t, f.ctx, id), id)
		if r.Reservation.Complete || r.Reservation.SourceHead.Sequence != 3 {
			t.Fatal("public admission omitted the newly committed extender", r.Reservation)
		}
	})
}

func TestProviderWorkSessionLegacyWriterWaitsBeforeTakingConnectionRow(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		f := newProviderWorkSessionFixture(t)
		ctx, cancel := context.WithTimeout(f.ctx, 2*time.Minute)
		defer cancel()
		legacyCtx, cancelLegacy := context.WithCancel(ctx)
		defer cancelLegacy()
		ready := make(chan int, 1)
		done := make(chan error, 1)
		joined := make(chan struct{})
		started := false
		defer func() {
			cancelLegacy()
			if !started {
				return
			}
			select {
			case <-joined:
			case <-time.After(30 * time.Second):
				t.Error("legacy writer did not join canceled cleanup")
			}
		}()
		server.Db(ctx, func(conn server.PgConn) {
			tx, err := conn.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(context.WithoutCancel(ctx))
			if !providerWorkLockSessionMutationInTx(ctx, tx, f.sourceId) {
				t.Fatal("new admission owner did not acquire its ordered fences")
			}
			var ownerPid int
			server.Raise(tx.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&ownerPid))
			started = true
			go func() {
				defer close(joined)
				var resultErr error
				server.HandleError(func() {
					server.Db(legacyCtx, func(other server.PgConn) {
						legacy, err := other.BeginTx(legacyCtx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
						server.Raise(err)
						defer legacy.Rollback(context.WithoutCancel(legacyCtx))
						var pid int
						server.Raise(legacy.QueryRow(legacyCtx, `SELECT pg_backend_pid()`).Scan(&pid))
						ready <- pid
						_, resultErr = legacy.Exec(legacyCtx, `UPDATE network_client_connection SET connected=false,disconnect_time=$2 WHERE connection_id=$1`, f.sourceConnectionId, server.NowUtc())
						if resultErr == nil {
							resultErr = legacy.Commit(legacyCtx)
						}
					})
				}, func(err error) { resultErr = err })
				done <- resultErr
			}()
			var legacyPid int
			select {
			case legacyPid = <-ready:
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			for {
				select {
				case err := <-done:
					t.Fatal("legacy writer crossed the original reservation fence", err)
				default:
				}
				var blocked, bridge bool
				server.Raise(tx.QueryRow(ctx, `SELECT $2=ANY(pg_blocking_pids($1)),EXISTS(
				 SELECT 1 FROM pg_locks WHERE pid=$1 AND locktype='advisory' AND NOT granted
				 AND classid=4294967295::oid AND objid=4294966520::oid AND objsubid=1)`, legacyPid, ownerPid).Scan(&blocked, &bridge))
				if blocked {
					if !bridge {
						t.Fatal("legacy writer took the row before waiting for the endpoint; lock order inverted")
					}
					break
				}
				if err := ctx.Err(); err != nil {
					t.Fatal(err)
				}
			}
			server.RaisePgResult(tx.Exec(ctx, `UPDATE network_client_connection SET connected=false,disconnect_time=$2 WHERE connection_id=$1`, f.sourceConnectionId, server.NowUtc()))
			providerWorkRetainSessionEventsInTx(ctx, tx, f.sourceId)
			server.Raise(tx.Commit(ctx))
		})
		select {
		case err := <-done:
			if err != nil {
				t.Fatal("legacy traffic failed after the original owner committed", err)
			}
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
		id := f.contract(t)
		if !providerWorkFixtureReservation(t, providerWorkFixtureReceipts(t, f.ctx, id), id).Reservation.Complete {
			t.Fatal("ordered legacy no-op lost the original retirement")
		}
	})
}

func TestProviderWorkSessionStreamDirectoryRaceLeavesAttributionUnknown(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		f := newProviderWorkSessionFixture(t)
		ctx := f.requestContext(t, []server.Id{f.intermediaryId})
		id, err := CreateContractNoEscrow(ctx, f.sourceNetworkId, f.sourceId, f.destinationNetworkId, f.destinationId, 121)
		if err != nil {
			t.Fatal(err)
		}
		streamId := AddToStream(ctx, id, f.sourceId, f.destinationId, []server.Id{f.intermediaryId})
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE network_client SET network_id=$2 WHERE client_id=$1`, f.intermediaryId, server.NewId()))
		})
		if err := SetContractStream(ctx, id, streamId, []server.Id{f.intermediaryId}); err != nil {
			t.Fatal("optional provenance blocked ordinary stream traffic", err)
		}
		f.close(t, id)
		for _, r := range providerWorkFixtureReceipts(t, ctx, id) {
			if r.Outcome != nil {
				t.Fatal("raced directory was signed as the original stream owner")
			}
		}
	})
}

func TestProviderWorkSessionMissingOriginalRequestCannotMintOutcome(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		f := newProviderWorkSessionFixture(t)
		id, err := CreateContractNoEscrow(f.ctx, f.sourceNetworkId, f.sourceId, f.destinationNetworkId, f.destinationId, 121)
		if err != nil {
			t.Fatal(err)
		}
		f.close(t, id)
		receipts := providerWorkFixtureReceipts(t, f.ctx, id)
		if providerWorkFixtureReservation(t, receipts, id).Reservation.Complete {
			t.Fatal("absent original request became a complete source admission")
		}
		for _, r := range receipts {
			if r.Outcome != nil {
				t.Fatal("missing original request borrowed live settlement authority")
			}
		}
	})
}

func TestProviderWorkSessionOptionalStreamAcquireFailureKeepsTrafficOwner(t *testing.T) {
	key := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{123}, ed25519.SeedSize))
	authority := protocol.ProviderWorkSourceAuthority{DomainHash: [32]byte{124}, SourceId: server.NewId().String(), Generation: server.NewId().String(), PublicKey: [32]byte(key.Public().(ed25519.PublicKey)), FromUnixMicro: 1, ThroughUnixMicro: 1000, MaxEndpointEvents: 16, MaxCohortMembers: 4, DirectoryPublicKeys: [][32]byte{}}
	source, err := NewProviderWorkSessionSource(authority, key)
	if err != nil {
		t.Fatal(err)
	}
	ctx := WithProviderWorkRequestFrameHash(WithProviderWorkSessionSource(t.Context(), source), [32]byte{125})
	called := false
	cohort := providerWorkPrepareStreamWithDb(ctx, server.NewId(), server.NewId(), server.NewId(), []server.Id{server.NewId()}, func(context.Context, func(server.PgConn)) {
		called = true
		panic(errors.New("synthetic pool acquisition failed before callback"))
	})
	if !called || cohort != nil {
		t.Fatal("optional database acquisition escaped or minted a cohort")
	}
	authority.MaxCohortMembers = 0
	if _, err := NewProviderWorkSessionSource(authority, key); err != nil {
		t.Fatal("independently approved direct-only source was refused", err)
	}
}

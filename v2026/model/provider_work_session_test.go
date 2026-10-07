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
	"github.com/urfoundation/sn/v2026/protocol"
	"github.com/urnetwork/connect/v2026"
	coreprotocol "github.com/urnetwork/connect/v2026/protocol"
	"github.com/urnetwork/server/v2026"
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
	providerWorkSessionFundingOriginals(t, false)
}

func TestProviderWorkSessionRedisPaidAndFreeAdmissionsKeepEqualOriginalWork(t *testing.T) {
	providerWorkSessionFundingOriginals(t, true)
}

// The legacy admission owner acknowledges a durable intent before its worker
// settles. Redis admission settles on the close path. Both must retain the same
// original completed work for paid credit, a free grant and no-escrow traffic.
func providerWorkSessionFundingOriginals(t *testing.T, redis bool) {
	t.Helper()
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		for _, paid := range []bool{true, false} {
			f := newProviderWorkSessionFixture(t)
			if paid {
				addContractPayoutTestBalance(f.ctx, f.sourceNetworkId, 1000)
			} else {
				start, end := FreeGrantWindow(server.NowUtc())
				server.Tx(f.ctx, func(tx server.PgTx) {
					if err := AddGrantTransferBalanceInTx(tx, f.ctx, f.sourceNetworkId, GrantKindFree, 1000, start, end); err != nil {
						t.Fatal(err)
					}
				})
			}
			balances := GetActiveTransferBalances(f.ctx, f.sourceNetworkId)
			if len(balances) != 1 || balances[0].Paid != paid || paid && balances[0].GrantKind != GrantKindNone || !paid && balances[0].GrantKind != GrantKindFree {
				t.Fatal("fixture did not create the original paid or free grant", paid, balances)
			}
			balance := balances[0]
			var escrow *TransferEscrow
			if redis {
				var err error
				escrow, err = CreateTransferEscrow(f.requestContext(t, nil), f.sourceNetworkId, f.sourceId, f.destinationNetworkId, f.destinationId, 121)
				if err != nil {
					t.Fatal(err)
				}
			} else {
				var posts []func() any
				server.Tx(f.ctx, func(tx server.PgTx) {
					var err error
					escrow, posts, err = createTransferEscrowInTx(f.requestContext(t, nil), tx, f.sourceNetworkId, f.sourceId, f.destinationNetworkId, f.destinationId, f.sourceNetworkId, 121, nil)
					if err != nil {
						t.Fatal(err)
					}
				})
				server.RunPosts(f.ctx, posts...)
			}
			free := f.contract(t)
			contractIds := []server.Id{escrow.ContractId, free}
			before := providerWorkFixtureReceipts(t, f.ctx, contractIds...)
			reservationHashes := map[string][32]byte{}
			for _, id := range contractIds {
				r := providerWorkFixtureReservation(t, before, id)
				if !r.Reservation.Complete || r.Reservation.Capacity != 121 {
					t.Fatal("funding changed original admission facts", r.Reservation)
				}
				hash, err := r.ContentHash(f.ctx)
				if err != nil {
					t.Fatal(err)
				}
				reservationHashes[id.String()] = hash
				f.close(t, id)
			}
			if reservationHashes[escrow.ContractId.String()] == reservationHashes[free.String()] {
				t.Fatal("distinct admissions shared an original reservation")
			}
			server.Db(f.ctx, func(conn server.PgConn) {
				var redisReserved, pending, terminal, original, freeTerminal, freeOriginal bool
				server.Raise(conn.QueryRow(f.ctx, `SELECT
 (SELECT bool_and(redis_reserved) FROM transfer_escrow WHERE contract_id=$1),
 EXISTS(SELECT 1 FROM legacy_settlement_intent WHERE contract_id=$1),
 (SELECT outcome IS NOT NULL FROM transfer_contract WHERE contract_id=$1),
 EXISTS(SELECT 1 FROM provider_work_outcome_original WHERE contract_id=$1),
 (SELECT outcome='settled' FROM transfer_contract WHERE contract_id=$2),
 EXISTS(SELECT 1 FROM provider_work_outcome_original WHERE contract_id=$2)`,
					escrow.ContractId, free).Scan(&redisReserved, &pending, &terminal, &original, &freeTerminal, &freeOriginal))
				if redisReserved != redis || pending == redis || terminal != redis || original != redis {
					t.Fatal("funded close did not retain its actual settlement owner", redis, paid, redisReserved, pending, terminal, original)
				}
				if !freeTerminal || !freeOriginal {
					t.Fatal("no-escrow close did not retain its immediate original", redis, paid, freeTerminal, freeOriginal)
				}
			})
			// Drive the public worker under the same independently approved
			// source authority. No sleep or synthesized outcome can settle it.
			if !redis {
				if reserved := Testing_NetEscrowByteCount(f.ctx, balance.BalanceId); reserved != 121 {
					t.Fatal("pending settlement released its reservation", paid, reserved)
				}
				result, err := FlushLegacySettlements(f.ctx, int(escrow.ContractId[15])%LegacySettlementShardCount, nil, 64)
				if err != nil || result.Visited != 1 || result.Completed != 1 || result.BusyOrGone != 0 || result.Failed != 0 || result.More {
					t.Fatal("actual legacy worker did not settle the funded original", paid, result, err)
				}
			}
			receipts := providerWorkFixtureReceipts(t, f.ctx, contractIds...)
			outcomeIds := map[string]bool{}
			for _, r := range receipts {
				if err := protocol.VerifyProviderWorkReceiptAuthority(f.ctx, r, f.source.authority); err != nil {
					t.Fatal("settlement lost the independently signed original", err)
				}
				if r.Reservation != nil {
					hash, err := r.ContentHash(f.ctx)
					if err != nil || reservationHashes[r.Reservation.ContractId] != hash {
						t.Fatal("settlement rewrote the original reservation", err)
					}
				}
				if r.Outcome != nil {
					outcome := r.Outcome
					if outcomeIds[outcome.ContractId] || outcome.ReservationHash != reservationHashes[outcome.ContractId] || outcome.Capacity != 121 || outcome.SourceBytes != 121 || outcome.DestinationBytes != 121 || !outcome.SourceComplete || !outcome.DestinationComplete || outcome.Outcome != ContractOutcomeSettled {
						t.Fatal("funding changed equal completed original work", paid, outcome)
					}
					outcomeIds[outcome.ContractId] = true
				}
			}
			if len(outcomeIds) != 2 || !outcomeIds[escrow.ContractId.String()] || !outcomeIds[free.String()] {
				t.Fatal("paid/free original outcomes missing", len(outcomeIds))
			}
			originals, err := ListProviderWorkOriginals(f.ctx, contractIds)
			if err != nil {
				t.Fatal(err)
			}
			result, err := FlushLegacySettlements(f.ctx, int(escrow.ContractId[15])%LegacySettlementShardCount, nil, 64)
			if err != nil || result.Visited != 0 || result.Completed != 0 || result.BusyOrGone != 0 || result.Failed != 0 || result.More {
				t.Fatal("completed work was admitted to a second settlement", result, err)
			}
			after, err := ListProviderWorkOriginals(f.ctx, contractIds)
			if err != nil || !slices.EqualFunc(originals, after, bytes.Equal) {
				t.Fatal("idle worker changed retained original outcomes", err)
			}
			if !redis {
				server.Db(f.ctx, func(conn server.PgConn) {
					var pending, settled bool
					var remaining, consumed ByteCount
					server.Raise(conn.QueryRow(f.ctx, `SELECT
                EXISTS(SELECT 1 FROM legacy_settlement_intent WHERE contract_id=$1),
                e.settled,b.balance_byte_count,e.payout_byte_count
                FROM transfer_escrow e JOIN transfer_balance b USING(balance_id)
                WHERE e.contract_id=$1 AND b.balance_id=$2`, escrow.ContractId, balance.BalanceId).Scan(&pending, &settled, &remaining, &consumed))
					if pending || !settled || remaining != 879 || consumed != 121 {
						t.Fatalf("settlement replay changed finances: paid=%t pending=%t settled=%t remaining=%d consumed=%d", paid, pending, settled, remaining, consumed)
					}
				})
				if reserved := Testing_NetEscrowByteCount(f.ctx, balance.BalanceId); reserved != 0 {
					t.Fatal("completed settlement kept its reservation", paid, reserved)
				}
			}
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

// A legacy row owner refuses a conflicting endpoint immediately, so the
// cooperating owner can retire and sign before the legacy no-op is retried.
func TestProviderWorkSessionLegacyWriterRetriesWithoutHoldingConnectionRow(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		f := newProviderWorkSessionFixture(t)
		ctx, cancel := context.WithTimeout(f.ctx, 30*time.Second)
		defer cancel()
		server.Db(ctx, func(conn server.PgConn) {
			tx, err := conn.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
			if err != nil {
				t.Fatal(err)
			}
			defer rollbackCloseReportTestTransaction(ctx, tx)
			if !providerWorkLockSessionMutationInTx(ctx, tx, f.sourceId) {
				t.Fatal("new admission owner did not acquire its ordered fences")
			}
			server.Db(ctx, func(other server.PgConn) {
				legacy, err := other.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
				server.Raise(err)
				defer rollbackCloseReportTestTransaction(ctx, legacy)
				server.RaisePgResult(legacy.Exec(ctx, `SET LOCAL lock_timeout='2s'`))
				_, err = legacy.Exec(ctx, `UPDATE network_client_connection SET connected=false,disconnect_time=$2 WHERE connection_id=$1`, f.sourceConnectionId, server.NowUtc())
				var pgErr *pgconn.PgError
				if !errors.As(err, &pgErr) || pgErr.Code != "40001" {
					t.Fatal("legacy writer did not release its conflicting row for retry", err)
				}
				server.Raise(legacy.Rollback(ctx))
			})
			server.RaisePgResult(tx.Exec(ctx, `UPDATE network_client_connection SET connected=false,disconnect_time=$2 WHERE connection_id=$1`, f.sourceConnectionId, server.NowUtc()))
			providerWorkRetainSessionEventsInTx(ctx, tx, f.sourceId)
			server.Raise(tx.Commit(ctx))
		})
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE network_client_connection SET connected=false,disconnect_time=$2 WHERE connection_id=$1`, f.sourceConnectionId, server.NowUtc()))
		})
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

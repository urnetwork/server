package model

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/certsservice"
	"github.com/urnetwork/sidecar/sidecartest"

	"github.com/urnetwork/server"
)

// certs' Postgres store on a private database (TLS-M5-16): it keeps what the
// engine's suite asks of every CertsStore, the fake's included (D-TEST-2), a
// custom domain's chain reads back, and a binary without MG5 touches nothing.

func TestCertsStorePassesTheSuite(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		if !server.SidecarMg5Linked() {
			return
		}
		sidecartest.RunCertsStoreSuite(t, NewCertsStore(nil), &sidecartest.CertsStoreSuiteHooks{
			MarkEphemeral: func(ctx context.Context, clientId sidecar.Id) error {
				server.Tx(ctx, func(tx server.PgTx) {
					server.RaisePgResult(tx.Exec(
						ctx,
						`INSERT INTO network_client_owner (client_id, ephemeral) VALUES ($1, true)`,
						server.Id(clientId),
					))
				})
				return nil
			},
		})
	})
}

func TestCertsStoreKeepsAChainAndEmptyColumns(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		if !server.SidecarMg5Linked() {
			return
		}
		ctx := context.Background()
		store := NewCertsStore(nil)
		orderTime := time.Date(2031, time.May, 4, 3, 2, 1, 0, time.UTC)
		row := &sidecar.TlsCertRecord{
			CertId:              sidecartest.CertsIdAt(orderTime),
			NetworkId:           sidecar.NewId(),
			ClientId:            sidecar.NewId(),
			SidecarId:           sidecar.NewId(),
			IdentifierSetSha256: make([]byte, 32),
			SpkiSha256:          make([]byte, 32),
			Attestation:         []byte("attestation"),
			State:               sidecar.TlsCertStateIssued,
			StateTime:           orderTime.Add(time.Minute),
			// a reason named on a row that is not revoking is not kept
			RevocationReason: sidecar.TlsCertRevocationReasonKeyCompromise,
			Serial:           "0a1b",
			NotBefore:        orderTime,
			NotAfter:         orderTime.Add(90 * 24 * time.Hour),
			Chain:            [][]byte{[]byte("leaf"), []byte("intermediate")},
		}
		copy(row.IdentifierSetSha256, row.CertId[:])
		if err := store.InsertCert(ctx, row); err != nil {
			t.Fatal(err)
		}
		stored, err := store.GetCert(ctx, row.CertId)
		if err != nil {
			t.Fatal(err)
		}
		if len(stored.Chain) != 2 || string(stored.Chain[0]) != "leaf" || string(stored.Chain[1]) != "intermediate" {
			t.Fatalf("the chain read back as %q", stored.Chain)
		}
		if stored.Csr != nil || stored.Ca != "" || stored.AccountUri != "" || stored.Profile != "" ||
			!stored.AriWindowStart.IsZero() || !stored.AriWindowEnd.IsZero() || stored.SuccessorCertId != (sidecar.Id{}) {
			t.Fatalf("an unset column read back set: %+v", stored)
		}
		if !stored.NotBefore.Equal(row.NotBefore) || !stored.NotAfter.Equal(row.NotAfter) || stored.NotBefore.Location() != time.UTC {
			t.Fatalf("the validity read back as %v to %v", stored.NotBefore, stored.NotAfter)
		}
		if !stored.StateTime.Equal(row.StateTime) || stored.StateTime.Location() != time.UTC || stored.RevocationReason != "" {
			t.Fatalf("the state's time read back as %v with reason %q", stored.StateTime, stored.RevocationReason)
		}
		// a row with no chain keeps none, not an empty one
		chainless := *row
		chainless.CertId = sidecartest.CertsIdAt(orderTime.Add(time.Second))
		chainless.Chain = [][]byte{}
		if err := store.InsertCert(ctx, &chainless); err != nil {
			t.Fatal(err)
		}
		if stored, err := store.GetCert(ctx, chainless.CertId); err != nil || stored.Chain != nil {
			t.Fatalf("a row with no chain read back %v, %v", stored, err)
		}

		// a set's orders since a time between two milliseconds start at the next one
		between := orderTime.Add(500 * time.Microsecond)
		if rows, err := store.ListSetOrders(ctx, row.IdentifierSetSha256, "", orderTime); err != nil || len(rows) != 0 {
			t.Fatalf("a row at no CA listed under the empty CA: %v, %v", rows, err)
		}
		if applied, err := store.TransitionCert(ctx, &certsservice.CertsTransition{
			CertId: row.CertId,
			From:   []sidecar.TlsCertState{sidecar.TlsCertStateIssued},
			To:     sidecar.TlsCertStateIssued,
			At:     orderTime.Add(time.Hour),
			Ca:     "le",
		}); err != nil || !applied {
			t.Fatalf("the transition: %v, %v", applied, err)
		}
		if rows, err := store.ListSetOrders(ctx, row.IdentifierSetSha256, "le", orderTime); err != nil || len(rows) != 1 {
			t.Fatalf("the set's orders at its order time: %v, %v", rows, err)
		}
		if rows, err := store.ListSetOrders(ctx, row.IdentifierSetSha256, "le", between); err != nil || len(rows) != 0 {
			t.Fatalf("the set's orders after its order time: %v, %v", rows, err)
		}

		// a transition with no states is refused, not applied to every row
		if _, err := store.TransitionCert(ctx, &certsservice.CertsTransition{CertId: row.CertId, To: sidecar.TlsCertStateRevoked, At: orderTime.Add(time.Hour)}); err == nil {
			t.Fatal("a transition from no state was taken")
		}
		if stored, err := store.GetCert(ctx, row.CertId); err != nil || stored.State != sidecar.TlsCertStateIssued {
			t.Fatalf("the row after a refused transition: %+v, %v", stored, err)
		}
	})
}

func TestRequestTlsCertRevocationMarksIssuedRowsOfItsNetwork(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		if !server.SidecarMg5Linked() {
			return
		}
		ctx := context.Background()
		store := NewCertsStore(nil)
		networkId := sidecar.NewId()
		otherNetworkId := sidecar.NewId()
		orderTime := time.Date(2033, time.May, 4, 3, 2, 1, 0, time.UTC)
		insert := func(rowNetworkId sidecar.Id, state sidecar.TlsCertState) sidecar.Id {
			t.Helper()
			row := &sidecar.TlsCertRecord{
				CertId:              sidecartest.CertsIdAt(orderTime),
				NetworkId:           rowNetworkId,
				ClientId:            sidecar.NewId(),
				SidecarId:           sidecar.NewId(),
				IdentifierSetSha256: make([]byte, 32),
				SpkiSha256:          make([]byte, 32),
				Attestation:         []byte("attestation"),
				State:               state,
				StateTime:           orderTime,
			}
			if err := store.InsertCert(ctx, row); err != nil {
				t.Fatal(err)
			}
			return row.CertId
		}
		// a marked row holds its reason and the time of the mark
		marked := func(certId sidecar.Id, reason sidecar.TlsCertRevocationReason, since time.Time) {
			t.Helper()
			row, err := store.GetCert(ctx, certId)
			if err != nil {
				t.Fatal(err)
			}
			if row.State != sidecar.TlsCertStateRevoking || row.RevocationReason != reason || row.StateTime.Before(since.Truncate(time.Microsecond)) {
				t.Fatalf("a marked row: state %s, reason %q, state time %v, marked since %v", row.State, row.RevocationReason, row.StateTime, since)
			}
		}
		since := server.NowUtc()
		stateOf := func(certId sidecar.Id) sidecar.TlsCertState {
			t.Helper()
			row, err := store.GetCert(ctx, certId)
			if err != nil {
				t.Fatal(err)
			}
			return row.State
		}
		first := insert(networkId, sidecar.TlsCertStateIssued)
		second := insert(networkId, sidecar.TlsCertStateIssued)
		queued := insert(networkId, sidecar.TlsCertStateQueued)
		expired := insert(networkId, sidecar.TlsCertStateExpired)
		other := insert(otherNetworkId, sidecar.TlsCertStateIssued)

		// another network's certificate is not this network's to revoke
		if count, err := RequestTlsCertRevocation(ctx, &TlsCertRevocationRequest{NetworkId: server.Id(networkId), CertId: server.Id(other), Reason: "abuse:case"}); err != nil || count != 0 {
			t.Fatalf("another network's certificate: %d, %v", count, err)
		}
		if count, err := RequestTlsCertRevocation(ctx, &TlsCertRevocationRequest{NetworkId: server.Id(networkId), CertId: server.Id(first), Reason: "abuse:case"}); err != nil || count != 1 {
			t.Fatalf("one certificate: %d, %v", count, err)
		}
		if stateOf(first) != sidecar.TlsCertStateRevoking || stateOf(second) != sidecar.TlsCertStateIssued {
			t.Fatalf("after one: %s, %s", stateOf(first), stateOf(second))
		}
		// §21.6's takedown: the names stop being served
		marked(first, sidecar.TlsCertRevocationReasonCessationOfOperation, since)
		// the whole network: what is still issued, once
		for _, want := range []int{1, 0} {
			if count, err := RequestTlsCertRevocation(ctx, &TlsCertRevocationRequest{NetworkId: server.Id(networkId), Reason: "abuse:case"}); err != nil || count != want {
				t.Fatalf("the network: %d, %v, want %d", count, err, want)
			}
		}
		if stateOf(second) != sidecar.TlsCertStateRevoking || stateOf(queued) != sidecar.TlsCertStateQueued ||
			stateOf(expired) != sidecar.TlsCertStateExpired || stateOf(other) != sidecar.TlsCertStateIssued {
			t.Fatal("a row that was not the network's issued certificate moved")
		}
		marked(second, sidecar.TlsCertRevocationReasonCessationOfOperation, since)
		if row, err := store.GetCert(ctx, queued); err != nil || row.RevocationReason != "" || !row.StateTime.Equal(orderTime) {
			t.Fatalf("a row the request did not mark: %+v, %v", row, err)
		}
		if _, err := RequestTlsCertRevocation(ctx, &TlsCertRevocationRequest{CertId: server.Id(first)}); err == nil {
			t.Fatal("a revocation with no network was taken")
		}
		// a compromise: the CA is to be told keyCompromise
		compromised := insert(networkId, sidecar.TlsCertStateIssued)
		if count, err := RequestTlsCertRevocation(ctx, &TlsCertRevocationRequest{NetworkId: server.Id(networkId), CertId: server.Id(compromised), Reason: "compromise", KeyCompromise: true}); err != nil || count != 1 {
			t.Fatalf("a compromised certificate: %d, %v", count, err)
		}
		marked(compromised, sidecar.TlsCertRevocationReasonKeyCompromise, since)
	})
}

func TestCertsStoreUnlinkedTouchesNothing(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		if server.SidecarMg5Linked() {
			return
		}
		ctx := context.Background()
		store := NewCertsStore(nil)
		row := &sidecar.TlsCertRecord{CertId: sidecar.NewId(), NetworkId: sidecar.NewId(), State: sidecar.TlsCertStateQueued, StateTime: server.NowUtc()}
		if err := store.InsertCert(ctx, row); !errors.Is(err, ErrCertsStoreUnavailable) {
			t.Fatalf("an insert without MG5 answered %v", err)
		}
		if _, err := store.GetCert(ctx, row.CertId); !errors.Is(err, ErrCertsStoreUnavailable) {
			t.Fatalf("a read without MG5 answered %v", err)
		}
		if _, err := RequestTlsCertRevocation(ctx, &TlsCertRevocationRequest{NetworkId: server.Id(row.NetworkId)}); !errors.Is(err, ErrCertsStoreUnavailable) {
			t.Fatalf("a revocation without MG5 answered %v", err)
		}
		if _, err := store.PurgeEnded(ctx, server.NowUtc()); !errors.Is(err, ErrCertsStoreUnavailable) {
			t.Fatalf("a purge without MG5 answered %v", err)
		}
		if _, err := store.PurgeChains(ctx, server.NowUtc()); !errors.Is(err, ErrCertsStoreUnavailable) {
			t.Fatalf("a chain purge without MG5 answered %v", err)
		}
	})
}

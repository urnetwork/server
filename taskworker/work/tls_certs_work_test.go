// certs' purge chain on a private database (TLS-M5-17): scheduled only in a
// binary that links MG5, and each run deleting the rows whose certificate
// ended more than 30 days ago, and only those.
package work

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/certsservice"
	"github.com/urnetwork/sidecar/sidecartest"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
	"github.com/urnetwork/server/task"
)

func TestTlsCertPurgeScheduledOnlyWithMg5(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		clientSession := session.NewLocalClientSession(ctx, "", nil)
		defer clientSession.Cancel()
		for range 2 {
			server.Tx(ctx, func(tx server.PgTx) {
				ScheduleTlsCertPurge(clientSession, tx)
			})
		}
		want := 0
		if server.SidecarMg5Linked() {
			want = 1
		}
		var count int
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(
				ctx,
				`SELECT count(*) FROM pending_task WHERE run_once_key = $1`,
				task.RunOnce("tls_cert_purge").String(),
			).Scan(&count))
		})
		if count != want {
			t.Fatalf("%d runs queued, want %d", count, want)
		}
		if !server.SidecarMg5Linked() {
			result, err := TlsCertPurge(&TlsCertPurgeArgs{}, clientSession)
			if err != nil || result.RemovedCount != 0 || result.More {
				t.Fatalf("a run without MG5: %+v, %v", result, err)
			}
		}
	})
}

func TestTlsCertPurgeRemovesRowsPastTheirRetention(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		if !server.SidecarMg5Linked() {
			return
		}
		ctx := context.Background()
		clientSession := session.NewLocalClientSession(ctx, "", nil)
		defer clientSession.Cancel()
		store := model.NewCertsStore(nil)
		// a time of this test's own, so the store's other rows are not in the run
		now := time.Date(2051, time.March, 1, 0, 0, 0, 0, time.UTC)
		retention := DefaultTlsCertPurgeSettings().RowRetention
		if retention != 30*24*time.Hour {
			t.Fatalf("the retention is %v", retention)
		}
		insert := func(state sidecar.TlsCertState, notAfter time.Time) sidecar.Id {
			t.Helper()
			row := &sidecar.TlsCertRecord{
				CertId:              sidecartest.CertsIdAt(now.Add(-100 * 24 * time.Hour)),
				NetworkId:           sidecar.NewId(),
				ClientId:            sidecar.NewId(),
				SidecarId:           sidecar.NewId(),
				IdentifierSetSha256: make([]byte, 32),
				SpkiSha256:          make([]byte, 32),
				Attestation:         []byte("attestation"),
				State:               state,
			}
			if !notAfter.IsZero() {
				row.Serial = "0a"
				row.NotBefore = notAfter.Add(-45 * 24 * time.Hour)
				row.NotAfter = notAfter
			}
			if err := store.InsertCert(ctx, row); err != nil {
				t.Fatal(err)
			}
			return row.CertId
		}
		ended := insert(sidecar.TlsCertStateExpired, now.Add(-retention-time.Second))
		revoked := insert(sidecar.TlsCertStateRevoked, now.Add(-retention-time.Hour))
		atTheLine := insert(sidecar.TlsCertStateExpired, now.Add(-retention))
		recent := insert(sidecar.TlsCertStateExpired, now.Add(-time.Hour))
		live := insert(sidecar.TlsCertStateIssued, now.Add(time.Hour))
		failed := insert(sidecar.TlsCertStateFailed, time.Time{})

		result, err := tlsCertPurgeAt(clientSession, now)
		if err != nil || result.More || result.RemovedCount < 2 {
			t.Fatalf("the purge: %+v, %v", result, err)
		}
		for _, certId := range []sidecar.Id{ended, revoked} {
			if _, err := store.GetCert(ctx, certId); !errors.Is(err, certsservice.ErrCertsNotFound) {
				t.Fatalf("a row past its retention stayed: %v", err)
			}
		}
		for _, certId := range []sidecar.Id{atTheLine, recent, live, failed} {
			if _, err := store.GetCert(ctx, certId); err != nil {
				t.Fatalf("a row inside its retention went: %v", err)
			}
		}
	})
}

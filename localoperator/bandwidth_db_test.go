// The in-process operator's bandwidth budget against the database.
package localoperator

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/qualityprobe/bandwidth"
)

// The quota rows one client holds.
func bandwidthTestQuotaRows(ctx context.Context, clientId server.Id) (count int) {
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(ctx, `SELECT count(*) FROM provider_bandwidth_quota WHERE client_id = $1`, clientId).Scan(&count))
	})
	return
}

// The bandwidth probe measures now, over a tunnel it already opened, so it
// has no use for budget in a later hour. With operator_transport local, a
// full current hour must read as bandwidth.ErrNoBudget, the clean skip that
// stops the pass, exactly as the API's 429 does over HTTP, and the deferred
// reservation the model made in a later hour must be given back. With room,
// the reservation is the current hour's.
func TestReserveBandwidthReadsAFullHourAsTheCleanSkip(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		operator, err := New(0, 1, 30*time.Second, 1)
		if err != nil {
			t.Fatal(err)
		}
		bucketStart := server.NowUtc().UTC().Truncate(model.ProviderBandwidthBucketDuration)
		fill := server.NewId()
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx,
				`INSERT INTO provider_bandwidth_quota (provider_bandwidth_quota_id, client_id, byte_count, bucket_start, create_time) VALUES ($1, $2, $3, $4, $5)`,
				server.NewId(), fill, model.MaxProviderBandwidthBytesPerBucket(), bucketStart, server.NowUtc(),
			))
		})

		refused := server.NewId()
		err = operator.ReserveBandwidth(ctx, refused.String(), model.MaxProviderBandwidthBytesPerProbe)
		if !errors.Is(err, bandwidth.ErrNoBudget) {
			t.Fatalf("a full hour reserved %v, want bandwidth.ErrNoBudget", err)
		}
		if rows := bandwidthTestQuotaRows(ctx, refused); rows != 0 {
			t.Fatalf("a refused reservation left %d quota rows, want the deferred one given back", rows)
		}

		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `DELETE FROM provider_bandwidth_quota WHERE client_id = $1`, fill))
		})
		admitted := server.NewId()
		if err := operator.ReserveBandwidth(ctx, admitted.String(), model.MaxProviderBandwidthBytesPerProbe); err != nil {
			t.Fatalf("an hour with room refused the reservation: %v", err)
		}
		var reservedIn time.Time
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT bucket_start FROM provider_bandwidth_quota WHERE client_id = $1`, admitted).Scan(&reservedIn))
		})
		if !reservedIn.Equal(bucketStart) {
			t.Fatalf("the reservation landed in the bucket of %s, want the current hour %s", reservedIn, bucketStart)
		}
	})
}

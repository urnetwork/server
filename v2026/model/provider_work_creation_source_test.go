// Creation resolves its optional source before owning pg, and one operation
// retains the same source or absence through aliases and transaction reruns.
package model

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/urfoundation/sn/v2026/protocol"
	"github.com/urnetwork/server/v2026"
)

// Public aliases and direct transaction owners must share the same boundary.
type providerWorkCreationSourceEntry struct {
	name   string
	create func(context.Context) error
}

// Generated identities are sufficient because the pg guard stops before any
// database, Redis, configuration, or client fixture can be acquired.
func providerWorkCreationSourceEntries() []providerWorkCreationSourceEntry {
	sourceNetworkId, sourceId := server.NewId(), server.NewId()
	destinationNetworkId, destinationId := server.NewId(), server.NewId()
	return []providerWorkCreationSourceEntry{
		{name: "contract", create: func(ctx context.Context) error {
			_, _, err := CreateContract(ctx, sourceNetworkId, sourceId, destinationNetworkId, destinationId, 1)
			return err
		}},
		{name: "transfer", create: func(ctx context.Context) error {
			_, err := CreateTransferEscrow(ctx, sourceNetworkId, sourceId, destinationNetworkId, destinationId, 1)
			return err
		}},
		{name: "direct_transfer", create: func(ctx context.Context) error {
			_, err := createTransferEscrow(ctx, sourceNetworkId, sourceId, destinationNetworkId, destinationId, 1)
			return err
		}},
		{name: "companion_contract", create: func(ctx context.Context) error {
			_, _, err := CreateCompanionContract(ctx, sourceNetworkId, sourceId, destinationNetworkId, destinationId, 1, time.Minute)
			return err
		}},
		{name: "companion_transfer", create: func(ctx context.Context) error {
			_, err := CreateCompanionTransferEscrow(ctx, sourceNetworkId, sourceId, destinationNetworkId, destinationId, 1, time.Minute)
			return err
		}},
		{name: "direct_companion", create: func(ctx context.Context) error {
			_, err := createCompanionTransferEscrow(ctx, sourceNetworkId, sourceId, destinationNetworkId, destinationId, 1, time.Minute)
			return err
		}},
		{name: "no_escrow", create: func(ctx context.Context) error {
			_, err := CreateContractNoEscrow(ctx, sourceNetworkId, sourceId, destinationNetworkId, destinationId, 1)
			return err
		}},
		{name: "no_escrow_usage", create: func(ctx context.Context) error {
			_, err := CreateContractNoEscrowWithUsageOrigin(ctx, sourceNetworkId, sourceId, destinationNetworkId, destinationId, 1, false)
			return err
		}},
		{name: "no_escrow_expiration", create: func(ctx context.Context) error {
			_, _, err := CreateContractNoEscrowWithExpiration(ctx, sourceNetworkId, sourceId, destinationNetworkId, destinationId, 1, true)
			return err
		}},
		{name: "zero_transfer", create: func(ctx context.Context) error {
			_, err := CreateTransferEscrow(ctx, sourceNetworkId, sourceId, destinationNetworkId, destinationId, 0)
			return err
		}},
		{name: "zero_companion", create: func(ctx context.Context) error {
			_, err := CreateCompanionTransferEscrow(ctx, sourceNetworkId, sourceId, destinationNetworkId, destinationId, 0, time.Minute)
			return err
		}},
		{name: "zero_no_escrow", create: func(ctx context.Context) error {
			_, err := CreateContractNoEscrow(ctx, sourceNetworkId, sourceId, destinationNetworkId, destinationId, 0)
			return err
		}},
	}
}

// Preserve the exact raised sentinel without logging expected guard failures.
func providerWorkCreationSourceAttempt(ctx context.Context, entry providerWorkCreationSourceEntry) (result any) {
	defer func() {
		if raised := recover(); raised != nil {
			result = raised
		}
	}()
	return entry.create(ctx)
}

// The loader barrier precedes the shared acquisition guard. With the old
// placement, the actual entry reaches pg and returns before loading can start.
func TestProviderWorkCreationSourceLoadsBeforePostgres(t *testing.T) {
	for _, entry := range providerWorkCreationSourceEntries() {
		func() {
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			ctx = server.WithoutPostgres(ctx)
			started := make(chan struct{})
			release := make(chan struct{})
			done := make(chan struct{})
			loads := 0
			ctx = context.WithValue(ctx, providerWorkSessionSourceLoaderContextKey{}, func() (*ProviderWorkSessionSource, error) {
				loads++
				if loads == 1 {
					close(started)
				}
				select {
				case <-release:
					return nil, os.ErrNotExist
				case <-ctx.Done():
					return nil, ctx.Err()
				}
			})
			var result any
			go func() {
				defer close(done)
				result = providerWorkCreationSourceAttempt(ctx, entry)
			}()
			defer func() {
				cancel()
				select {
				case <-done:
				case <-time.After(10 * time.Second):
					t.Errorf("%s creation owner did not join", entry.name)
				}
			}()
			select {
			case <-started:
			case <-done:
				t.Fatalf("%s acquired pg before entering the loader: %v", entry.name, result)
			case <-ctx.Done():
				t.Fatalf("%s never reached the loader barrier", entry.name)
			}
			if attempts := server.PacketPostgresAttempts(ctx); attempts != 0 {
				t.Fatalf("%s attempted pg while the loader was blocked: %d", entry.name, attempts)
			}
			close(release)
			select {
			case <-done:
			case <-ctx.Done():
				t.Fatalf("%s did not reach pg after loading completed", entry.name)
			}
			raised, ok := result.(error)
			if !ok || !errors.Is(raised, server.ErrPacketPostgres) || server.PacketPostgresAttempts(ctx) != 1 || loads != 1 {
				t.Fatalf("%s changed the acquisition boundary after loading: result=%v attempts=%d loads=%d", entry.name, result, server.PacketPostgresAttempts(ctx), loads)
			}
		}()
	}
}

// An explicit source and an explicit typed nil both bypass the optional loader
// at every public alias and direct transaction owner.
func TestProviderWorkCreationSourceHonorsPinnedSourceAndAbsence(t *testing.T) {
	source, err := providerWorkSessionSourceFromBytes(providerWorkInspectionBytes(t, newProviderWorkInspectionConfig()))
	if err != nil {
		t.Fatal(err)
	}
	defer clear(source.key)
	for _, pinned := range []*ProviderWorkSessionSource{source, nil} {
		for _, entry := range providerWorkCreationSourceEntries() {
			ctx := server.WithoutPostgres(WithProviderWorkSessionSource(t.Context(), pinned))
			loads := 0
			ctx = context.WithValue(ctx, providerWorkSessionSourceLoaderContextKey{}, func() (*ProviderWorkSessionSource, error) {
				loads++
				return nil, os.ErrPermission
			})
			result := providerWorkCreationSourceAttempt(ctx, entry)
			raised, ok := result.(error)
			if !ok || !errors.Is(raised, server.ErrPacketPostgres) || server.PacketPostgresAttempts(ctx) != 1 || loads != 0 {
				t.Fatalf("%s reloaded a pinned source (absent=%t): result=%v attempts=%d loads=%d", entry.name, pinned == nil, result, server.PacketPostgresAttempts(ctx), loads)
			}
		}
	}
}

// Optional read and validation failures stay pinned as absence while creation
// continues to its ordinary pg boundary. Valid decoding is also performed once.
func TestProviderWorkCreationSourcePinsLoaderOutcome(t *testing.T) {
	valid := providerWorkInspectionBytes(t, newProviderWorkInspectionConfig())
	wrongKey := newProviderWorkInspectionConfig()
	wrongKey.PrivateKey = nil
	invalid := providerWorkInspectionBytes(t, wrongKey)
	for _, sourceCase := range []struct {
		name string
		raw  []byte
		err  error
	}{
		{name: "valid", raw: valid},
		{name: "missing", err: os.ErrNotExist},
		{name: "unreadable", err: os.ErrPermission},
		{name: "empty"},
		{name: "malformed", raw: []byte("{")},
		{name: "invalid_key", raw: invalid},
	} {
		for _, entry := range providerWorkCreationSourceEntries() {
			ctx := server.WithoutPostgres(t.Context())
			loads := 0
			ctx = context.WithValue(ctx, providerWorkSessionSourceLoaderContextKey{}, func() (*ProviderWorkSessionSource, error) {
				loads++
				if sourceCase.err != nil {
					return nil, sourceCase.err
				}
				return providerWorkSessionSourceFromBytes(sourceCase.raw)
			})
			result := providerWorkCreationSourceAttempt(ctx, entry)
			raised, ok := result.(error)
			if !ok || !errors.Is(raised, server.ErrPacketPostgres) || server.PacketPostgresAttempts(ctx) != 1 || loads != 1 {
				t.Fatalf("%s/%s did not pin optional loading: result=%v attempts=%d loads=%d", entry.name, sourceCase.name, result, server.PacketPostgresAttempts(ctx), loads)
			}
		}
	}
}

// A sequence-backed deferred trigger rolls back the first real creation commit.
// The public retry must preserve the source selected before either transaction,
// including explicit nil and a loader failure followed by an available source.
// A controlled database event clock crosses the pinned source's expiry exactly
// between attempts, proving signing still checks the successful attempt's time.
func TestProviderWorkCreationSourcePreservesAuthorityAcrossCommitRetry(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		f := newProviderWorkSessionFixture(t)
		server.Tx(f.ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(f.ctx, `CREATE SEQUENCE provider_work_source_retry_test_attempt`))
			server.RaisePgResult(tx.Exec(f.ctx, `CREATE TABLE provider_work_source_retry_test_clock (create_time timestamp without time zone NOT NULL)`))
			server.RaisePgResult(tx.Exec(f.ctx, `CREATE FUNCTION provider_work_source_retry_test_time()
 RETURNS trigger LANGUAGE plpgsql AS $source$
 DECLARE controlled_time timestamp without time zone;
 BEGIN
  SELECT create_time INTO controlled_time FROM provider_work_source_retry_test_clock;
  IF controlled_time IS NOT NULL THEN
   NEW.create_time := controlled_time;
   NEW.expiration_time := controlled_time + interval '60 minutes';
  END IF;
  RETURN NEW;
 END
 $source$`))
			server.RaisePgResult(tx.Exec(f.ctx, `CREATE TRIGGER provider_work_source_retry_test_time
 BEFORE INSERT ON transfer_contract FOR EACH ROW EXECUTE FUNCTION provider_work_source_retry_test_time()`))
			server.RaisePgResult(tx.Exec(f.ctx, `CREATE FUNCTION provider_work_source_retry_test_rollback()
 RETURNS trigger LANGUAGE plpgsql AS $source$
 BEGIN
  IF nextval('provider_work_source_retry_test_attempt')=1 THEN
   IF EXISTS (SELECT 1 FROM provider_work_source_retry_test_clock)
    AND NOT EXISTS (SELECT 1 FROM provider_work_reservation_original WHERE contract_id=NEW.contract_id) THEN
    RAISE EXCEPTION USING ERRCODE='P0001',MESSAGE='synthetic unexpired source did not sign';
   END IF;
   RAISE EXCEPTION USING ERRCODE='40001',MESSAGE='synthetic source commit retry';
  END IF;
  RETURN NEW;
 END
 $source$`))
			server.RaisePgResult(tx.Exec(f.ctx, `CREATE CONSTRAINT TRIGGER provider_work_source_retry_test_rollback
 AFTER INSERT ON transfer_contract DEFERRABLE INITIALLY DEFERRED
 FOR EACH ROW EXECUTE FUNCTION provider_work_source_retry_test_rollback()`))
		})
		requestHash, ok := f.requestContext(t, nil).Value(providerWorkRequestFrameContextKey{}).([32]byte)
		if !ok || requestHash == ([32]byte{}) {
			t.Fatal("synthetic original request hash missing")
		}
		for _, sourceCase := range []struct {
			name           string
			pinned         bool
			available      bool
			expiresOnRetry bool
		}{
			{name: "pinned_source", pinned: true, available: true},
			{name: "pinned_absence", pinned: true},
			{name: "loaded_source", available: true},
			{name: "unavailable_source"},
			{name: "expires_during_retry", available: true, expiresOnRetry: true},
		} {
			server.Tx(f.ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(f.ctx, `ALTER SEQUENCE provider_work_source_retry_test_attempt RESTART WITH 1`))
				if sourceCase.expiresOnRetry {
					server.RaisePgResult(tx.Exec(f.ctx, `INSERT INTO provider_work_source_retry_test_clock(create_time) VALUES($1)`, time.UnixMicro(f.source.authority.ThroughUnixMicro-1).UTC()))
				}
			})
			ctx := WithProviderWorkRequestFrameHash(t.Context(), requestHash)
			if sourceCase.pinned {
				var source *ProviderWorkSessionSource
				if sourceCase.available {
					source = f.source
				}
				ctx = WithProviderWorkSessionSource(ctx, source)
			}
			loads := 0
			ctx = context.WithValue(ctx, providerWorkSessionSourceLoaderContextKey{}, func() (*ProviderWorkSessionSource, error) {
				loads++
				// A second read sees the opposite state, making any retry reload
				// observable in both loader count and the committed original.
				if sourceCase.available == (loads == 1) {
					return f.source, nil
				}
				return nil, os.ErrNotExist
			})
			reruns := 0
			ctx = server.Testing_WithTxRerunHook(ctx, func() {
				reruns++
				if sourceCase.expiresOnRetry {
					// The hook owns no creation connection; the next attempt reads
					// this exact expiry boundary instead of depending on wall time.
					server.Tx(f.ctx, func(tx server.PgTx) {
						server.RaisePgResult(tx.Exec(f.ctx, `UPDATE provider_work_source_retry_test_clock SET create_time=$1`, time.UnixMicro(f.source.authority.ThroughUnixMicro).UTC()))
					})
				}
			})
			id, _, err := CreateContractNoEscrowWithExpiration(ctx, f.sourceNetworkId, f.sourceId,
				f.destinationNetworkId, f.destinationId, 121, true)
			if err != nil {
				t.Fatalf("%s creation failed: %v", sourceCase.name, err)
			}
			var commits int
			var createdAt time.Time
			server.Db(f.ctx, func(conn server.PgConn) {
				server.Raise(conn.QueryRow(f.ctx, `SELECT last_value FROM provider_work_source_retry_test_attempt`).Scan(&commits))
				server.Raise(conn.QueryRow(f.ctx, `SELECT create_time FROM transfer_contract WHERE contract_id=$1`, id).Scan(&createdAt))
			})
			if commits != 2 || reruns != 1 {
				t.Fatalf("%s did not cross the forced rollback and retry: commits=%d reruns=%d", sourceCase.name, commits, reruns)
			}
			if sourceCase.expiresOnRetry && createdAt.UnixMicro() != f.source.authority.ThroughUnixMicro {
				t.Fatal("retry did not use the exact source expiry boundary")
			}
			wantLoads := 1
			if sourceCase.pinned {
				wantLoads = 0
			}
			if loads != wantLoads {
				t.Fatalf("%s changed source during the actual retry: loads=%d want=%d", sourceCase.name, loads, wantLoads)
			}
			receipts := providerWorkFixtureReceipts(t, f.ctx, id)
			if !sourceCase.available || sourceCase.expiresOnRetry {
				if len(receipts) != 0 {
					t.Fatalf("%s acquired provenance with an absent or expired source", sourceCase.name)
				}
				continue
			}
			reservation := providerWorkFixtureReservation(t, receipts, id)
			if !reservation.Reservation.Complete || reservation.Reservation.RequestFrameHash == nil || *reservation.Reservation.RequestFrameHash != requestHash || reservation.Reservation.UsageOriginIsSource == nil || !*reservation.Reservation.UsageOriginIsSource {
				t.Fatalf("%s lost the actual request or complete original on retry", sourceCase.name)
			}
			for _, receipt := range receipts {
				if err := protocol.VerifyProviderWorkReceiptAuthority(f.ctx, receipt, f.source.authority); err != nil {
					t.Fatalf("%s changed the signed source authority: %v", sourceCase.name, err)
				}
			}
		}
	})
}

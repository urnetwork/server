package model

// The real transaction boundary must precede every advisory notification. The
// deferred-constraint failure pins rollback ordering even when the publisher's
// goroutine is not scheduled until after the creation call returns.

import (
	"context"
	"encoding/hex"
	"errors"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/urnetwork/server"
)

func TestContractOriginPublicationAllCreatorsAreCommitted(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		for _, kind := range []string{"escrow", "companion", "no_escrow"} {
			func() {
				sourceNetworkId, destinationNetworkId := server.NewId(), server.NewId()
				sourceId, destinationId := server.NewId(), server.NewId()
				insertContractLifecycleTestClients(t, ctx, map[server.Id]server.Id{sourceId: sourceNetworkId, destinationId: destinationNetworkId})
				AddBasicTransferBalance(ctx, sourceNetworkId, 1024*1024, server.NowUtc().Add(-time.Minute), server.NowUtc().Add(time.Hour))
				if kind == "companion" {
					if _, err := CreateTransferEscrow(ctx, sourceNetworkId, sourceId, destinationNetworkId, destinationId, 1024); err != nil {
						t.Fatal(err)
					}
					sourceNetworkId, destinationNetworkId = destinationNetworkId, sourceNetworkId
					sourceId, destinationId = destinationId, sourceId
				}
				published := make(chan error, 1)
				owner := newContractOriginNotifications(ctx, DefaultContractOriginNotificationSettings(),
					func(context.Context, string) (contractOriginSubscription, error) {
						return nil, errors.New("unused reader")
					},
					func(publishCtx context.Context, channel string, payload string) error {
						key := contractOriginPairHash(sourceId, destinationId)
						var observedErr error
						if payload != hex.EncodeToString(key[:]) || channel != contractOriginChannel(int(key[0])%contractOriginBucketCount) {
							observedErr = errors.New("wrong ordered-pair event")
						}
						server.Db(publishCtx, func(conn server.PgConn) {
							var count int
							result, err := conn.Query(publishCtx, `SELECT count(*) FROM transfer_contract WHERE source_id=$1 AND destination_id=$2`, sourceId, destinationId)
							server.WithPgResult(result, err, func() {
								if !result.Next() {
									observedErr = errors.New("independent read absent")
									return
								}
								server.Raise(result.Scan(&count))
							})
							if count != 1 {
								observedErr = errors.New("notification preceded committed contract visibility")
							}
						})
						published <- observedErr
						return errors.New("synthetic Redis publication failure")
					})
				defer owner.Close()
				createCtx := WithContractOriginNotifications(ctx, owner)
				var createErr error
				switch kind {
				case "escrow":
					_, createErr = CreateTransferEscrow(createCtx, sourceNetworkId, sourceId, destinationNetworkId, destinationId, 1024)
				case "companion":
					_, createErr = CreateCompanionTransferEscrow(createCtx, sourceNetworkId, sourceId, destinationNetworkId, destinationId, 1024, time.Minute)
				case "no_escrow":
					_, createErr = CreateContractNoEscrow(createCtx, sourceNetworkId, sourceId, destinationNetworkId, destinationId, 1024)
				}
				if createErr != nil {
					t.Fatalf("%s: notification failure changed creation result: %v", kind, createErr)
				}
				select {
				case err := <-published:
					if err != nil {
						t.Fatalf("%s: %v", kind, err)
					}
				case <-time.After(10 * time.Second):
					t.Fatalf("%s: committed origin was not published", kind)
				}
			}()
		}
	})
}

// A scanned INSERT deadline is not a successful result until commit returns.
func TestContractOriginPublicationDoesNotEscapeFailedCommit(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		sourceNetworkId, destinationNetworkId := server.NewId(), server.NewId()
		sourceId, destinationId := server.NewId(), server.NewId()
		insertContractLifecycleTestClients(t, ctx, map[server.Id]server.Id{sourceId: sourceNetworkId, destinationId: destinationNetworkId})
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `CREATE FUNCTION synthetic_contract_commit_failure() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'synthetic contract commit failure'; END $$`))
			server.RaisePgResult(tx.Exec(ctx, `CREATE CONSTRAINT TRIGGER synthetic_contract_commit_failure AFTER INSERT ON transfer_contract DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION synthetic_contract_commit_failure()`))
		})
		owner := newContractOriginNotifications(ctx, DefaultContractOriginNotificationSettings(),
			func(context.Context, string) (contractOriginSubscription, error) {
				return nil, errors.New("unused reader")
			},
			func(context.Context, string, string) error { return nil })
		defer owner.Close()
		before := testutil.ToFloat64(contractOriginNotificationCounter.WithLabelValues("enqueued"))
		var recovered any
		var returnedId server.Id
		var returnedExpiration time.Time
		func() {
			defer func() { recovered = recover() }()
			returnedId, returnedExpiration, _ = CreateContractNoEscrowWithExpiration(WithContractOriginNotifications(ctx, owner), sourceNetworkId, sourceId, destinationNetworkId, destinationId, 1024, true)
		}()
		if recovered == nil {
			t.Fatal("deferred commit failure did not reach transaction caller")
		}
		if returnedId != (server.Id{}) || !returnedExpiration.IsZero() {
			t.Fatal("failed commit returned a signable contract id or deadline")
		}
		if after := testutil.ToFloat64(contractOriginNotificationCounter.WithLabelValues("enqueued")); after != before {
			t.Fatalf("failed commit enqueued an origin: %v -> %v", before, after)
		}
		server.Db(ctx, func(conn server.PgConn) {
			var count int
			result, err := conn.Query(ctx, `SELECT count(*) FROM transfer_contract WHERE source_id=$1 AND destination_id=$2`, sourceId, destinationId)
			server.WithPgResult(result, err, func() {
				if result.Next() {
					server.Raise(result.Scan(&count))
				}
			})
			if count != 0 {
				t.Fatalf("failed commit left %d contracts", count)
			}
		})
	})
}

func TestContractOriginNotificationsRedisEvent(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: false}).Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		owner := NewContractOriginNotifications(ctx, DefaultContractOriginNotificationSettings())
		defer owner.Close()
		sourceId, destinationId := server.NewId(), server.NewId()
		watch := owner.Watch(sourceId, destinationId)
		defer watch.Close()
		for {
			version, update := watch.pair.version.Get()
			if version > 0 {
				break
			}
			select {
			case <-update:
			case <-ctx.Done():
				t.Fatal("Redis subscription acknowledgement missing")
			}
		}
		update := watch.Update()
		notifyCommittedContractOrigin(WithContractOriginNotifications(ctx, owner), sourceId, destinationId)
		select {
		case <-update:
		case <-ctx.Done():
			t.Fatal("committed origin notification did not reach real Redis subscriber")
		}
	})
}

//go:build sidecar_localstack

// T3's connect clients on server's exchange (exchange.go), which needs server's schema: the network, its clients and
// their contracts are server's own rows.

package sidecart3

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/urnetwork/sidecar/sidecartest"
	"github.com/urnetwork/sidecar/sidecartest/fault"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// The private stores, a database of the test's own, with server's migrations applied, as a Net that opens clients
// needs.
func testEnvWithSchema() *server.TestEnv {
	return &server.TestEnv{ApplyDbMigrations: true}
}

// A row on the fake contract issuer and ControlId sink at T2 (catalog.yml's fake_issuer), one builder for both tiers:
// two clients trade a message each way inside one actor's step, and each holds a sealed session with the other.
func clientsBuilder(net *sidecartest.Net) *fault.Schedule {
	return &fault.Schedule{
		Id:    "clients",
		Tiers: []fault.Tier{fault.TierT2, fault.TierT3},
		Steps: []*fault.Step{
			{Actor: "daemonA", Action: func(ctx context.Context, net *sidecartest.Net) error {
				a, err := net.OpenClient("a", nil)
				if err != nil {
					return err
				}
				b, err := net.OpenClient("b", nil)
				if err != nil {
					return err
				}
				asks := make(chan *sidecartest.NetMessage, 4)
				defer b.ReceiveMessages(asks)()
				answers := make(chan *sidecartest.NetMessage, 4)
				defer a.ReceiveMessages(answers)()
				if err := a.SendMessage(b.ClientId, "ask"); err != nil {
					return err
				}
				ask, err := net.AwaitMessage(ctx, asks)
				if err != nil {
					return fmt.Errorf("b: %w", err)
				}
				if ask.Source != a.ClientId || ask.Content != "ask" {
					return fmt.Errorf("b read %+v", ask)
				}
				if err := b.SendMessage(a.ClientId, "answer"); err != nil {
					return err
				}
				answer, err := net.AwaitMessage(ctx, answers)
				if err != nil {
					return fmt.Errorf("a: %w", err)
				}
				if answer.Source != b.ClientId || answer.Content != "answer" {
					return fmt.Errorf("a read %+v", answer)
				}
				if !a.Sealed(b.ClientId) || !b.Sealed(a.ClientId) {
					return errors.New("a pair traded messages with no sealed session")
				}
				return nil
			}},
		},
	}
}

// Q-TEST-1's check on a fake_issuer row: it passes on T2's fake contract issuer and ControlId sink, and on server's
// exchange, controller and stores, from one builder.
func TestT3AFakeIssuerRowPassesOnBothTiers(t *testing.T) {
	testEnvWithSchema().Run(t, func(tb testing.TB) {
		divergence := Diverge(t, clientsBuilder, nil)
		if divergence.Diverged() || divergence.Err() != nil {
			t.Errorf("%v", divergence.Err())
		}
	})
}

// A T3 Net's clients are server's: each is a network client in one network of the Net's, its contract to its peer is
// one the controller signed and stored, and none of it touched the fake issuer.
func TestT3ClientsRunOnServersExchange(t *testing.T) {
	testEnvWithSchema().Run(t, func(tb testing.TB) {
		net := NewNet(t, nil)
		defer net.Close()
		ctx := t.Context()
		a, err := net.OpenClient("a", nil)
		if err != nil {
			t.Fatal(err)
		}
		b, err := net.OpenClient("b", nil)
		if err != nil {
			t.Fatal(err)
		}
		messages := make(chan *sidecartest.NetMessage, 4)
		defer b.ReceiveMessages(messages)()
		if err := a.SendMessage(b.ClientId, "hello"); err != nil {
			t.Fatal(err)
		}
		if message, err := net.AwaitMessage(ctx, messages); err != nil || message.Content != "hello" {
			t.Fatalf("%+v, %v", message, err)
		}
		if !a.Sealed(b.ClientId) {
			t.Error("the sender holds no sealed session")
		}

		clientA := model.GetNetworkClient(ctx, server.Id(a.ClientId))
		clientB := model.GetNetworkClient(ctx, server.Id(b.ClientId))
		if clientA == nil || clientB == nil {
			t.Fatalf("the clients are not server's: %+v, %+v", clientA, clientB)
		}
		if clientA.NetworkId != clientB.NetworkId || clientA.Description != "a" {
			t.Errorf("the clients are %+v and %+v", clientA, clientB)
		}
		contractCount := 0
		server.Db(ctx, func(conn server.PgConn) {
			result, err := conn.Query(
				ctx,
				`SELECT count(*) FROM transfer_contract WHERE source_id = $1 AND destination_id = $2`,
				server.Id(a.ClientId),
				server.Id(b.ClientId),
			)
			server.WithPgResult(result, err, func() {
				if result.Next() {
					server.Raise(result.Scan(&contractCount))
				}
			})
		})
		if contractCount == 0 {
			t.Error("the controller signed no contract from a to b")
		}
		if net.UsesPlatformFakes() {
			t.Error("a T3 client ran on the fake issuer")
		}
	})
}

// A platform that cannot start, here on a database without server's schema, refuses its client with the reason and
// leaves nothing running: the run fails rather than the process.
func TestT3PlatformWithoutTheSchemaRefusesItsClient(t *testing.T) {
	testEnv().Run(t, func(tb testing.TB) {
		net := NewNet(t, nil)
		defer net.Close()
		if _, err := net.OpenClient("a", nil); err == nil || !strings.Contains(err.Error(), "did not start") {
			t.Errorf("a client opened on a platform with no schema: %v", err)
		}
	})
}

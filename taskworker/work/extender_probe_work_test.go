package work

import (
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"fmt"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/connect/protocol"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

// The extender uptime task (connect/EXTENDER.md C3).
//
// The dial itself is proved against a real in-process extender by the
// activation tests. What is proved here is the accounting the task does with
// the outcome, which is where the interesting behavior is: a consecutive
// budget, a deactivation that removes the address from the probe set, and a
// revocation signed and queued exactly when the last address is lost.

// The operator host of the task tests.
const testExtenderWorkNetworkHost = "ur.example"

// Installs an `extender.yml` with a fresh root key and returns the public key
// that verifies what the tasks sign.
func installTestExtenderWorkConfig(t testing.TB) ed25519.PublicKey {
	t.Helper()
	rootKeySeed, err := connect.NewExtenderKeySeed()
	if err != nil {
		t.Fatal(err)
	}
	rootPublicKey, err := connect.ExtenderPublicKeyFromSeed(rootKeySeed)
	if err != nil {
		t.Fatal(err)
	}
	installTestExtenderWorkConfigYaml(t, strings.Join([]string{
		"root_private_key_hex: " + connect.ExtenderKeySeedHex(rootKeySeed),
		"root_public_keys_hex:",
		"  - " + hex.EncodeToString(rootPublicKey),
		"network_host: " + testExtenderWorkNetworkHost,
		"api_url: https://api." + testExtenderWorkNetworkHost,
	}, "\n"))
	return rootPublicKey
}

func installTestExtenderWorkConfigYaml(t testing.TB, body string) {
	t.Helper()
	pop := server.Vault.PushSimpleResource("extender.yml", []byte(body))
	controller.Testing_ResetExtenderConfig()
	t.Cleanup(func() {
		pop()
		controller.Testing_ResetExtenderConfig()
	})
}

// Creates one active extender with an active address per family given.
func createTestExtender(
	ctx context.Context,
	index int,
	ipVersions ...int,
) server.Id {
	extenderId := server.NewId()
	createTime := server.NowUtc()
	addresses := []*model.NetworkExtenderAddress{}
	for _, ipVersion := range ipVersions {
		ip := netip.MustParseAddr(fmt.Sprintf("192.0.2.%d", 1+index))
		if ipVersion == 6 {
			ip = netip.MustParseAddr(fmt.Sprintf("2001:db8::%d", 1+index))
		}
		addresses = append(addresses, &model.NetworkExtenderAddress{
			IpVersion:    ipVersion,
			Ip:           ip,
			Carriers:     []string{connect.ExtenderCarrierTcp},
			ActivateTime: createTime,
			Active:       true,
		})
	}
	model.Testing_CreateNetworkExtender(
		ctx,
		&model.NetworkExtender{
			ExtenderId:  extenderId,
			NetworkId:   server.NewId(),
			ClientId:    server.NewId(),
			PublicKey:   []byte(fmt.Sprintf("extender-public-key-work-%07d", index)),
			CreateTime:  createTime,
			TcpPort:     443,
			UdpPort:     443,
			DnsPort:     53,
			DnsTld:      connect.DefaultExtenderDnsTld,
			CountryCode: "US",
			Active:      true,
		},
		addresses,
	)
	return extenderId
}

// One probed address, as the stub saw it.
type testExtenderProbeCall struct {
	extenderId server.Id
	ipVersion  int
}

// Replaces the probe dial with a stub that answers as told and records what it
// was asked to probe.
func stubExtenderProbe(t testing.TB, answer func() error) func() []testExtenderProbeCall {
	t.Helper()
	// the task probes up to sixteen addresses at once, so the record of what
	// was probed is shared state
	stateLock := sync.Mutex{}
	calls := []testExtenderProbeCall{}
	previous := probeExtenderAddress
	probeExtenderAddress = func(
		ctx context.Context,
		target *model.NetworkExtenderProbeTarget,
		serverName string,
		destinationHost string,
	) error {
		// the task must front every probe with a name under the operator host
		// and ask for the api as its destination, exactly as an activation
		// does; a stub that ignored them would hide a regression there
		if !strings.HasSuffix(serverName, "."+testExtenderWorkNetworkHost) {
			t.Errorf("probe server name %q is not under the network host", serverName)
		}
		if destinationHost != "api."+testExtenderWorkNetworkHost {
			t.Errorf("probe destination %q is not the api host", destinationHost)
		}
		func() {
			stateLock.Lock()
			defer stateLock.Unlock()
			calls = append(calls, testExtenderProbeCall{
				extenderId: target.ExtenderId,
				ipVersion:  target.IpVersion,
			})
		}()
		return answer()
	}
	t.Cleanup(func() {
		probeExtenderAddress = previous
	})
	return func() []testExtenderProbeCall {
		stateLock.Lock()
		defer stateLock.Unlock()
		return slices.Clone(calls)
	}
}

func runTestExtenderProbe(t testing.TB, ctx context.Context) *ExtenderProbeResult {
	t.Helper()
	clientSession := session.NewLocalClientSession(ctx, "0.0.0.0:0", nil)
	defer clientSession.Cancel()
	result, err := ExtenderProbe(&ExtenderProbeArgs{}, clientSession)
	if err != nil {
		t.Fatalf("the probe task must not fail on probe failures: %v", err)
	}
	return result
}

// Six consecutive failures deactivate the address; because it was the last
// one, the extender is revoked with a revocation that verifies under the root
// key, and the address is never probed again.
func TestExtenderProbeDeactivatesAndRevokesAfterSixFailures(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		rootPublicKey := installTestExtenderWorkConfig(t)
		extenderId := createTestExtender(ctx, 1, 4)

		calls := stubExtenderProbe(t, func() error {
			return fmt.Errorf("the extender did not answer")
		})

		for i := range ExtenderMaxConsecutiveProbeFailures {
			result := runTestExtenderProbe(t, ctx)
			last := i == ExtenderMaxConsecutiveProbeFailures-1
			connect.AssertEqual(t, result.Probed, 1)
			connect.AssertEqual(t, result.Failed, 1)
			connect.AssertEqual(t, result.Succeeded, 0)
			if last {
				connect.AssertEqual(t, result.Deactivated, 1)
				connect.AssertEqual(t, result.Revoked, 1)
			} else {
				connect.AssertEqual(t, result.Deactivated, 0)
				connect.AssertEqual(t, result.Revoked, 0)
			}
		}

		stored := model.Testing_GetNetworkExtender(ctx, extenderId)
		connect.AssertEqual(t, stored.Addresses[0].Active, false)
		connect.AssertEqual(t, stored.Extender.Active, false)
		connect.AssertEqual(t, stored.Extender.RevokeTime != nil, true)

		publishes := model.Testing_GetNetworkExtenderPublishes(ctx)
		connect.AssertEqual(t, len(publishes), 1)
		connect.AssertEqual(t, publishes[0].Kind, model.NetworkExtenderPublishKindRevocation)

		message := &protocol.ExtenderGossipMessage{}
		if err := proto.Unmarshal(publishes[0].Message, message); err != nil {
			t.Fatalf("the publish row is not a gossip message: %v", err)
		}
		body, err := connect.NewExtenderRootKeySet(rootPublicKey).VerifyRevocation(
			message.GetRevocation(),
		)
		if err != nil {
			t.Fatalf("the revocation does not verify under the root key: %v", err)
		}
		connect.AssertEqual(t, string(body.PublicKey), string(stored.Extender.PublicKey))
		connect.AssertEqual(t, body.NetworkHost, testExtenderWorkNetworkHost)

		// a deactivated address leaves the probe set: only a new activation
		// brings it back
		probedBefore := len(calls())
		result := runTestExtenderProbe(t, ctx)
		connect.AssertEqual(t, result.Probed, 0)
		connect.AssertEqual(t, len(calls()), probedBefore)
		connect.AssertEqual(t, len(model.Testing_GetNetworkExtenderPublishes(ctx)), 1)
	})
}

// A success anywhere in the run clears the budget, so an extender that flaps
// is never removed for the sum of its bad minutes.
func TestExtenderProbeSuccessResetsTheBudget(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		installTestExtenderWorkConfig(t)
		extenderId := createTestExtender(ctx, 2, 4)

		succeed := false
		stubExtenderProbe(t, func() error {
			if succeed {
				return nil
			}
			return fmt.Errorf("the extender did not answer")
		})

		for range ExtenderMaxConsecutiveProbeFailures - 1 {
			runTestExtenderProbe(t, ctx)
		}
		stored := model.Testing_GetNetworkExtender(ctx, extenderId)
		connect.AssertEqual(t, stored.Addresses[0].ConsecutiveProbeFailures, 5)

		succeed = true
		result := runTestExtenderProbe(t, ctx)
		connect.AssertEqual(t, result.Succeeded, 1)
		connect.AssertEqual(t, result.Failed, 0)
		stored = model.Testing_GetNetworkExtender(ctx, extenderId)
		connect.AssertEqual(t, stored.Addresses[0].ConsecutiveProbeFailures, 0)
		connect.AssertEqual(t, stored.Addresses[0].LastProbeSuccessTime != nil, true)

		succeed = false
		for range ExtenderMaxConsecutiveProbeFailures - 1 {
			runTestExtenderProbe(t, ctx)
		}
		stored = model.Testing_GetNetworkExtender(ctx, extenderId)
		connect.AssertEqual(t, stored.Addresses[0].Active, true)
		connect.AssertEqual(t, stored.Extender.Active, true)
		connect.AssertEqual(t, len(model.Testing_GetNetworkExtenderPublishes(ctx)), 0)
	})
}

// Losing one family of a dual-stack extender deactivates that address only,
// and the other family keeps being probed. Revoking here would take a working
// extender out of every directory.
func TestExtenderProbeKeepsTheExtenderWhileAFamilyAnswers(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		installTestExtenderWorkConfig(t)
		extenderId := createTestExtender(ctx, 3, 4, 6)

		calls := stubExtenderProbe(t, func() error {
			return nil
		})
		// only the v4 address fails
		previous := probeExtenderAddress
		probeExtenderAddress = func(
			ctx context.Context,
			target *model.NetworkExtenderProbeTarget,
			serverName string,
			destinationHost string,
		) error {
			if err := previous(ctx, target, serverName, destinationHost); err != nil {
				return err
			}
			if target.IpVersion == 4 {
				return fmt.Errorf("the v4 address did not answer")
			}
			return nil
		}

		for range ExtenderMaxConsecutiveProbeFailures {
			result := runTestExtenderProbe(t, ctx)
			connect.AssertEqual(t, result.Probed, 2)
			connect.AssertEqual(t, result.Revoked, 0)
		}

		stored := model.Testing_GetNetworkExtender(ctx, extenderId)
		connect.AssertEqual(t, stored.Addresses[0].IpVersion, 4)
		connect.AssertEqual(t, stored.Addresses[0].Active, false)
		connect.AssertEqual(t, stored.Addresses[1].IpVersion, 6)
		connect.AssertEqual(t, stored.Addresses[1].Active, true)
		connect.AssertEqual(t, stored.Extender.Active, true)
		connect.AssertEqual(t, len(model.Testing_GetNetworkExtenderPublishes(ctx)), 0)

		// the next run probes the surviving family only
		before := len(calls())
		runTestExtenderProbe(t, ctx)
		probed := calls()[before:]
		connect.AssertEqual(t, len(probed), 1)
		connect.AssertEqual(t, probed[0].ipVersion, 6)
	})
}

// An operator with no root key cannot revoke, so the task probes nothing
// rather than deactivating addresses it could never withdraw a record for.
func TestExtenderProbePausesWithoutARootKey(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		installTestExtenderWorkConfigYaml(t, strings.Join([]string{
			"network_host: " + testExtenderWorkNetworkHost,
			"api_url: https://api." + testExtenderWorkNetworkHost,
		}, "\n"))
		extenderId := createTestExtender(ctx, 4, 4)

		calls := stubExtenderProbe(t, func() error {
			return fmt.Errorf("the extender did not answer")
		})

		result := runTestExtenderProbe(t, ctx)
		connect.AssertEqual(t, result.Probed, 0)
		connect.AssertEqual(t, len(calls()), 0)

		stored := model.Testing_GetNetworkExtender(ctx, extenderId)
		connect.AssertEqual(t, stored.Addresses[0].Active, true)
		connect.AssertEqual(t, stored.Addresses[0].ConsecutiveProbeFailures, 0)
	})
}

// The chain re-arms every five minutes, which is the whole liveness cadence:
// a Post that did not reschedule would leave the directory unchecked forever.
func TestExtenderProbePostRearmsTheChain(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		clientSession := session.NewLocalClientSession(ctx, "0.0.0.0:0", nil)
		defer clientSession.Cancel()

		before := server.NowUtc()
		server.Tx(ctx, func(tx server.PgTx) {
			if err := ExtenderProbePost(
				&ExtenderProbeArgs{},
				&ExtenderProbeResult{},
				clientSession,
				tx,
			); err != nil {
				t.Fatalf("ExtenderProbePost: %v", err)
			}
		})

		runAt := testExtenderTaskRunAt(t, ctx, "extender_probe")
		want := before.Add(ExtenderProbeTimeout)
		if runAt.Before(want.Add(-time.Second)) || want.Add(5*time.Second).Before(runAt) {
			t.Fatalf("probe run_at = %s, want about %s", runAt, want)
		}
	})
}

// The run_at of one scheduled recurring task.
func testExtenderTaskRunAt(t testing.TB, ctx context.Context, runOnceKey string) time.Time {
	t.Helper()
	var runAt time.Time
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`SELECT run_at FROM pending_task WHERE run_once_key = $1`,
			fmt.Sprintf("[%q]", runOnceKey),
		)
		server.WithPgResult(result, err, func() {
			if !result.Next() {
				t.Fatalf("no %s task was scheduled", runOnceKey)
			}
			server.Raise(result.Scan(&runAt))
		})
	})
	return runAt
}

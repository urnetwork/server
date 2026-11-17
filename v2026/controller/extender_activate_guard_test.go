package controller

import (
	"context"
	"encoding/hex"
	"fmt"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/session"
)

// The probe guard of the activations of one api process (connect#230), and
// the fake probes and fleets the activation budget tests share.

// Fake probes for a caller a test cannot bind: the carrier probe answers
// `carrierErr`, nil passing, and the forward arrives from the caller's own
// address, so the family rule of C2a holds.
func testFakeExtenderProbes(carrierErr error) func(clientIp netip.Addr, ipVersion int) *extenderActivationProbes {
	return func(clientIp netip.Addr, ipVersion int) *extenderActivationProbes {
		return &extenderActivationProbes{
			carrier: func(ctx context.Context, connectMode connect.ExtenderConnectMode, port int) error {
				return carrierErr
			},
			forward: func(ctx context.Context) (string, error) {
				return netip.AddrPortFrom(clientIp, 443).String(), nil
			},
		}
	}
}

// One account of the activation budget tests: a network and its user.
type testExtenderAccount struct {
	networkId server.Id
	userId    server.Id
}

func newTestExtenderAccount(ctx context.Context) *testExtenderAccount {
	account := &testExtenderAccount{
		networkId: server.NewId(),
		userId:    server.NewId(),
	}
	model.Testing_CreateNetwork(
		ctx,
		account.networkId,
		fmt.Sprintf("extender-budget-%s", account.networkId),
		account.userId,
	)
	return account
}

// One extender host of an account: its own client and its own identity key.
type testExtenderHost struct {
	byJwt     *session.ByJwt
	publicKey []byte
}

func (self *testExtenderAccount) newHost(t testing.TB, ctx context.Context) *testExtenderHost {
	t.Helper()
	deviceId := server.NewId()
	clientId := server.NewId()
	model.Testing_CreateDevice(ctx, self.networkId, deviceId, clientId, "extender", "extender-budget-test")
	keySeed, err := connect.NewExtenderKeySeed()
	if err != nil {
		t.Fatal(err)
	}
	publicKey, err := connect.ExtenderPublicKeyFromSeed(keySeed)
	if err != nil {
		t.Fatal(err)
	}
	return &testExtenderHost{
		byJwt: &session.ByJwt{
			NetworkId: self.networkId,
			UserId:    self.userId,
			DeviceId:  &deviceId,
			ClientId:  &clientId,
		},
		publicKey: publicKey,
	}
}

// The host's session for an activation from `clientIp`, which is the address
// the fake probes answer for.
func (self *testExtenderHost) session(ctx context.Context, clientIp string) *session.ClientSession {
	clientSession := session.Testing_CreateClientSession(ctx, self.byJwt)
	clientSession.ClientAddress = netip.AddrPortFrom(netip.MustParseAddr(clientIp), 54321).String()
	return clientSession
}

// The host's activation over the tcp carrier alone, the one the fake probes
// answer.
func (self *testExtenderHost) activateArgs() *ExtenderActivateArgs {
	return &ExtenderActivateArgs{
		PublicKeyHex: hex.EncodeToString(self.publicKey),
		TcpPort:      443,
		Carriers:     []string{connect.ExtenderCarrierTcp},
	}
}

// The wait a refusal asks for (`retry_after_seconds`) goes out in whole
// seconds, rounded up so a caller that waits it out is never early, and never
// as 0, which would read as no wait at all.
func TestExtenderActivateRetryAfterSecondsRoundsUp(t *testing.T) {
	cases := []struct {
		retryAfter time.Duration
		want       int
	}{
		{retryAfter: 0, want: 1},
		{retryAfter: time.Nanosecond, want: 1},
		{retryAfter: time.Second, want: 1},
		{retryAfter: time.Second + time.Millisecond, want: 2},
		{retryAfter: 90 * time.Second, want: 90},
		{retryAfter: 59*time.Minute + 59*time.Second + time.Microsecond, want: 3600},
	}
	for _, c := range cases {
		if got := extenderActivateRetryAfterSeconds(c.retryAfter); got != c.want {
			t.Errorf("%s goes out as %d seconds, want %d", c.retryAfter, got, c.want)
		}
	}
}

// A busy refusal asks for a wait drawn between its bounds, so a fleet refused
// together does not come back together; equal bounds ask for exactly that
// wait. The defaults hold 64 probes and ask for one to three minutes.
func TestExtenderActivateBusyRetryAfterIsWithinItsBounds(t *testing.T) {
	settings := DefaultExtenderActivateSettings()
	connect.AssertEqual(t, settings.MaxConcurrentProbes, 64)
	connect.AssertEqual(t, settings.BusyRetryAfterMin, time.Minute)
	connect.AssertEqual(t, settings.BusyRetryAfterMax, 3*time.Minute)

	service := newExtenderActivateService(settings)
	shortest := settings.BusyRetryAfterMax
	longest := settings.BusyRetryAfterMin
	for range 10000 {
		retryAfter := service.busyRetryAfter()
		if retryAfter < settings.BusyRetryAfterMin || settings.BusyRetryAfterMax < retryAfter {
			t.Fatalf("a busy refusal asked for %s, outside %s to %s", retryAfter, settings.BusyRetryAfterMin, settings.BusyRetryAfterMax)
		}
		shortest = min(shortest, retryAfter)
		longest = max(longest, retryAfter)
	}
	// ten thousand uniform draws over two minutes fall within a second of
	// both ends unless the draw is not spread at all
	if time.Second < shortest-settings.BusyRetryAfterMin || time.Second < settings.BusyRetryAfterMax-longest {
		t.Errorf("the busy waits ranged only %s to %s", shortest, longest)
	}

	settings.BusyRetryAfterMin = 90 * time.Second
	settings.BusyRetryAfterMax = 90 * time.Second
	connect.AssertEqual(t, newExtenderActivateService(settings).busyRetryAfter(), 90*time.Second)
}

// The process probes at most MaxConcurrentProbes activations at once
// (connect#230). One more is refused as busy with a `retry_after_seconds`
// before it spends any budget, and once a probe finishes the next activation
// is probed again. Before, every activation was probed as it arrived, so a
// fleet restarting at once held a handler per activation for up to the 10 s
// probe budget each.
func TestExtenderActivateRefusesBusyOverTheProbeGuard(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		installTestExtenderConfig(t, nil)

		settings := DefaultExtenderActivateSettings()
		settings.MaxConcurrentProbes = 2
		settings.BusyRetryAfterMin = 90 * time.Second
		settings.BusyRetryAfterMax = 90 * time.Second
		service := newExtenderActivateService(settings)

		// every probe waits for the release, and says when it has begun
		probeStarted := make(chan netip.Addr, 8)
		probeRelease := make(chan struct{})
		service.testProbes = func(clientIp netip.Addr, ipVersion int) *extenderActivationProbes {
			probes := testFakeExtenderProbes(nil)(clientIp, ipVersion)
			passCarrier := probes.carrier
			probes.carrier = func(ctx context.Context, connectMode connect.ExtenderConnectMode, port int) error {
				probeStarted <- clientIp
				select {
				case <-probeRelease:
				case <-ctx.Done():
					return ctx.Err()
				}
				return passCarrier(ctx, connectMode, port)
			}
			return probes
		}

		account := newTestExtenderAccount(ctx)
		hosts := []*testExtenderHost{}
		for range 3 {
			hosts = append(hosts, account.newHost(t, ctx))
		}
		hostIps := []string{"198.51.100.1", "198.51.100.2", "198.51.100.3"}
		activate := func(i int) *ExtenderActivateResult {
			result, err := service.activate(hosts[i].activateArgs(), hosts[i].session(ctx, hostIps[i]))
			if err != nil {
				t.Errorf("host %d: %s", i+1, err)
				return &ExtenderActivateResult{}
			}
			return result
		}

		// two activations hold the guard in their probes
		var heldWait sync.WaitGroup
		heldResults := make([]*ExtenderActivateResult, 2)
		for i := range 2 {
			heldWait.Add(1)
			go func() {
				defer heldWait.Done()
				heldResults[i] = activate(i)
			}()
		}
		for range 2 {
			select {
			case <-probeStarted:
			case <-time.After(30 * time.Second):
				t.Fatalf("the first two activations did not reach their probes")
			}
		}

		// the third is refused as busy, and spends nothing
		spentBefore := testAccountActionAttemptCount(t, ctx, account.userId)
		busy := activate(2)
		if busy.Activated || !strings.Contains(busy.Error, "busy") {
			close(probeRelease)
			heldWait.Wait()
			t.Fatalf("the third activation over the guard was not refused as busy: activated %t, %q", busy.Activated, busy.Error)
		}
		connect.AssertEqual(t, busy.RetryAfterSeconds, 90)
		if !strings.Contains(busy.Error, "Please try again in 2 minutes.") {
			t.Errorf("the busy refusal %q does not name its wait", busy.Error)
		}
		connect.AssertEqual(t, testAccountActionAttemptCount(t, ctx, *hosts[2].byJwt.ClientId), 0)
		connect.AssertEqual(t, testAccountActionAttemptCount(t, ctx, account.userId), spentBefore)

		close(probeRelease)
		heldWait.Wait()
		for i, result := range heldResults {
			if !result.Activated {
				t.Fatalf("held activation %d was refused: %q", i+1, result.Error)
			}
			connect.AssertEqual(t, result.RetryAfterSeconds, 0)
		}

		// with the guard free again, the third is probed
		if result := activate(2); !result.Activated {
			t.Fatalf("the third activation was refused once the guard was free: %q", result.Error)
		}
		select {
		case clientIp := <-probeStarted:
			connect.AssertEqual(t, clientIp.String(), hostIps[2])
		default:
			t.Fatalf("the third activation was not probed")
		}
	})
}

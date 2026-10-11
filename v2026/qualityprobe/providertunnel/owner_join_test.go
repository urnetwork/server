// A probe's close callback is its worker-slot boundary, not just a canceled
// wait. These controls use the real generator and its admitted credentials.
package providertunnel

import (
	"context"
	"errors"
	"net"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/v2026"
)

// Retains the actual strategy's cancellation behavior while refusing all
// sockets and giving the credential callback its ordinary independent budget.
func ownerJoinTestStrategy(t *testing.T, ctx context.Context) *connect.ClientStrategy {
	t.Helper()
	var calls atomic.Int32
	refuse := func(context.Context, string, string) (net.Conn, error) {
		calls.Add(1)
		return nil, errors.New("synthetic owner fixture forbids sockets")
	}
	settings := connect.DefaultClientStrategySettings()
	settings.RequestTimeout = time.Minute
	settings.Resolver = &net.Resolver{PreferGo: true, Dial: refuse}
	settings.DialContextSettings = &connect.DialContextSettings{DialContext: refuse}
	strategy := connect.NewClientStrategy(ctx, settings)
	t.Cleanup(func() {
		strategy.Close()
		if calls.Load() != 0 {
			t.Error("internal retirement attempted a public socket")
		}
	})
	return strategy
}

// Only the credential authority is synthetic; its lifecycle and request
// admission are the production generator's. Child identity is locally made.
func ownerJoinTestGenerator(t *testing.T, ctx context.Context, remove func(context.Context, *connect.RemoveNetworkClientArgs) (*connect.RemoveNetworkClientResult, error)) (*connect.ApiMultiClientGenerator, *connect.ClientStrategy) {
	t.Helper()
	strategy := ownerJoinTestStrategy(t, ctx)
	settings := connect.DefaultApiMultiClientGeneratorSettings()
	settings.ClientCredentials = &syntheticProbeCredentials{remove: remove}
	generator := connect.NewApiMultiClientGenerator(ctx, nil, strategy, nil,
		"https://api.owner.example", "synthetic-parent-token", "wss://platform.owner.example",
		"synthetic probe", "synthetic", "test", nil, connect.DefaultClientSettings, settings)
	return generator, strategy
}

// Represents an admitted external control callback at the real generator's
// OOB join boundary. A wait timeout does not make that callback terminal.
type ownerJoinTestOob struct {
	connect.OutOfBandControl
	entered chan struct{}
	release chan struct{}
}

func (self *ownerJoinTestOob) CloseAndWait(ctx context.Context) error {
	close(self.entered)
	select {
	case <-self.release:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// A real admitted credential request remains live after the graceful deadline
// and strategy cancellation. The URL worker must remain owned until it finishes.
func TestCloseTunnelPartsRetainsActualGeneratorAfterDeadline(t *testing.T) {
	connect.GetMessagePoolAggregateStats()
	synctest.Test(t, func(t *testing.T) {
		lifecycleCtx, cancelLifecycle := context.WithCancel(t.Context())
		retired := make(chan struct{})
		childId := connect.NewId()
		generator, strategy := ownerJoinTestGenerator(t, lifecycleCtx, func(ctx context.Context, args *connect.RemoveNetworkClientArgs) (*connect.RemoveNetworkClientResult, error) {
			if args.ClientId != childId || ctx.Err() != nil {
				t.Error("strategy/lifecycle cancellation changed the independent retirement request")
			}
			if deadline, ok := ctx.Deadline(); !ok || time.Until(deadline) != time.Minute {
				t.Error("late credential retirement lost its fresh finite request budget")
			}
			close(retired)
			return &connect.RemoveNetworkClientResult{}, nil
		})
		oob := &ownerJoinTestOob{OutOfBandControl: connect.NewNoContractClientOob(), entered: make(chan struct{}), release: make(chan struct{})}
		clientSettings := connect.DefaultClientSettings()
		clientSettings.EncryptionSettings.Mode = connect.EncryptionModeOff
		clientSettings.ControlPingTimeout = 0
		clientSettings.Log = connect.NewNoopLogger()
		client := connect.NewClient(lifecycleCtx, childId, oob, clientSettings)
		generator.RemoveClientWithArgs(client, &connect.MultiClientGeneratorClientArgs{ClientId: childId})
		client.Cancel()
		<-oob.entered
		pumpDone := make(chan struct{})
		close(pumpDone)
		order := []string{}
		multiClient := &recordingCloseWaiter{name: "multi-client", order: &order}
		strategyClosed := make(chan struct{})
		closeCtx, closeCancel := context.WithTimeout(t.Context(), time.Second)
		defer closeCancel()
		closed := make(chan error, 1)
		go func() {
			closed <- closeTunnelParts(closeCtx, func() {}, func() error { return nil }, pumpDone, multiClient, generator,
				func() { strategy.Close(); close(strategyClosed) }, cancelLifecycle)
		}()
		<-strategyClosed
		synctest.Wait()
		escaped := false
		var closeErr error
		select {
		case closeErr = <-closed:
			escaped = true
		default:
		}
		close(oob.release)
		if !escaped {
			closeErr = <-closed
		}
		if err := generator.CloseAndWait(t.Context()); err != nil {
			t.Fatal(err)
		}
		<-retired
		if escaped {
			t.Error("tunnel close released its URL worker before admitted generator retirement completed")
		}
		if !errors.Is(closeErr, context.DeadlineExceeded) || lifecycleCtx.Err() == nil {
			t.Errorf("terminal join lost the original deadline/cancellation: %v", closeErr)
		}
	})
}

// The rare partial-Open cleanup must have the same terminal ownership contract
// as a successful Open's close callback, even though no tunnel is returned.
func TestOpenFailureRetainsActualGeneratorAfterDeadline(t *testing.T) {
	connect.GetMessagePoolAggregateStats()
	synctest.Test(t, func(t *testing.T) {
		oldTun, oldStrategy, oldGenerator := createTun, newControlplaneClientStrategy, newApiMultiClientGenerator
		defer func() {
			createTun, newControlplaneClientStrategy, newApiMultiClientGenerator = oldTun, oldStrategy, oldGenerator
		}()
		entered, release, retired, lifecycleCanceled := make(chan struct{}), make(chan struct{}), make(chan struct{}), make(chan struct{})
		childId := connect.NewId()
		authority := &syntheticProbeCredentials{remove: func(ctx context.Context, args *connect.RemoveNetworkClientArgs) (*connect.RemoveNetworkClientResult, error) {
			close(entered)
			select {
			case <-release:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
			if ctx.Err() != nil || args.ClientId != childId {
				t.Error("partial-open retirement lost its independent request")
			}
			close(retired)
			return &connect.RemoveNetworkClientResult{}, nil
		}}
		newControlplaneClientStrategy = func(ctx context.Context) *connect.ClientStrategy {
			go func() { <-ctx.Done(); close(lifecycleCanceled) }()
			return ownerJoinTestStrategy(t, ctx)
		}
		var generator *connect.ApiMultiClientGenerator
		newApiMultiClientGenerator = func(ctx context.Context, specs []*connect.ProviderSpec, strategy *connect.ClientStrategy, exclude []connect.Id,
			apiUrl, token, platformUrl, description, device, version string, source *connect.Id,
			clientSettings func() *connect.ClientSettings, settings *connect.ApiMultiClientGeneratorSettings) *connect.ApiMultiClientGenerator {
			generator = connect.NewApiMultiClientGenerator(ctx, specs, strategy, exclude, apiUrl, token, platformUrl,
				description, device, version, source, clientSettings, settings)
			return generator
		}
		openErr := errors.New("synthetic tun construction failed")
		createTun = func(context.Context, *connect.DnsResolverSettings) (*connect.Tun, error) {
			generator.RemoveClientArgs(&connect.MultiClientGeneratorClientArgs{ClientId: childId})
			<-entered
			return nil, openErr
		}
		closed := make(chan error, 1)
		go func() {
			tunnel, err := Open(t.Context(), Config{
				ApiUrl: "https://api.owner.example", PlatformUrl: "wss://platform.owner.example", ByJwt: "synthetic-parent-token",
				ClientId: connect.NewId(), ClientCredentials: authority, CloseTimeout: time.Second,
			}, connect.NewId())
			if tunnel != nil {
				t.Error("failed constructor returned a tunnel")
			}
			closed <- err
		}()
		<-lifecycleCanceled
		synctest.Wait()
		escaped := false
		var closeErr error
		select {
		case closeErr = <-closed:
			escaped = true
		default:
		}
		close(release)
		if !escaped {
			closeErr = <-closed
		}
		if err := generator.CloseAndWait(t.Context()); err != nil {
			t.Fatal(err)
		}
		<-retired
		if escaped {
			t.Error("failed Open returned while its admitted generator still owned retirement")
		}
		if !errors.Is(closeErr, openErr) || !errors.Is(closeErr, context.DeadlineExceeded) {
			t.Errorf("terminal Open cleanup lost its construction/deadline error: %v", closeErr)
		}
	})
}

// A slow owner must occupy its own worker only. A second private generator
// closes immediately without waiting for or inheriting the first owner's error.
func TestCloseTunnelPartsOwnersRemainIndependent(t *testing.T) {
	connect.GetMessagePoolAggregateStats()
	synctest.Test(t, func(t *testing.T) {
		entered, release, canceled := make(chan struct{}), make(chan struct{}), make(chan struct{})
		firstCtx, cancelFirst := context.WithCancel(t.Context())
		first, firstStrategy := ownerJoinTestGenerator(t, firstCtx, func(ctx context.Context, args *connect.RemoveNetworkClientArgs) (*connect.RemoveNetworkClientResult, error) {
			close(entered)
			select {
			case <-release:
				return &connect.RemoveNetworkClientResult{}, nil
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		})
		first.RemoveClientArgs(&connect.MultiClientGeneratorClientArgs{ClientId: connect.NewId()})
		<-entered
		pumpDone := make(chan struct{})
		close(pumpDone)
		firstOrder, secondOrder := []string{}, []string{}
		closeCtx, cancelClose := context.WithCancel(t.Context())
		cancelClose()
		firstClosed := make(chan error, 1)
		go func() {
			firstClosed <- closeTunnelParts(closeCtx, func() {}, func() error { return nil }, pumpDone,
				&recordingCloseWaiter{name: "first", order: &firstOrder}, first, firstStrategy.Close,
				func() { cancelFirst(); close(canceled) })
		}()
		<-canceled
		secondCtx, cancelSecond := context.WithCancel(t.Context())
		second, secondStrategy := ownerJoinTestGenerator(t, secondCtx, func(ctx context.Context, args *connect.RemoveNetworkClientArgs) (*connect.RemoveNetworkClientResult, error) {
			if ctx.Err() != nil {
				t.Error("healthy retirement inherited another owner's cancellation")
			}
			return &connect.RemoveNetworkClientResult{}, nil
		})
		second.RemoveClientArgs(&connect.MultiClientGeneratorClientArgs{ClientId: connect.NewId()})
		if err := closeTunnelParts(t.Context(), func() {}, func() error { return nil }, pumpDone,
			&recordingCloseWaiter{name: "second", order: &secondOrder}, second, secondStrategy.Close, cancelSecond); err != nil {
			t.Fatal(err)
		}
		synctest.Wait()
		escaped := false
		var firstErr error
		select {
		case firstErr = <-firstClosed:
			escaped = true
		default:
		}
		close(release)
		if !escaped {
			firstErr = <-firstClosed
		}
		if err := first.CloseAndWait(t.Context()); err != nil {
			t.Fatal(err)
		}
		if escaped || !errors.Is(firstErr, context.Canceled) {
			t.Errorf("slow owner's final join was lost: escaped=%t err=%v", escaped, firstErr)
		}
	})
}

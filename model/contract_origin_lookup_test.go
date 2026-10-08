package model

// Barriers and virtual time isolate repeated missing reads from successful
// contract writes. The source callback represents both origin SQL statements.

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/urnetwork/connect"
	"github.com/urnetwork/server"
)

// No worker is needed for coordinator-only controls; notification controls
// below use the real owner and its synthetic subscription transport.
func testContractOriginLookupWatch(ctx context.Context) *ContractOriginWatch {
	return &ContractOriginWatch{
		owner: &ContractOriginNotifications{ctx: ctx},
		pair:  &contractOriginPair{version: connect.NewMonitorValue(uint64(0))},
	}
}

func TestContractOriginLookupCoalescesMissingSqlWork(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const requests = 64
		readCounts := []int64{}
		for _, watch := range []*ContractOriginWatch{nil, testContractOriginLookupWatch(t.Context())} {
			var reads atomic.Int64
			release := make(chan struct{})
			results := make(chan error, requests)
			for range requests {
				go func() {
					_, err := watch.Lookup(t.Context(), false, func() (*TransferEscrow, error) {
						reads.Add(2)
						<-release
						return nil, ErrMissingCompanionOrigin
					})
					results <- err
				}()
			}
			synctest.Wait()
			close(release)
			for range requests {
				if err := <-results; err != ErrMissingCompanionOrigin {
					t.Fatalf("missing result changed: %v", err)
				}
			}
			readCounts = append(readCounts, reads.Load())
		}
		if readCounts[0] != 2*requests || readCounts[1] != 2 {
			t.Fatalf("missing SQL reads before/after = %v, want [128 2]", readCounts)
		}
		t.Logf("64 concurrent same-pair missing requests: source SQL reads %d -> %d", readCounts[0], readCounts[1])
	})
}

func TestContractOriginLookupSuccessesCreateIndependently(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const requests = 32
		watch := testContractOriginLookupWatch(t.Context())
		var calls atomic.Int64
		release := make(chan struct{})
		results := make(chan *TransferEscrow, requests)
		for range requests {
			go func() {
				escrow, err := watch.Lookup(t.Context(), false, func() (*TransferEscrow, error) {
					calls.Add(1)
					<-release
					return &TransferEscrow{}, nil
				})
				if err != nil {
					t.Errorf("successful creation failed: %v", err)
				}
				results <- escrow
			}()
		}
		synctest.Wait()
		close(release)
		seen := map[*TransferEscrow]bool{}
		for range requests {
			result := <-results
			if result == nil || seen[result] {
				t.Fatal("successful contract result was shared")
			}
			seen[result] = true
		}
		if calls.Load() != requests {
			t.Fatalf("successful create calls = %d, want %d", calls.Load(), requests)
		}
	})
}

func TestContractOriginLookupMissingExpiresFromReadStart(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		watch := testContractOriginLookupWatch(t.Context())
		calls := 0
		create := func() (*TransferEscrow, error) {
			calls++
			if calls == 1 {
				time.Sleep(90 * time.Millisecond)
				return nil, ErrMissingCompanionOrigin
			}
			return &TransferEscrow{}, nil
		}
		_, _ = watch.Lookup(t.Context(), false, create)
		if _, err := watch.Lookup(t.Context(), false, create); err != ErrMissingCompanionOrigin || calls != 1 {
			t.Fatalf("recent miss not reused: err=%v calls=%d", err, calls)
		}
		time.Sleep(10 * time.Millisecond)
		if got, err := watch.Lookup(t.Context(), false, create); got == nil || err != nil || calls != 2 {
			t.Fatalf("read completion extended absence: got=%v err=%v calls=%d", got, err, calls)
		}
	})
}

func TestContractOriginLookupEventDuringReadInvalidatesMissing(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		watch := testContractOriginLookupWatch(t.Context())
		release := make(chan struct{})
		finished := make(chan struct{})
		go func() {
			defer close(finished)
			_, _ = watch.Lookup(t.Context(), false, func() (*TransferEscrow, error) {
				<-release
				return nil, ErrMissingCompanionOrigin
			})
		}()
		synctest.Wait()
		watch.pair.lookup.invalidate()
		close(release)
		<-finished
		want := &TransferEscrow{}
		got, err := watch.Lookup(t.Context(), false, func() (*TransferEscrow, error) { return want, nil })
		if got != want || err != nil {
			t.Fatalf("pre-event absence was republished: got=%v err=%v", got, err)
		}
	})
}

func TestContractOriginLookupDeadlineBypassesPendingAndCachedMissing(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		watch := testContractOriginLookupWatch(t.Context())
		release := make(chan struct{})
		finished := make(chan struct{})
		go func() {
			defer close(finished)
			_, _ = watch.Lookup(t.Context(), false, func() (*TransferEscrow, error) {
				<-release
				return nil, ErrMissingCompanionOrigin
			})
		}()
		synctest.Wait()
		want := &TransferEscrow{}
		for range 2 {
			got, err := watch.Lookup(t.Context(), true, func() (*TransferEscrow, error) { return want, nil })
			if got != want || err != nil {
				t.Fatalf("final lookup did not run independently: got=%v err=%v", got, err)
			}
			if release != nil {
				close(release)
				<-finished
				release = nil
			}
		}
	})
}

func TestContractOriginLookupCancellationAndOperationalFailureAreNotShared(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		watch := testContractOriginLookupWatch(t.Context())
		leaderCtx, cancel := context.WithCancel(t.Context())
		leader := make(chan error, 1)
		go func() {
			_, err := watch.Lookup(leaderCtx, false, func() (*TransferEscrow, error) {
				<-leaderCtx.Done()
				return nil, leaderCtx.Err()
			})
			leader <- err
		}()
		synctest.Wait()
		want := &TransferEscrow{}
		follower := make(chan *TransferEscrow, 1)
		go func() {
			got, err := watch.Lookup(t.Context(), false, func() (*TransferEscrow, error) { return want, nil })
			if err != nil {
				t.Errorf("leader cancellation escaped to follower: %v", err)
			}
			follower <- got
		}()
		synctest.Wait()
		cancel()
		if err := <-leader; err != context.Canceled {
			t.Fatalf("leader cancellation changed: %v", err)
		}
		if got := <-follower; got != want {
			t.Fatal("independent follower did not create")
		}
		operational := errors.Join(ErrMissingCompanionOrigin, errors.New("synthetic unavailable source"))
		_, err := watch.Lookup(t.Context(), false, func() (*TransferEscrow, error) { return nil, operational })
		if err != operational {
			t.Fatal("operational result changed")
		}
		got, err := watch.Lookup(t.Context(), false, func() (*TransferEscrow, error) { return want, nil })
		if got != want || err != nil {
			t.Fatal("joined operational failure became a cached absence")
		}
	})
}

func TestContractOriginLookupCanceledFollowerDoesNotCancelLeader(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		watch := testContractOriginLookupWatch(t.Context())
		release := make(chan struct{})
		finished := make(chan error, 1)
		go func() {
			_, err := watch.Lookup(t.Context(), false, func() (*TransferEscrow, error) {
				<-release
				return nil, ErrMissingCompanionOrigin
			})
			finished <- err
		}()
		synctest.Wait()
		followerCtx, cancel := context.WithCancel(t.Context())
		follower := make(chan error, 1)
		go func() {
			_, err := watch.Lookup(followerCtx, false, func() (*TransferEscrow, error) {
				t.Error("canceled follower opened a transaction")
				return nil, nil
			})
			follower <- err
		}()
		synctest.Wait()
		cancel()
		if err := <-follower; err != context.Canceled {
			t.Fatalf("follower cancellation changed: %v", err)
		}
		close(release)
		if err := <-finished; err != ErrMissingCompanionOrigin {
			t.Fatalf("follower canceled leader: %v", err)
		}
	})
}

func TestContractOriginLookupPanicReleasesFollower(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		watch := testContractOriginLookupWatch(t.Context())
		release := make(chan struct{})
		finished := make(chan struct{})
		go func() {
			defer close(finished)
			defer func() { _ = recover() }()
			_, _ = watch.Lookup(t.Context(), false, func() (*TransferEscrow, error) {
				<-release
				panic("synthetic lookup failure")
			})
		}()
		synctest.Wait()
		want := &TransferEscrow{}
		follower := make(chan *TransferEscrow, 1)
		go func() {
			got, _ := watch.Lookup(t.Context(), false, func() (*TransferEscrow, error) { return want, nil })
			follower <- got
		}()
		synctest.Wait()
		close(release)
		<-finished
		if got := <-follower; got != want {
			t.Fatal("panic retained or failed another caller")
		}
	})
}

func TestContractOriginLookupRestartAcknowledgementInvalidatesMissing(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		opened := make(chan testContractOriginOpen, contractOriginBucketCount)
		owner := testContractOriginOwner(t.Context(), DefaultContractOriginNotificationSettings(), opened)
		defer owner.Close()
		sourceId, destinationId := server.NewId(), server.NewId()
		watch := owner.Watch(sourceId, destinationId)
		defer watch.Close()
		first := <-opened
		first.subscription.events <- &redis.Subscription{Kind: "ssubscribe", Channel: first.channel}
		synctest.Wait()
		first.subscription.events <- errors.New("synthetic Redis restart")
		second := <-opened
		_, _ = watch.Lookup(t.Context(), false, func() (*TransferEscrow, error) { return nil, ErrMissingCompanionOrigin })
		second.subscription.events <- &redis.Subscription{Kind: "ssubscribe", Channel: second.channel}
		synctest.Wait()
		want := &TransferEscrow{}
		got, err := watch.Lookup(t.Context(), false, func() (*TransferEscrow, error) { return want, nil })
		if got != want || err != nil {
			t.Fatal("reconnected acknowledgement retained pre-ack absence")
		}
		_, _ = watch.Lookup(t.Context(), false, func() (*TransferEscrow, error) { return nil, ErrMissingCompanionOrigin })
		watch.Close()
		replacement := owner.Watch(sourceId, destinationId)
		defer replacement.Close()
		got, err = replacement.Lookup(t.Context(), false, func() (*TransferEscrow, error) { return want, nil })
		if got != want || err != nil {
			t.Fatal("retired pair's absence survived a new registration")
		}
	})
}

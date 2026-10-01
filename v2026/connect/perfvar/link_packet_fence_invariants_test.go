package perfvar

import (
	"context"
	"testing"
	"testing/synctest"
	"time"
)

func TestDirectionalLinkPacketFenceCapturedPrefixAndLaterMaintenance(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx := context.Background()
		requestEntered, releaseRequest := make(chan struct{}), make(chan struct{})
		pingEntered, releasePing := make(chan struct{}), make(chan struct{})
		link := newDirectionalLink(ctx, newLinkProfile(1_000_000_000, 0, 0, 0, time.Millisecond), 17, func(packet []byte) bool {
			if packet[0] == 1 {
				close(requestEntered)
				<-releaseRequest
			} else {
				close(pingEntered)
				<-releasePing
			}
			return true
		})
		defer link.close()
		if err := link.enablePacketFences(); err != nil {
			t.Fatal(err)
		}
		_, _ = link.submit([]byte{1})
		<-requestEntered
		fence, err := link.capturePacketFence()
		if err != nil {
			t.Fatal(err)
		}
		joined := make(chan error, 1)
		go func() { _, err := fence.wait(ctx); joined <- err }()
		_, _ = link.submit([]byte{2}) // later maintenance, same physical link
		synctest.Wait()
		select {
		case err := <-joined:
			t.Fatalf("held request passed fence: %v", err)
		default:
		}
		if _, err := link.capturePacketFence(); err == nil {
			t.Fatal("unfinished captures accumulated")
		}
		close(releaseRequest)
		<-pingEntered
		if err := <-joined; err != nil {
			t.Fatal(err)
		}
		observed := fence.snapshot()
		if observed.StartedSubmissions != 1 || observed.PendingOwners != 0 || observed.PostFenceSubmissions != 1 || observed.PostFenceQueuedPackets != 1 {
			t.Fatalf("prefix/maintenance ownership=%+v", observed)
		}
		// The historical global-idle contract still refuses this exact live ping.
		idleCtx, cancel := context.WithTimeout(ctx, time.Second)
		defer cancel()
		if waitForP2pCarrierQuiescent(idleCtx, []*directionalLink{link}, nil, nil) {
			t.Fatal("canonical/v4 global idle changed")
		}
		close(releasePing)
		if !link.waitIdle(ctx) {
			t.Fatal("released maintenance did not drain")
		}
	})
}

func TestDirectionalLinkPacketFenceIncludesLateDuplicateAndUnpublishedSubmit(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx := context.Background()
		profile := newLinkProfile(1_000_000_000, 0, 0, 0, time.Millisecond)
		profile.DuplicateProbability = 1
		originalDelivered, duplicateEntered, releaseDuplicate := make(chan struct{}), make(chan struct{}), make(chan struct{})
		count := 0
		link := newDirectionalLink(ctx, profile, 23, func([]byte) bool {
			count++
			if count == 1 {
				close(originalDelivered)
			} else {
				close(duplicateEntered)
				<-releaseDuplicate
			}
			return true
		})
		defer link.close()
		if err := link.enablePacketFences(); err != nil {
			t.Fatal(err)
		}
		beforeIngress, releaseIngress := make(chan struct{}), make(chan struct{})
		link.beforeIngressForTest = func() { close(beforeIngress); <-releaseIngress }
		submitted := make(chan struct{})
		go func() { _, _ = link.submit([]byte{1}); close(submitted) }()
		<-beforeIngress
		fence, err := link.capturePacketFence()
		if err != nil {
			t.Fatal(err)
		}
		if got := fence.snapshot(); got.PendingOwners != 2 {
			t.Fatalf("submit plus queue owner=%+v", got)
		}
		close(releaseIngress)
		<-submitted
		<-originalDelivered
		<-duplicateEntered
		joined := make(chan error, 1)
		go func() { _, err := fence.wait(ctx); joined <- err }()
		synctest.Wait()
		select {
		case err := <-joined:
			t.Fatalf("late duplicate escaped captured prefix: %v", err)
		default:
		}
		if got := fence.snapshot(); got.Duplicates != 1 || got.PendingOwners != 1 {
			t.Fatalf("duplicate ownership=%+v", got)
		}
		close(releaseDuplicate)
		if err := <-joined; err != nil {
			t.Fatal(err)
		}
	})
}

func TestDirectionalLinkPacketFenceTerminalFailuresAndCancellation(t *testing.T) {
	for _, kind := range []string{"queue", "mtu", "receiver", "cancel", "allowed-loss"} {
		t.Run(kind, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx := context.Background()
				profile := newLinkProfile(1_000_000_000, 0, 0, 0, time.Millisecond)
				packet := []byte{1}
				switch kind {
				case "queue":
					profile.QueuePacketCount = 0
				case "mtu":
					packet = make([]byte, profile.OuterMtu+1)
				case "cancel":
					profile.BaseDelay = time.Hour
				case "allowed-loss":
					profile.LossModel, profile.DropEveryPacketCount = lossModelEveryN, 1
				}
				link := newDirectionalLink(ctx, profile, 29, func([]byte) bool { return kind != "receiver" })
				defer link.close()
				if err := link.enablePacketFences(); err != nil {
					t.Fatal(err)
				}
				_, _ = link.submit(packet)
				fence, err := link.capturePacketFence()
				if err != nil {
					t.Fatal(err)
				}
				if kind == "cancel" {
					link.close()
				}
				observation, err := fence.wait(ctx)
				if (err == nil) != (kind == "allowed-loss") || observation.PendingOwners != 0 {
					t.Fatalf("kind=%s observation=%+v err=%v", kind, observation, err)
				}
				cancelCtx, cancel := context.WithCancel(ctx)
				cancel()
				if _, err := fence.wait(cancelCtx); err == nil {
					t.Fatal("caller cancellation was hidden")
				}
			})
		})
	}
}

func TestDirectionalLinkPacketFenceOptInAndBound(t *testing.T) {
	link := newDirectionalLink(context.Background(), newLinkProfile(1_000_000_000, 0, 0, 0, time.Millisecond), 31, func([]byte) bool { return true })
	defer link.close()
	if _, err := link.capturePacketFence(); err == nil || link.fenceCurrent != nil {
		t.Fatal("canonical link silently enabled scoped fences")
	}
	if err := link.enablePacketFences(); err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 1000; i++ {
		fence, err := link.capturePacketFence()
		if err != nil {
			t.Fatal(err)
		}
		got, err := fence.wait(context.Background())
		if err != nil || got.Epoch != uint64(i) || got.PendingOwners != 0 || link.fencePrevious != fence.epoch {
			t.Fatalf("bounded epoch=%+v err=%v", got, err)
		}
	}
	if err := link.enablePacketFences(); err == nil {
		t.Fatal("second enable erased ownership")
	}
}

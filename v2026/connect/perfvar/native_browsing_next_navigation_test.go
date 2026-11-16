// Revised pressure keeps one original image unfinished at the next HTML
// handoff. Its completion gate is independent of page-retirement ownership.
package perfvar

import (
	"context"
	"errors"
	"fmt"
	"sync"
)

// Nil disables the revised workload. Both notifications are safe to repeat;
// readiness means either positive client body progress or a finished fetch.
// No body, request or connection is retained by this fixed per-page state.
type nativeBrowsingNextNavigationBody struct {
	released         chan struct{}
	releaseOnce      sync.Once
	bodyProgress     chan struct{}
	bodyProgressOnce sync.Once
}

// All state is installed before any request starts, including receipt pointers.
// Only pages with a following real navigation receive a completion gate.
func (self nativeBrowsingRunSettings) newPages() [nativeBrowsingPageCount]*nativeBrowsingPageState {
	var pages [nativeBrowsingPageCount]*nativeBrowsingPageState
	if self.pressure != nil {
		self.pressure.WorkloadVersion = "streaming-v1"
		if self.nextNavigationOverlap {
			self.pressure.WorkloadVersion = "next-navigation-image-v2"
		}
	}
	for page := range pages {
		state := newNativeBrowsingPageState()
		pages[page] = state
		if self.pressure != nil {
			for asset := range nativeBrowsingAssetCount {
				self.pressure.assets[page][asset].serverReceipt = state.serverReceipts[asset]
			}
		}
		if self.nextNavigationOverlap && page+1 < nativeBrowsingPageCount {
			state.nextNavigation = &nativeBrowsingNextNavigationBody{released: make(chan struct{}), bodyProgress: make(chan struct{})}
			self.pressure.assets[page][2].nextNavigation = state.nextNavigation
		}
	}
	return pages
}

// Release occurs only at the next HTML handoff or final owner cleanup.
func (self *nativeBrowsingNextNavigationBody) release() {
	if self != nil {
		self.releaseOnce.Do(func() { close(self.released) })
	}
}

// A failed fetch also wakes the owner so an empty body cannot park it forever.
func (self *nativeBrowsingNextNavigationBody) signalBodyProgress() {
	if self != nil {
		self.bodyProgressOnce.Do(func() { close(self.bodyProgress) })
	}
}

// Positive received bytes and a withheld final fragment identify an actual
// original HTTP body, rather than a queued fetch or a historical prefix token.
func (self *nativeBrowsingNextNavigationBody) checkBodyProgress(progress *nativeBrowsingRequestProgress) error {
	if self == nil || progress.nextNavigation != self {
		return errors.New("missing next-navigation image completion gate")
	}
	select {
	case <-self.released:
		return errors.New("next-navigation image was released before its handoff")
	default:
	}
	received := progress.receivedBytes.Load()
	receipt, known := progress.serverOwnership()
	if !progress.started.Load() || !progress.bodyOpened.Load() || progress.finished.Load() ||
		received <= 0 || progress.expectedBytes <= received || !known || receipt.started <= receipt.finished {
		return fmt.Errorf("original image body is not unfinished: received=%d expected=%d finished=%t server_started=%d server_finished=%d",
			received, progress.expectedBytes, progress.finished.Load(), receipt.started, receipt.finished)
	}
	return nil
}

// The driver's existing prefix receipts precede this wait. No timer, retry or
// second request is introduced; cancellation retains the original owner.
func (self *nativeBrowsingNextNavigationBody) waitForBodyProgress(ctx context.Context, progress *nativeBrowsingRequestProgress) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-self.bodyProgress:
		return self.checkBodyProgress(progress)
	}
}

// Called at the original HTML handoff, after all pre-tap barriers and sampling.
// Snapshot the live original body before releasing its tail and performing the
// original even-page cancellations; the caller immediately starts that HTML.
func (self nativeBrowsingRunSettings) startPressureNavigation(observation *nativeBrowsingObservation, pages *[nativeBrowsingPageCount]*nativeBrowsingPageState, page int) (*nativeBrowsingHttpTrace, error) {
	if self.nextNavigationOverlap && page != 0 {
		if err := pages[page-1].nextNavigation.checkBodyProgress(&self.pressure.assets[page-1][2]); err != nil {
			return nil, fmt.Errorf("page %d body overlap: %w", page, err)
		}
	}
	trace := self.pressure.beginNavigation(page, self.maxLivePages)
	if self.pressure.Navigations[page].UnfinishedPriorAssets != 0 {
		observation.OverlapProofs++
	}
	if page != 0 {
		pages[page-1].nextNavigation.release()
		if (page-1)%2 == 0 {
			for asset := 2; asset < nativeBrowsingAssetCount; asset++ {
				self.pressure.assets[page-1][asset].cancelRequested.Store(true)
				self.pressure.CancellationAttempts++
				pages[page-1].assetCancel[asset]()
			}
		}
	}
	return trace, nil
}

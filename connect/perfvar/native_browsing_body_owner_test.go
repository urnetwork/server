// Shared test-only request ownership checks do not wait for body progress.
package perfvar

import (
	"context"
	"errors"
	"sync/atomic"
)

// One exact original trace owns its context/slot/gate. Transport GetConn may
// repeat for the same HTTP request; it only marks dispatch idempotently.
type nativeBrowsingBodyRequestOwner struct {
	ctx        context.Context
	gate       *nativeBrowsingHttp2BodyGate
	target     string
	dispatched atomic.Bool
}

func nativeBrowsingBodyRequestDispatched(owner *nativeBrowsingBodyRequestOwner, gate *nativeBrowsingHttp2BodyGate, target string) error {
	if owner == nil || owner.ctx == nil || owner.ctx.Err() != nil || owner.gate != gate || owner.target != target || !owner.dispatched.Load() {
		return errors.New("original HTTP request has no live exact Transport dispatch owner")
	}
	return nil
}

func nativeBrowsingUnfinishedBodyOwner(gate *nativeBrowsingHttp2BodyGate, progress *nativeBrowsingRequestProgress, expected int) error {
	if gate == nil || progress == nil {
		return errors.New("original response owner is missing")
	}
	select {
	case <-gate.released:
		return errors.New("original response owner released before next HTML")
	default:
	}
	received := progress.receivedBytes.Load()
	if !progress.started.Load() || progress.cancelRequested.Load() || progress.finished.Load() || progress.bodyClosed.Load() ||
		progress.expectedBytes != int64(expected) || expected <= nativeBrowsingHeldPrefixBytes ||
		received < 0 || nativeBrowsingHeldPrefixBytes < received || progress.expectedBytes <= received {
		return errors.New("original HTTP request/body is not unfinished with its expected unread remainder")
	}
	return nil
}

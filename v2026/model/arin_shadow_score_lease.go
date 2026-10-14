package model

import (
	"context"
	"time"

	"github.com/urnetwork/server/v2026"
)

// A capture pins the immutable generation that was actually current when it
// began. Normal 30-second publication does not mutate that generation. Only
// one lease may retain an older snapshot per installed observer, for <=90s;
// there is no copy of its population and no lock held during capture work.
type ArinShadowScoreLease struct {
	owner                *ArinShadowScoreCapture
	snapshot             *ArinShadowScoreSnapshot
	token                server.Id
	startedAt, expiresAt time.Time
	timer                *time.Timer
}

func (s *ArinShadowScoreSnapshot) AcquireCaptureLease(ctx context.Context) (*ArinShadowScoreLease, error) {
	expires := server.NowUtc().Add(server.ArinShadowCaptureMaxAge)
	if ctx != nil {
		if deadline, ok := ctx.Deadline(); ok && deadline.Before(expires) {
			expires = deadline
		}
	}
	return s.acquireCaptureLease(ctx, expires)
}

func (s *ArinShadowScoreSnapshot) acquireCaptureLease(ctx context.Context, expires time.Time) (*ArinShadowScoreLease, error) {
	if !s.Validate(ctx) {
		return nil, server.ErrArinShadowInput
	}
	c := s.owner
	c.mu.Lock()
	defer c.mu.Unlock()
	now := server.NowUtc()
	if c.closed || c.current != s || currentArinShadowScoreCapture.Load() != c || ctx.Err() != nil {
		return nil, server.ErrArinShadowInput
	}
	if c.lease != nil {
		if now.Before(c.lease.expiresAt) {
			return nil, server.ErrArinShadowInput
		}
		c.lease.snapshot = nil
	}
	if !expires.After(now) || expires.After(now.Add(server.ArinShadowCaptureMaxAge)) {
		return nil, server.ErrArinShadowInput
	}
	lease := &ArinShadowScoreLease{owner: c, snapshot: s, token: server.NewId(), startedAt: now, expiresAt: expires}
	c.lease = lease
	lease.timer = time.AfterFunc(expires.Sub(now), lease.Close)
	return lease, nil
}

func (l *ArinShadowScoreLease) current(ctx context.Context) (*ArinShadowScoreSnapshot, bool) {
	if l == nil || ctx == nil || ctx.Err() != nil {
		return nil, false
	}
	l.owner.mu.Lock()
	defer l.owner.mu.Unlock()
	now := server.NowUtc()
	if l.owner.closed || currentArinShadowScoreCapture.Load() != l.owner || l.owner.lease != l ||
		l.snapshot == nil || now.Before(l.startedAt) || !now.Before(l.expiresAt) {
		if l.owner.lease == l {
			l.owner.lease = nil
			l.snapshot = nil
		}
		return nil, false
	}
	return l.snapshot, true
}

func (l *ArinShadowScoreLease) Close() {
	if l == nil {
		return
	}
	l.owner.mu.Lock()
	defer l.owner.mu.Unlock()
	if l.owner.lease == l {
		l.owner.lease = nil
	}
	if l.timer != nil {
		l.timer.Stop()
	}
	l.snapshot = nil
}

package oauth

// Periodic removal of expired authorization codes and refresh tokens.
//
// Both tables are append-only during normal operation: a redeemed code and a
// rotated refresh token are deliberately KEPT until they expire, because that
// is what makes a replay detectable rather than indistinguishable from an
// unknown token. Once expired they carry no information, so they are deleted.
//
// Follows the proxy's periodic maintenance shape (server/proxy): a goroutine
// under `server.HandleError` that selects on ctx.Done and a timeout, started by
// the constructor so the returned object is already running.
//
// Running in every api replica is intentional and safe: the delete is a bounded
// idempotent range over an indexed expire_time, so a replica that loses the
// race simply deletes nothing.

import (
	"context"
	"time"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
)

func DefaultReaperSettings() *ReaperSettings {
	return &ReaperSettings{
		// codes live a minute and refresh tokens ninety days, so nothing here
		// is urgent; this is about the table not growing without bound
		ReapTimeout: 30 * time.Minute,
		// spread the first sweep so a simultaneous deploy of every replica
		// does not run them all at once
		InitialReapTimeout: 5 * time.Minute,
	}
}

type ReaperSettings struct {
	ReapTimeout        time.Duration
	InitialReapTimeout time.Duration
}

type Reaper struct {
	ctx      context.Context
	settings *ReaperSettings
}

func NewReaperWithDefaults(ctx context.Context) *Reaper {
	return NewReaper(ctx, DefaultReaperSettings())
}

func NewReaper(ctx context.Context, settings *ReaperSettings) *Reaper {
	self := &Reaper{
		ctx:      ctx,
		settings: settings,
	}

	go server.HandleError(self.run)

	return self
}

func (self *Reaper) run() {
	timeout := self.settings.InitialReapTimeout

	for {
		select {
		case <-self.ctx.Done():
			return
		case <-time.After(timeout):
		}
		timeout = self.settings.ReapTimeout

		codeCount, refreshTokenCount := ReapOauthTokens(self.ctx)
		// only report a sweep that removed something, so the log shows real
		// churn rather than a heartbeat
		if 0 < codeCount || 0 < refreshTokenCount {
			glog.Infof(
				"[oauth]reaped %d expired codes, %d expired refresh tokens\n",
				codeCount,
				refreshTokenCount,
			)
		}
	}
}

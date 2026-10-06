package model

import (
	"context"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/internal/privateprovidercapture"
)

// One fixed-size request-local tuple. Only the reader's actual missing GROUP
// can populate it; a location or arbitrary first request spec cannot substitute.
type findProviders2MissingGroup struct {
	group  server.Id
	caller server.Id
	rank   RankMode
	source string
}

func (self *findProviders2LoadObservation) missingTarget(group *server.Id, caller server.Id, rank RankMode) {
	self.missingTargets++
	if self.privateCapture != nil && group != nil && self.missingGroup == nil {
		self.missingGroup = &findProviders2MissingGroup{group: *group, caller: caller, rank: rank, source: self.privateSource}
	}
}

// The API attaches its private owner only to the natural HTTP route after a
// root-authenticated arm. Failed/healthy/mixed/intentional requests retain no
// tuple beyond their normal request lifetime and never enter the private ring.
func (self *findProviders2SelectionObservation) beginPrivateCapture(ctx context.Context) {
	if self.targetKind != "group" || self.requestClass != "default_minimum" {
		return
	}
	if self.rankMode != RankModeQuality && self.rankMode != RankModeSpeed {
		return
	}
	family := findProviders2OutcomeIpFamily(self.args.IpFamily)
	if family != "any" && family != "v4" && family != "dualstack" {
		return
	}
	self.load.privateCapture = privateprovidercapture.FromContext(ctx)
	if self.load.privateCapture != nil {
		self.privateStarted = time.Now()
	}
}

func (self *findProviders2SelectionObservation) finishPrivateCapture(ctx context.Context) {
	missing := self.load.missingGroup
	if self.load.privateCapture == nil || missing == nil || ctx.Err() != nil || self.outcome != "zero" || self.reason != "cache_missing" || self.targetKind != "group" || self.requestClass != "default_minimum" {
		return
	}
	self.load.privateCapture.Offer(privateprovidercapture.Record{
		StartedUTC: self.privateStarted,
		GroupID:    missing.group.String(), CallerLocationID: missing.caller.String(),
		RequestRank: self.rankMode, LoadRank: missing.rank, Source: missing.source,
		IPFamily:        findProviders2OutcomeIpFamily(self.args.IpFamily),
		RequestedGroups: self.privateGroupCount, MissingTargets: self.load.missingTargets,
	})
}

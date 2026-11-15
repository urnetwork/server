package dnshost

// dnsservice.DnsCustomHostMapStore over model's custom-host map functions
// (DNS-M6-03; §13.4): the map's versions, each lb interface's report and route
// probes, and each custom host's readiness, for the dns API block's map routes
// and its feed. Safe for concurrent use.

import (
	"context"
	"time"

	"github.com/urnetwork/sidecar/dnsservice"

	"github.com/urnetwork/server/model"
)

var _ dnsservice.DnsCustomHostMapStore = (*CustomHostMaps)(nil)

// The store. It holds nothing.
type CustomHostMaps struct{}

// The store over model.
func NewCustomHostMaps() *CustomHostMaps {
	return &CustomHostMaps{}
}

// model.GetDnsCustomHostMap.
func (self *CustomHostMaps) CustomHostMap(ctx context.Context, root string) (customHostMap *dnsservice.DnsCustomHostMap, returnErr error) {
	returnErr = storeCall(func() error {
		var err error
		customHostMap, err = model.GetDnsCustomHostMap(ctx, root)
		return err
	})
	return
}

// model.GetDnsCustomHostMapState.
func (self *CustomHostMaps) CustomHostMapState(ctx context.Context, root string) (state *dnsservice.DnsMapState, returnErr error) {
	returnErr = storeCall(func() error {
		var err error
		state, err = model.GetDnsCustomHostMapState(ctx, root)
		return err
	})
	return
}

// model.RecordDnsCustomHostMapPoll.
func (self *CustomHostMaps) RecordMapPoll(ctx context.Context, root string, poll *dnsservice.DnsMapPoll) error {
	return storeCall(func() error {
		return model.RecordDnsCustomHostMapPoll(ctx, poll)
	})
}

// model.RecordDnsCustomHostMapReport.
func (self *CustomHostMaps) RecordMapReport(ctx context.Context, root string, report *dnsservice.DnsCustomHostMapReport, now time.Time, routeHostCount int, droppedMaxCount int) error {
	return storeCall(func() error {
		return model.RecordDnsCustomHostMapReport(ctx, report, now, routeHostCount, droppedMaxCount)
	})
}

// model.RecordDnsRouteProbes.
func (self *CustomHostMaps) RecordRouteProbes(ctx context.Context, root string, report *dnsservice.DnsRouteProbeReport, now time.Time) error {
	return storeCall(func() error {
		return model.RecordDnsRouteProbes(ctx, report, now)
	})
}

// model.SyncDnsCustomHostMapInterfaces.
func (self *CustomHostMaps) SyncMapInterfaces(ctx context.Context, root string, names []string) error {
	return storeCall(func() error {
		return model.SyncDnsCustomHostMapInterfaces(ctx, names)
	})
}

// model.SetDnsCustomHostReadiness.
func (self *CustomHostMaps) SetCustomHostReadiness(ctx context.Context, root string, ready map[uint64]bool, now time.Time) error {
	return storeCall(func() error {
		return model.SetDnsCustomHostReadiness(ctx, root, ready, now)
	})
}

package dnshost

// §12.6's restore step on Postgres (DNS-M5-15): RestoreStore is what
// dnsservice.RestoreStep reads and writes beyond Store, and NewRestoreStep
// builds the step a §19.10 runbook command or OPS's restore frame runs, needing
// neither api nor the token exchange.
//
// dns_ns waits for its §2.8 row (plan-DNS-metadata-rows #7, v9), so until it
// lands no NS is ever stopped here: StoppedNs reads none and StopNs refuses with
// dnsservice.ErrDnsNsStateUnavailable, and the step then counts every NS dns.yml
// lists until each serves the restore serial, plan-DNS-running-ns's interim and
// the closed direction.

import (
	"context"
	"errors"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/dnsservice"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// dnsservice.DnsRestoreStore on Postgres. Safe for concurrent use.
type RestoreStore struct{}

var (
	_ dnsservice.DnsRestoreStore    = (*RestoreStore)(nil)
	_ dnsservice.DnsRestoreMapStore = (*RestoreStore)(nil)
)

// No NS is stopped until dns_ns exists.
func (self *RestoreStore) StoppedNs(ctx context.Context, root string) ([]string, error) {
	if !server.SidecarMg5Linked() {
		return nil, model.ErrDnsUnlinked
	}
	return nil, nil
}

// Refuses until dns_ns exists, so the step keeps counting ns.
func (self *RestoreStore) StopNs(ctx context.Context, root string, ns string, reason string) error {
	return dnsservice.ErrDnsNsStateUnavailable
}

// Refuses until dns_ns exists; nothing is stopped to start.
func (self *RestoreStore) StartNs(ctx context.Context, root string, ns string) error {
	return dnsservice.ErrDnsNsStateUnavailable
}

// model.ListDnsRestoreChanges at the server's now.
func (self *RestoreStore) ListRestoreChanges(ctx context.Context, root string, incidentId sidecar.Id, keys []dnsservice.DnsRecordKey) (listing *dnsservice.DnsRestoreListing, returnErr error) {
	returnErr = storeCall(func() error {
		var err error
		listing, err = model.ListDnsRestoreChanges(ctx, root, server.Id(incidentId), keys, server.NowUtc())
		return err
	})
	return
}

// model.DnsRaiseCustomHostMapForRestore at the server's now (DNS-M6-07).
func (self *RestoreStore) RaiseCustomHostMapForRestore(ctx context.Context, root string, incidentId sidecar.Id) (version uint64, returnErr error) {
	returnErr = storeCall(func() error {
		var err error
		version, err = model.DnsRaiseCustomHostMapForRestore(ctx, root, server.Id(incidentId), server.NowUtc())
		return err
	})
	return
}

// model.GetDnsCustomHostMapState: each interface's report.
func (self *RestoreStore) CustomHostMapState(ctx context.Context, root string) (state *dnsservice.DnsMapState, returnErr error) {
	returnErr = storeCall(func() error {
		var err error
		state, err = model.GetDnsCustomHostMapState(ctx, root)
		return err
	})
	return
}

// model.ListDnsRestoreCustomHosts at the server's now.
func (self *RestoreStore) ListRestoreCustomHosts(ctx context.Context, root string, incidentId sidecar.Id, added []string, dropped []string) (listing *dnsservice.DnsRestoreListing, returnErr error) {
	returnErr = storeCall(func() error {
		var err error
		listing, err = model.ListDnsRestoreCustomHosts(ctx, root, server.Id(incidentId), added, dropped, server.NowUtc())
		return err
	})
	return
}

// The restore step over Store and RestoreStore, reading each NS's SOA over TCP
// at dns.yml's glue, with the custom-host map's half (DNS-M6-07). clock nil is
// the wall clock.
func NewRestoreStep(store dnsservice.DnsStore, settings *dnsservice.DnsSettings, clock sidecar.Clock, log sidecar.Logger) (*dnsservice.RestoreStep, error) {
	if store == nil {
		return nil, errors.New("dns restore: no store")
	}
	return dnsservice.NewRestoreStep(settings, &dnsservice.RestoreStepDeps{
		Store:          store,
		Restore:        &RestoreStore{},
		SoaReader:      dnsservice.NewDnsTcpSoaReader(hostNsAddresses(settings)),
		CustomHostMaps: &RestoreStore{},
		Clock:          clock,
		Log:            log,
	})
}

package dnshost

// §12.6's restore step on Postgres (DNS-M5-15): RestoreStore is what
// dnsservice.RestoreStep reads and writes beyond Store, and NewRestoreStep
// builds the step a §19.10 runbook command or OPS's restore frame runs, needing
// neither api nor the token exchange.

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

// The stopped names and both state transitions persist across block restarts.
func (self *RestoreStore) StoppedNs(ctx context.Context, root string) (names []string, returnErr error) {
	returnErr = storeCall(func() error {
		var err error
		names, err = model.GetStoppedDnsNameServers(ctx, root)
		return err
	})
	return
}

func (self *RestoreStore) StopNs(ctx context.Context, root string, ns string, reason string) error {
	return storeCall(func() error { return model.SetDnsNameServerStopped(ctx, root, ns, reason, server.NowUtc()) })
}

func (self *RestoreStore) StartNs(ctx context.Context, root string, ns string) error {
	return storeCall(func() error { return model.SetDnsNameServerStopped(ctx, root, ns, "", server.NowUtc()) })
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

// The current map state, including the bounded sample used by readiness probes.
func (self *RestoreStore) CustomHostMapState(ctx context.Context, root string) (state *dnsservice.DnsMapState, returnErr error) {
	returnErr = storeCall(func() error {
		var err error
		state, err = model.GetDnsCustomHostMapState(ctx, root)
		return err
	})
	return
}

// The complete receipts for this incident, independently of the current probe samples.
func (self *RestoreStore) RestoreMapReports(ctx context.Context, root string, incidentId sidecar.Id) (reports map[string]*dnsservice.DnsMapRestoreReport, returnErr error) {
	returnErr = storeCall(func() error {
		var err error
		reports, err = model.GetDnsRestoreMapReports(ctx, root, server.Id(incidentId))
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

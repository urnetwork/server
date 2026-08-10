package controller

import (
	"context"
	"fmt"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// AddSampleEventsForTesting writes FAKE sample rows ("Palo Alto", random ids)
// into the audit_provider_event and audit_contract_event feeds so the
// /stats/last-90 pipeline can be exercised in a development environment.
//
// It must never run against a production database: these feeds are aggregated
// into the public stats by ComputeStats90, and sample rows are
// indistinguishable from real data to the aggregation (fake rows were what
// made the public feed report a single "Palo Alto" city). Real events come
// from model.SweepProviderAuditEvents / model.RollupTransferAuditEvents.
// The env allowlist below refuses everything except local/test, and every
// sample row is provenance-marked so `bringyourctl stats purge-samples` can
// remove it.
func AddSampleEventsForTesting(ctx context.Context, intervalSeconds int) error {
	env, err := server.Env()
	if err != nil {
		return fmt.Errorf("refusing to add sample audit events: %w", err)
	}
	switch env {
	case "local", "test":
	default:
		return fmt.Errorf(
			"refusing to add FAKE sample audit events in env %q: sample data poisons the public stats feed (allowed envs: local, test)",
			env,
		)
	}

	sampleDetails := model.AuditEventDetailsSample

	auditProviderEvent := model.NewAuditProviderEvent(model.AuditEventTypeProviderOnlineSuperspeed)
	auditProviderEvent.NetworkId = server.NewId()
	auditProviderEvent.DeviceId = server.NewId()
	auditProviderEvent.EventDetails = &sampleDetails
	countryName := "United States"
	regionName := "California"
	cityName := "Palo Alto"
	auditProviderEvent.CountryName = countryName
	auditProviderEvent.RegionName = regionName
	auditProviderEvent.CityName = cityName
	model.AddAuditEvent(ctx, auditProviderEvent)

	auditContractEvent := model.NewAuditContractEvent(model.AuditEventTypeContractClosedSuccess)
	auditContractEvent.ContractId = server.NewId()
	auditContractEvent.ClientNetworkId = server.NewId()
	auditContractEvent.ClientDeviceId = server.NewId()
	auditContractEvent.ProviderNetworkId = server.NewId()
	auditContractEvent.ProviderDeviceId = server.NewId()
	auditContractEvent.ExtenderNetworkId = nil
	auditContractEvent.ExtenderId = nil
	auditContractEvent.EventDetails = &sampleDetails
	auditContractEvent.TransferBytes = 1024 * 1024 * 1024
	auditContractEvent.TransferPackets = 1024 * 1024 * 1024 / 1500
	model.AddAuditEvent(ctx, auditContractEvent)

	return nil
}

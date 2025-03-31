package controller

import (
	"context"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

func AddSampleEvents(ctx context.Context, intervalSeconds int) {
	// random events to have these rate targets

	auditProviderEvent := model.NewAuditProviderEvent(model.AuditEventTypeProviderOnlineSuperspeed)
	auditProviderEvent.NetworkId = server.NewId()
	auditProviderEvent.DeviceId = server.NewId()
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
	auditContractEvent.TransferBytes = 1024 * 1024 * 1024
	auditContractEvent.TransferPackets = 1024 * 1024 * 1024 / 1500
	model.AddAuditEvent(ctx, auditContractEvent)

}

package controller

import (
	"context"

	"bringyour.com/bringyour"
	"bringyour.com/bringyour/model"
)


func AddSampleEvents(ctx context.Context, intervalSeconds int) {
	// random events to have these rate targets

	auditProviderEvent := model.NewAuditProviderEvent(model.AuditEventTypeProviderOnlineSuperspeed)
	auditProviderEvent.NetworkId = bringyour.NewId()
	auditProviderEvent.DeviceId = bringyour.NewId()
	countryName := "United States"
	regionName := "California"
	cityName := "Palo Alto"
	auditProviderEvent.CountryName = countryName
	auditProviderEvent.RegionName = regionName
	auditProviderEvent.CityName = cityName
	model.AddAuditEvent(ctx, auditProviderEvent)


	auditContractEvent := model.NewAuditContractEvent(model.AuditEventTypeContractClosedSuccess)
	auditContractEvent.ContractId = bringyour.NewId()
	auditContractEvent.ClientNetworkId = bringyour.NewId()
	auditContractEvent.ClientDeviceId = bringyour.NewId()
	auditContractEvent.ProviderNetworkId = bringyour.NewId()
	auditContractEvent.ProviderDeviceId = bringyour.NewId()
	auditContractEvent.ExtenderNetworkId = nil
	auditContractEvent.ExtenderId = nil
	auditContractEvent.TransferBytes = 1024 * 1024 * 1024
	auditContractEvent.TransferPackets = 1024 * 1024 * 1024 / 1500
	model.AddAuditEvent(ctx, auditContractEvent)

}


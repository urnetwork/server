package controller

import (

	// "bringyour.com/bringyour"
	"bringyour.com/bringyour/ulid"
	"bringyour.com/bringyour/model"
)


func AddSampleEvents(intervalSeconds int) {
	// random events to have these rate targets



	auditProviderEvent := model.NewAuditProviderEvent(model.AuditEventTypeProviderOnlineSuperspeed)
	auditProviderEvent.NetworkId = ulid.Make()
	auditProviderEvent.DeviceId = ulid.Make()
	countryName := "United States"
	regionName := "California"
	cityName := "Palo Alto"
	auditProviderEvent.CountryName = countryName
	auditProviderEvent.RegionName = regionName
	auditProviderEvent.CityName = cityName
	model.AddAuditEvent(auditProviderEvent)


	auditContractEvent := model.NewAuditContractEvent(model.AuditEventTypeContractClosedSuccess)
	auditContractEvent.ContractId = ulid.Make()
	auditContractEvent.ClientNetworkId = ulid.Make()
	auditContractEvent.ClientDeviceId = ulid.Make()
	auditContractEvent.ProviderNetworkId = ulid.Make()
	auditContractEvent.ProviderDeviceId = ulid.Make()
	auditContractEvent.ExtenderNetworkId = nil
	auditContractEvent.ExtenderId = nil
	auditContractEvent.TransferBytes = 1024 * 1024 * 1024
	auditContractEvent.TransferPackets = 1024 * 1024 * 1024 / 1500
	model.AddAuditEvent(auditContractEvent)

}


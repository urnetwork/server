



func ConnectNetworkClient(clientId Id, ipStr string) Id {
	connectionId := model.ConnectNetworkClient(clientId, ipStr)
	go setConnectionLocation(clientId, connectionId, ipStr)
	return connectionId
}


func setConnectionLocation(clientId Id, connectionId Id, ipStr string) {
	// ipinfo to find location
	// update model


	// city (region, country)
	// region (county)
	// country
	// group name
	
	// location_model create city entry
	// location_model create region entry
	// location_model create country entry
	// store city_location_id, region_location_id, country_location_id for each entry
	
	// each location_group_id is also a location_id, stored as a group type
	// store map of location_id, location_group_id for each entry for any affiliated groups
	// store groups in a separate search index

	// there can be 0 or more location groups per location


	// framework are hard coded (can be null if the country is not part of a group):
	// European Union (EU)
	// Nordic
	// Strong Privacy (EU+Nordic+california)


	locationModel.FormatLocation(completeLocation)
	// returns city, region, country as complete as possible
	locationModel.ParseLocation(name)

	// for each group (including null),
	model.SetConnectionLocation(connectionId, clientId, city_locationId, region_locationId, country_locationId)
	// matches any of city, region, or country, or location_group
	model.GetClientIdsForLocation(locationId)
	// for any groups,
	model.GetClientIdsForLocationGroup(locationGroupId)



	// update location search index for city, region, country strings




}


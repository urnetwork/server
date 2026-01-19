package main

import "github.com/urnetwork/connect"

type FindProvidersArgs struct {
	LocationId       *connect.Id `json:"location_id,omitempty"`
	LocationGroupId  *connect.Id `json:"location_group_id,omitempty"`
	Count            int         `json:"count"`
	ExcludeClientIds *IdList     `json:"exclude_location_ids,omitempty"`
}

type IdList struct {
	exportedList[*connect.Id]
}

type FindProvidersResult struct {
	ClientIds []*connect.Id `json:"client_ids,omitempty"`
}

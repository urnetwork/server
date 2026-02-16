package main

import (
	"context"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/urnetwork/glog"
	"github.com/urnetwork/server/model"
)

// register tools with the MCP server.
func registerTools(server *mcp.Server) {

	mcp.AddTool(server, &mcp.Tool{
		Name:        "providerLocations",
		Description: "Get available URnetwork VPN provider locations",
	}, getProviderLocations,
	)
}

const (
	MsgNoProviderLocations    = "No provider locations found"
	MsgFoundProviderLocations = "Found %d provider locations"
	MsgFoundLocationGroups    = "Found %d provider location groups"
	MsgFoundProviderDevices   = "Found %d provider devices"
)

type LocationEntry struct {
	LocationId    string             `json:"location_id"`
	LocationType  model.LocationType `json:"location_type"`
	Name          string             `json:"name"`
	ProviderCount int                `json:"provider_count"`
	MatchDistance int                `json:"match_distance,omitempty"`
	Stable        bool               `json:"stable"`
	StrongPrivacy bool               `json:"strong_privacy"`
}

type FindLocationsArgs struct {
	Query string `json:"query" jsonschema:"Location name to search for (e.g. 'New York', 'Tokyo', 'US East'). Supports fuzzy matching. An empty query returns a list of available countries."`
}

func getProviderLocations(ctx context.Context, req *mcp.CallToolRequest, findLocations FindLocationsArgs) (*mcp.CallToolResult, any, error) {

	clientSession, err := createClientSession(ctx, req)
	if err != nil {
		glog.Infof("Error creating client session: %v", err)
		return nil, nil, err
	}

	result, err := model.FindProviderLocations(&model.FindLocationsArgs{
		Query: findLocations.Query,
	}, clientSession)

	locationEntries := make([]*LocationEntry, 0, len(result.Locations))

	for _, loc := range result.Locations {
		// flatten location info
		entry := &LocationEntry{
			LocationId:    loc.LocationId.String(),
			LocationType:  loc.LocationType,
			Name:          loc.Name,
			ProviderCount: loc.ProviderCount,
			MatchDistance: loc.MatchDistance,
			Stable:        loc.Stable,
			StrongPrivacy: loc.StrongPrivacy,
		}
		locationEntries = append(locationEntries, entry)
	}

	if err != nil {
		glog.Infof("Error finding provider locations: %v", err)
		return nil, nil, err
	}

	text := ""

	if len(locationEntries) > 0 {
		text += fmt.Sprintf(MsgFoundProviderLocations, len(locationEntries))
	} else {
		text += MsgNoProviderLocations
	}

	return &mcp.CallToolResult{
		Content: []mcp.Content{
			&mcp.TextContent{Text: text},
		},
	}, locationEntries, nil
}

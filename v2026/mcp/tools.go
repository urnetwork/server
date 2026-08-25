package mcp

// mcp tool registrations and handlers.
//
// Handlers are stateless: each call builds its own `ClientSession` from the
// verified access token (see auth.go) and releases it when the call returns.
// Each tool also checks the scope it needs -- the transport only enforces the
// read scope, which is the floor.
//
// Errors returned from a handler are embedded verbatim in the tool result by
// the sdk, so handlers log internal error detail and return a generic
// client-facing message instead.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/urnetwork/glog/v2026"

	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/oauth"
)

// Registers all tools with the mcp server.
func registerTools(mcpServer *mcpsdk.Server) {
	// the connectors directory requires a title and the applicable
	// read-only/destructive annotation on every tool
	mcpsdk.AddTool(mcpServer, &mcpsdk.Tool{
		Name:        "providerLocations",
		Title:       "Find provider locations",
		Description: "Get available URnetwork VPN provider locations",
		Annotations: &mcpsdk.ToolAnnotations{
			Title:          "Find provider locations",
			ReadOnlyHint:   true,
			IdempotentHint: true,
			OpenWorldHint:  boolPtr(false),
		},
	}, getProviderLocations)

	// fetch is not read-only: it provisions a billed egress client, and it
	// issues whatever method the caller asks for. It is additive rather than
	// destructive (it creates clients, never removes anything), and its world
	// is open -- it reaches arbitrary sites.
	mcpsdk.AddTool(mcpServer, &mcpsdk.Tool{
		Name:  "fetch",
		Title: "Fetch a URL from a location",
		Annotations: &mcpsdk.ToolAnnotations{
			Title:           "Fetch a URL from a location",
			ReadOnlyHint:    false,
			DestructiveHint: boolPtr(false),
			IdempotentHint:  false,
			OpenWorldHint:   boolPtr(true),
		},
		Description: `Load a URL from a chosen country, region, or city, as if browsing from there, and optionally return the images, stylesheets, scripts, and media the page references.

Use this to see what a site serves in a particular place: geo-targeted content, local pricing and availability, region-specific search results, or to check whether a site is reachable from somewhere.

Requires the mcp:fetch scope, because each new egress location opens a client billed to the network.

State is threaded through you, the caller, and every result tells you exactly what to pass back:
- signed_proxy_id names the egress that was used. Pass it back to load more pages from the same location; this is faster and reuses one client instead of opening a new one per call. Keep passing location too, so the egress can be re-established if it expires. Reuse guarantees the same location, not the same exit IP address.
- cookies carries the site session, so logins and consent banners survive across calls. It is opaque; pass it back unchanged, do not edit or interpret it.
- continuation appears when the page referenced more resources than fit in one call. Call again passing continuation to collect the rest; url is not needed then.
- payment_required appears when the network is at its plan's client limit and payment can settle it. Sign the payment it describes and call again with the same arguments plus payment set to the signed payment.

Resource discovery is static: the HTML is parsed for references. Content that a page loads with JavaScript is not seen.`,
	}, fetchTool)
}

// The sdk takes optional annotation hints by pointer to distinguish unset
// from false, and their defaults are not all false.
func boolPtr(b bool) *bool {
	return &b
}

const (
	MsgNoProviderLocations    = "No provider locations found"
	MsgFoundProviderLocations = "Found %d provider locations"
	MsgFoundLocationGroups    = "Found %d provider location groups"
	MsgFoundProviderDevices   = "Found %d provider devices"

	// client-facing tool error. Internal detail is logged, not sent.
	MsgProviderLocationsError = "Unable to find provider locations. Please try again later."
)

// One flattened location in the providerLocations output.
type LocationEntry struct {
	LocationId    string             `json:"location_id"`
	LocationType  model.LocationType `json:"location_type"`
	Name          string             `json:"name"`
	ProviderCount int                `json:"provider_count"`
	// 0 is an exact match
	MatchDistance int  `json:"match_distance"`
	Stable        bool `json:"stable"`
	StrongPrivacy bool `json:"strong_privacy"`
}

// Structured output of the providerLocations tool. A concrete object type
// (not a bare slice) so the sdk advertises an output schema and
// `structuredContent` is an object as the mcp spec requires.
type ProviderLocationsResult struct {
	Locations []*LocationEntry `json:"locations"`
}

// Input of the providerLocations tool.
type FindLocationsArgs struct {
	Query string `json:"query,omitempty" jsonschema:"Location name to search for (e.g. 'New York', 'Tokyo', 'US East'). Supports fuzzy matching. An empty or missing query returns a list of available countries."`
}

// Handles the providerLocations tool call.
func getProviderLocations(ctx context.Context, req *mcpsdk.CallToolRequest, findLocations FindLocationsArgs) (*mcpsdk.CallToolResult, *ProviderLocationsResult, error) {
	if !tokenHasScope(ctx, oauth.ScopeMcpRead) {
		return insufficientScopeResult(oauth.ScopeMcpRead), nil, nil
	}

	clientSession, err := clientSessionFromToken(ctx, req)
	if err != nil {
		// auth errors are actionable by the caller, return as-is
		return nil, nil, err
	}
	defer clientSession.Cancel()

	result, err := model.FindProviderLocations(&model.FindLocationsArgs{
		Query: findLocations.Query,
	}, clientSession)
	if err != nil {
		glog.Infof("[mcp]find provider locations error = %s\n", err)
		return nil, nil, errors.New(MsgProviderLocationsError)
	}

	locationEntries := make([]*LocationEntry, 0, len(result.Locations))
	for _, location := range result.Locations {
		// flatten location info
		locationEntry := &LocationEntry{
			LocationId:    location.LocationId.String(),
			LocationType:  location.LocationType,
			Name:          location.Name,
			ProviderCount: location.ProviderCount,
			MatchDistance: location.MatchDistance,
			Stable:        location.Stable,
			StrongPrivacy: location.StrongPrivacy,
		}
		locationEntries = append(locationEntries, locationEntry)
	}

	out := &ProviderLocationsResult{
		Locations: locationEntries,
	}

	text := MsgNoProviderLocations
	if 0 < len(locationEntries) {
		text = fmt.Sprintf(MsgFoundProviderLocations, len(locationEntries))
	}

	// per the mcp spec back-compat guidance, mirror the structured output as
	// serialized json in the text content, so hosts that only surface text
	// still deliver the data to the model
	outJson, err := json.Marshal(out)
	if err != nil {
		glog.Infof("[mcp]marshal provider locations error = %s\n", err)
		return nil, nil, errors.New(MsgProviderLocationsError)
	}

	return &mcpsdk.CallToolResult{
		Content: []mcpsdk.Content{
			&mcpsdk.TextContent{Text: text},
			&mcpsdk.TextContent{Text: string(outJson)},
		},
	}, out, nil
}

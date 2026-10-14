package controller

import (
	"net/http"
	"strings"

	"github.com/urnetwork/server/model"
)

// Automated lag.dev runs (lag.dev/PLAN.md amendment 12, item 13). A track is
// automated when its request came from an agent browser that signs what it
// sends (Web Bot Auth: a Signature-Agent header, as ChatGPT's agent sends),
// or from a hosting or cloud network, where a measurement describes a data
// centre's path rather than a visitor's own connection. An automated track
// is measured like any other, but its results count in no aggregate: the X9
// gauges leave them out and count them apart, and tracks per signing agent
// are capped. Only the flag and its reason are stored, never the address or
// the agent's name (amendment 9).

// The Web Bot Auth header naming the signing agent's key directory, such as
// "https://chatgpt.com".
const lagSignatureAgentHeader = "Signature-Agent"

// Hosting and cloud networks by autonomous system number, with whose network
// it is. A curated list of the networks agent browsers and automated clients
// run from, not a census of every data centre: an address outside it counts
// as a visitor's. Access networks that share an owner with a cloud are not
// listed, such as Google Fiber (AS16591).
//
// Cloudflare's own network is listed because Workers and its browser
// rendering run there. Its consumer VPN, WARP, leaves from the same network
// and cannot be told apart by number, so a WARP visitor is flagged too. That
// is right for the aggregates: such a measurement describes Cloudflare's path,
// not the visitor's access network.
var lagDatacenterAsns = map[int64]string{
	// Amazon Web Services
	7224:  "Amazon",
	8987:  "Amazon",
	14618: "Amazon",
	16509: "Amazon",
	38895: "Amazon",
	// Google's infrastructure, where its crawlers and renderers run, and
	// Google Cloud
	15169:  "Google",
	19527:  "Google",
	396982: "Google Cloud",
	// Microsoft and Azure
	8068: "Microsoft",
	8069: "Microsoft",
	8075: "Microsoft",
	// Oracle Cloud
	31898: "Oracle Cloud",
	// Alibaba Cloud
	37963: "Alibaba Cloud",
	45102: "Alibaba Cloud",
	// Tencent Cloud
	45090:  "Tencent Cloud",
	132203: "Tencent Cloud",
	// IBM Cloud (SoftLayer)
	36351: "IBM Cloud",
	// DigitalOcean
	14061: "DigitalOcean",
	// Akamai Connected Cloud (Linode)
	63949: "Akamai Linode",
	// OVHcloud
	16276: "OVHcloud",
	// Hetzner
	24940: "Hetzner",
	// Vultr
	20473: "Vultr",
	// Scaleway
	12876: "Scaleway",
	// Contabo
	51167: "Contabo",
	// Fly.io
	40509: "Fly.io",
	// Equinix Metal
	54825: "Equinix Metal",
	// Cloudflare, WARP included (see above)
	13335: "Cloudflare",
}

// The request's Signature-Agent, trimmed, or "" when it sent none.
func lagSignatureAgent(header http.Header) string {
	return strings.TrimSpace(header.Get(lagSignatureAgentHeader))
}

// Why a track from this request is automated (one of
// model.LagAutomatedReasons), or "" when it is a visitor's. A signing agent
// comes first: it names an agent outright, where a network only says where
// the request came from.
func lagTrackAutomatedReason(header http.Header, asn *int64) string {
	if lagSignatureAgent(header) != "" {
		return model.LagAutomatedReasonSignatureAgent
	}
	if asn != nil {
		if _, ok := lagDatacenterAsns[*asn]; ok {
			return model.LagAutomatedReasonDatacenterAsn
		}
	}
	return ""
}

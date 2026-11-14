package controller

// §12.8's record API on api for the console and network credentials (DNS-M5-12; R-DNS-2, R-DNS-3, D-DNS-7): GET
// /network/dns, read roles, and POST /network/dns/records, the DNS permission, which route_authz.go and
// route_permission.go check before either runs. Both serve dns.yml's one root (model.DnsUserRoot) and answer 503 while
// the process holds none or the binary does not link MG2 and MG5. A write goes through the model's one write, which
// the dns block's POST /dns/records for sidecars shares, so the two refuse alike; a network credential writes under no
// decision, so a body naming one is refused.

import (
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/urnetwork/sidecar"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

// The root the routes serve, or the refusal while there is none.
func networkDnsRoot() (string, error) {
	root := model.DnsUserRoot()
	if root == "" {
		return "", &sidecar.DnsApiError{Status: http.StatusServiceUnavailable, Code: sidecar.DnsApiCodeUnavailable, Message: "no dns root is configured"}
	}
	return root, nil
}

// GET /network/dns: the network's label, allocated on first need, its alias, its records and dns_entries' quota.
func GetNetworkDns(clientSession *session.ClientSession) (*sidecar.NetworkDns, error) {
	root, err := networkDnsRoot()
	if err != nil {
		return nil, err
	}
	return model.GetNetworkDns(clientSession.Ctx, root, clientSession.ByJwt.NetworkId)
}

// POST /network/dns/records: {expect_version, upserts, deletes} with compare-and-swap, answered the zone's version.
func WriteNetworkDnsRecords(request *sidecar.DnsRecordsRequest, clientSession *session.ClientSession) (*sidecar.DnsRecordsAnswer, error) {
	if request == nil {
		return nil, fmt.Errorf("%d %s", http.StatusBadRequest, "A write names its records.")
	}
	if request.Approval != nil {
		return nil, &sidecar.DnsApiError{Status: http.StatusBadRequest, Code: sidecar.DnsApiCodeInvalid, Message: "a network credential writes under no decision"}
	}
	root, err := networkDnsRoot()
	if err != nil {
		return nil, err
	}
	actorKind, actorId := model.AdminEventActorOf(clientSession)
	caller := &model.DnsRecordsCaller{
		NetworkId: clientSession.ByJwt.NetworkId,
		ActorKind: actorKind,
		ActorId:   actorId,
	}
	if requestId, ok := session.RequestIdFromContext(clientSession.Ctx); ok {
		caller.RequestId = &requestId
	}
	body, err := model.WriteDnsRecords(clientSession.Ctx, &model.DnsRecordsWrite{
		Root:    root,
		Caller:  caller,
		Request: request,
		Now:     server.NowUtc(),
	})
	if err != nil {
		return nil, err
	}
	answer := &sidecar.DnsRecordsAnswer{}
	if err := json.Unmarshal(body, answer); err != nil {
		return nil, err
	}
	return answer, nil
}

// POST /network/dns/label-reclaim (§12.8, DNS-M5-16): after a restore, binds an ownerless retiring name back to the
// caller's network, given the tuple signed by the key of a CT-logged leaf naming it (model.ReclaimDnsLabel). The
// DNS permission by the role gate; the tuple must name the credential's own network.
func ReclaimNetworkDnsLabel(request *sidecar.DnsLabelReclaimRequest, clientSession *session.ClientSession) (*sidecar.DnsLabelReclaimAnswer, error) {
	if request == nil {
		return nil, fmt.Errorf("%d %s", http.StatusBadRequest, "A reclaim names its tuple.")
	}
	root, err := networkDnsRoot()
	if err != nil {
		return nil, err
	}
	actorKind, actorId := model.AdminEventActorOf(clientSession)
	reclaim := &model.DnsLabelReclaim{
		Root:      root,
		NetworkId: clientSession.ByJwt.NetworkId,
		Request:   request,
		ActorKind: actorKind,
		ActorId:   actorId,
	}
	if requestId, ok := session.RequestIdFromContext(clientSession.Ctx); ok {
		reclaim.RequestId = &requestId
	}
	return model.ReclaimDnsLabel(clientSession.Ctx, reclaim)
}

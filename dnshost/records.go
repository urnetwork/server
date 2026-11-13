package dnshost

// dnsservice.DnsRecordStore over the model (DNS-M5-12): §12.8's one write for a
// sidecar's POST /dns/records, its admin event naming the sidecar, and the
// zone GET /dns/zone filters by the caller's grants. The model's refusals are
// *sidecar.DnsApiError already, which the block answers as they are. Safe for
// concurrent use.

import (
	"context"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/dnsservice"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

var _ dnsservice.DnsRecordStore = (*Records)(nil)

// The store. It holds nothing.
type Records struct{}

func NewRecords() *Records {
	return &Records{}
}

func (self *Records) WriteRecords(ctx context.Context, call *dnsservice.DnsRecordsCall) (answer []byte, returnErr error) {
	returnErr = storeCall(func() error {
		sidecarId := server.Id(call.SidecarId)
		var err error
		answer, err = model.WriteDnsRecords(ctx, &model.DnsRecordsWrite{
			Root: call.Root,
			Caller: &model.DnsRecordsCaller{
				NetworkId: server.Id(call.NetworkId),
				SidecarId: &sidecarId,
				ActorKind: model.AdminEventActorSidecar,
				ActorId:   &sidecarId,
			},
			Request:   call.Request,
			EffectKey: call.EffectKey,
			Now:       call.Now,
		})
		return err
	})
	return
}

func (self *Records) Zone(ctx context.Context, root string, networkId sidecar.Id) (zone *sidecar.DnsZone, returnErr error) {
	returnErr = storeCall(func() error {
		var err error
		zone, err = model.GetDnsZoneView(ctx, root, server.Id(networkId))
		return err
	})
	return
}

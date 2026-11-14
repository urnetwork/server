package dnshost

// dnsservice.DnsReclaimStore over the model (DNS-M5-16): a sidecar's POST
// /dns/label-reclaim, its admin event naming the sidecar. The model's refusals
// are *sidecar.DnsApiError already. Safe for concurrent use.

import (
	"context"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/dnsservice"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

var _ dnsservice.DnsReclaimStore = (*Reclaims)(nil)

// The store. It holds nothing.
type Reclaims struct{}

func NewReclaims() *Reclaims {
	return &Reclaims{}
}

func (self *Reclaims) ReclaimLabel(ctx context.Context, call *dnsservice.DnsReclaimCall) (answer *sidecar.DnsLabelReclaimAnswer, returnErr error) {
	returnErr = storeCall(func() error {
		sidecarId := server.Id(call.SidecarId)
		var err error
		answer, err = model.ReclaimDnsLabel(ctx, &model.DnsLabelReclaim{
			Root:      call.Root,
			NetworkId: server.Id(call.NetworkId),
			Request:   call.Request,
			ActorKind: model.AdminEventActorSidecar,
			ActorId:   &sidecarId,
		})
		return err
	})
	return
}

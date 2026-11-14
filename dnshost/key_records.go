package dnshost

// dnsservice.DnsKeyRecordStore over the model (DNS-M7-02): a receiver's POST
// /dns/key-record, which the block has already gated on its listen grant. The
// model's refusals are *sidecar.DnsApiError already. Safe for concurrent use.

import (
	"context"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/dnsservice"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

var _ dnsservice.DnsKeyRecordStore = (*KeyRecords)(nil)

// The store. It holds nothing.
type KeyRecords struct{}

func NewKeyRecords() *KeyRecords {
	return &KeyRecords{}
}

func (self *KeyRecords) PublishKeyRecord(ctx context.Context, call *dnsservice.DnsKeyRecordCall) (answer *sidecar.DnsKeyRecordAnswer, returnErr error) {
	returnErr = storeCall(func() error {
		var err error
		answer, err = model.PublishDnsKeyRecord(ctx, &model.DnsKeyRecord{
			Root:              call.Root,
			NetworkId:         server.Id(call.NetworkId),
			ReceiverSidecarId: server.Id(call.SidecarId),
			Request:           call.Request,
		})
		return err
	})
	return
}

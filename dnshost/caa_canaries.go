package dnshost

// dnsservice.DnsCaaCanaryWriter over model.DnsPutCaaCanary and
// model.DnsDeleteCaaCanary (DNS-M6-05): certs' CAA canaries for a strong-mode
// custom host's probe through §12.6's one write path, with the model's
// refusals passed as the route answers them. Safe for concurrent use.

import (
	"context"

	"github.com/urnetwork/sidecar/dnsservice"

	"github.com/urnetwork/server/model"
)

var _ dnsservice.DnsCaaCanaryWriter = (*CaaCanaries)(nil)

// The writer. It holds nothing.
type CaaCanaries struct{}

func NewCaaCanaries() *CaaCanaries {
	return &CaaCanaries{}
}

// Publishes put's canary in root and answers its serial.
func (self *CaaCanaries) PutCaaCanary(ctx context.Context, root string, put *dnsservice.DnsCaaCanaryPut) (serial uint64, returnErr error) {
	returnErr = storeCall(func() error {
		result, err := model.DnsPutCaaCanary(ctx, root, put)
		if err != nil {
			return err
		}
		serial = result.Serial
		return nil
	})
	return
}

// Takes the canary at owner and its copy out, answering the serial, or
// dnsservice.ErrDnsWriteEmpty when none stands.
func (self *CaaCanaries) DeleteCaaCanary(ctx context.Context, root string, owner string) (serial uint64, returnErr error) {
	returnErr = storeCall(func() error {
		result, err := model.DnsDeleteCaaCanary(ctx, root, owner)
		if err != nil {
			return err
		}
		serial = result.Serial
		return nil
	})
	return
}

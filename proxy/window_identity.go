package proxy

import (
	"context"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/glog"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// windowIdentityStore adapts the per-proxy redis persistence to
// `connect.MultiClientIdentityStore` for one hosted device
// (PROXYDRAIN1.md §3.5). Store/Load errors are swallowed (logged): identity
// persistence is a best-effort continuity optimization, and a redis outage
// must never break the device's window management.
type windowIdentityStore struct {
	ctx     context.Context
	proxyId server.Id
}

func newWindowIdentityStore(ctx context.Context, proxyId server.Id) *windowIdentityStore {
	return &windowIdentityStore{
		ctx:     ctx,
		proxyId: proxyId,
	}
}

func (self *windowIdentityStore) StoreWindowClientIdentities(identities []*connect.WindowClientIdentity) {
	modelIdentities := make([]*model.ProxyWindowClientIdentity, 0, len(identities))
	for _, identity := range identities {
		destinationIds := []server.Id{}
		for _, destinationId := range identity.Destination.Ids() {
			destinationIds = append(destinationIds, server.Id(destinationId))
		}
		modelIdentities = append(modelIdentities, &model.ProxyWindowClientIdentity{
			ClientId:       server.Id(identity.ClientId),
			ByJwt:          identity.ByJwt,
			InstanceId:     server.Id(identity.InstanceId),
			DestinationIds: destinationIds,
		})
	}
	if r := server.HandleError(func() {
		model.SetProxyWindowIdentities(self.ctx, self.proxyId, modelIdentities)
	}); r != nil {
		glog.Infof("[pd][%s]window identity store err=%v\n", self.proxyId, r)
	}
}

func (self *windowIdentityStore) LoadWindowClientIdentities() []*connect.WindowClientIdentity {
	var modelIdentities []*model.ProxyWindowClientIdentity
	if r := server.HandleError(func() {
		modelIdentities = model.GetProxyWindowIdentities(self.ctx, self.proxyId)
	}); r != nil {
		glog.Infof("[pd][%s]window identity load err=%v\n", self.proxyId, r)
		return nil
	}

	identities := make([]*connect.WindowClientIdentity, 0, len(modelIdentities))
	for _, modelIdentity := range modelIdentities {
		destinationIds := make([]connect.Id, 0, len(modelIdentity.DestinationIds))
		for _, destinationId := range modelIdentity.DestinationIds {
			destinationIds = append(destinationIds, connect.Id(destinationId))
		}
		destination, err := connect.NewMultiHopId(destinationIds...)
		if err != nil {
			continue
		}
		identities = append(identities, &connect.WindowClientIdentity{
			ClientId:    connect.Id(modelIdentity.ClientId),
			ByJwt:       modelIdentity.ByJwt,
			InstanceId:  connect.Id(modelIdentity.InstanceId),
			Destination: destination,
		})
	}
	if 0 < len(identities) {
		glog.Infof("[pd][%s]window identity restore: %d identities\n", self.proxyId, len(identities))
	}
	return identities
}

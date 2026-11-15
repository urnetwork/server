package model

// A read of a network's verified state at the store's tip (QD-829): what a route judges its caller by where the
// newest projection's rows decide, GET /sidecar/scim-changes's scim_signer and AdminSet among them. This instance's
// verifier may trail entries another api instance landed, which an append would only meet at its 412, so the read
// applies every entry the store holds past the verifier's tip first, as a 412 does (applyWithLock): the store, never
// this memory, says what the log is. It holds the network's lock across those store reads, as an append does, and no
// Postgres connection.

import (
	"context"
	"fmt"

	"github.com/urnetwork/sidecar"

	"github.com/urnetwork/server"
)

// The network's verified state at the store's tip, nil with no error before base 1. An entry that does not apply drops
// the verifier, as an append's does, and answers its error.
func (self *ConfigLogSequencer) CaughtUpState(ctx context.Context, networkId server.Id) (*sidecar.ConfigState, error) {
	network := self.network(networkId)
	network.stateLock.Lock()
	defer network.stateLock.Unlock()
	if err := network.loadWithLock(ctx, self, nil); err != nil {
		return nil, err
	}
	if network.log == nil {
		return nil, nil
	}
	for {
		tip := network.log.Pin().GetVersion()
		positions, more, err := self.listPositions(ctx, networkId, tip, max(self.settings.ReadPageCount, 1))
		if err != nil {
			return nil, err
		}
		for _, n := range positions {
			entryBytes, found, err := configLogGet(ctx, self.cluster, sidecar.ConfigEntryKey(sidecar.Id(networkId), n))
			if err != nil {
				return nil, err
			}
			if !found {
				return nil, fmt.Errorf("entry %d listed and gone", n)
			}
			if err := network.applyWithLock(ctx, self, entryBytes); err != nil {
				return nil, err
			}
		}
		if !more || len(positions) == 0 {
			return network.log.State(), nil
		}
	}
}

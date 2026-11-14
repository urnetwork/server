package model

// What §19.10's restore frame (server/sidecarrestore; OPS-M3-03) reads from a log sequencer beyond
// config_log_restore.go's steps, each from the store and never from a verifier this process held before:
//
//	head    AdvanceAclHeadAfterRestore: the heads step for one network whether or not the restored rows hold its
//	        head. A network whose log began after the sidecar dump has none, and its head goes in at the store's
//	        verified tip with has_document, so a first policy document filed after the dumps holds there too; a
//	        network with a head takes AdvanceAclHeadsAfterRestore's rule, never moving down or across
//	repos   RestoreRepoIds: the repos a network's verified log names, which §19.10's deleted-network path marks
//	        when the restored rows lack the network

import (
	"context"
	"errors"
	"fmt"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/protocol/bridge"

	"github.com/urnetwork/server"
)

// The network's verifier, loaded afresh from the store under its lock: nil with no log there. f runs under the
// network's lock and must not block on anything but the state it is given.
func (self *ConfigLogSequencer) withFreshLog(ctx context.Context, networkId server.Id, f func(log *sidecar.ConfigLog) error) error {
	network := self.network(networkId)
	network.stateLock.Lock()
	defer network.stateLock.Unlock()
	network.log, network.signedGenesis = nil, nil
	clear(network.basePins)
	network.bases = nil
	if err := network.loadWithLock(ctx, self, nil); err != nil {
		return err
	}
	return f(network.log)
}

// The heads step for one network (see the file header). The outcome says where the head stood and stands; its Err
// says why it did not move where it could not.
func AdvanceAclHeadAfterRestore(ctx context.Context, sequencer *ConfigLogSequencer, networkId server.Id) (*ConfigLogRestoreHead, error) {
	head, err := GetNetworkAclHead(ctx, networkId)
	if err != nil {
		return nil, err
	}
	outcome := &ConfigLogRestoreHead{NetworkId: networkId}
	var pin *bridge.AuthPin
	hasDocument := false
	if head != nil {
		outcome.From, outcome.To = head.Version, head.Version
		pin, hasDocument, err = sequencer.verifiedTip(ctx, networkId, &bridge.AuthPin{Version: head.Version, Chain: head.Chain})
	} else {
		err = sequencer.withFreshLog(ctx, networkId, func(log *sidecar.ConfigLog) error {
			if log == nil {
				return nil
			}
			state := log.State()
			pin = state.Pin()
			if acl, err := state.Acl(); err == nil {
				hasDocument = acl.HasDocument()
			}
			return nil
		})
	}
	if err != nil {
		outcome.Err = err
		return outcome, nil
	}
	if pin == nil || pin.GetVersion() <= outcome.From {
		return outcome, nil
	}
	if _, err := AdvanceNetworkAclHead(ctx, networkId, pin.GetVersion(), pin.GetChain(), hasDocument); err != nil {
		outcome.Err = err
		return outcome, nil
	}
	outcome.To = pin.GetVersion()
	return outcome, nil
}

// The store holds the network's log but its verifier holds no ACL state to read repos from.
var ErrRestoreRepoIdsUnreadable = errors.New("the network's log holds no readable repo rows")

// The repos the network's verified log names, false when the store holds no log for the network (see the file
// header).
func (self *ConfigLogSequencer) RestoreRepoIds(ctx context.Context, networkId server.Id) (repoIds []sidecar.Id, found bool, returnErr error) {
	returnErr = self.withFreshLog(ctx, networkId, func(log *sidecar.ConfigLog) error {
		if log == nil {
			return nil
		}
		found = true
		acl, err := log.State().Acl()
		if err != nil {
			return fmt.Errorf("%w: %v", ErrRestoreRepoIdsUnreadable, err)
		}
		repoIds = acl.RepoIds()
		return nil
	})
	return
}

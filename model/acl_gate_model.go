package model

import (
	"fmt"

	"github.com/urnetwork/sidecar"

	"github.com/urnetwork/server"
)

// §8.9's {acl_<networkId>}v in a process that hosts a gate: api INCRs it after every entry it appends to a network's
// authorization log, class $ in the keyspace events (xops/redis-set-notify-keyspace-events.sh), and the process's gate
// cache (sidecar.AclGateCache) reloads that network. Only such a process subscribes the pattern, since every
// notification on every master is matched against every pattern; engines take the same event through
// ServiceHost.Redis's shared subscriber, never per client (sidecar.AclGateCache.Watch).

// The psubscribe pattern of every network's {acl_}v on db.
func AclVersionKeyEventPattern(db int) string {
	return fmt.Sprintf("__keyspace@%d__:%s", db, sidecar.AclVersionKeyPattern)
}

// The network whose {acl_}v a channel matching AclVersionKeyEventPattern names, false for any other channel.
func ParseAclVersionKeyEvent(channel string) (server.Id, bool) {
	key, ok := ParseKeyspaceChannelKey(channel)
	if !ok {
		return server.Id{}, false
	}
	networkId, ok := sidecar.ParseAclVersionKey(key)
	if !ok {
		return server.Id{}, false
	}
	return server.Id(networkId), true
}

// The end as §8.10's gate reads it (sidecar.AclContractEnd), for a client the
// relationship's query or GetNetworkPeerAclEnds found: its network, the subject
// it acts as and that subject's binding and marks. A sidecar's client or a
// tagged one acts only as its sidecar. The caller adds what only it knows: a
// platform end, a declared source, a hard deny.
func (self *ProvideRelationshipAclEnd) ContractEnd() sidecar.AclContractEnd {
	end := sidecar.AclContractEnd{
		Known:        true,
		NetworkId:    sidecar.Id(self.NetworkId),
		SidecarHeld:  self.StrictExp || self.Tagged,
		AclEnforcing: self.AclEnforcing,
		Hosted:       self.Hosted,
		Roles:        self.Roles,
		Principal:    self.Principal,
	}
	if self.SubjectClientId != nil {
		end.SubjectClientId = sidecar.Id(*self.SubjectClientId)
	}
	if self.SidecarId != nil {
		end.SidecarId = sidecar.Id(*self.SidecarId)
	}
	if self.MemberId != nil {
		end.MemberId = sidecar.Id(*self.MemberId)
	}
	return end
}

// The retained global chains are keyed by every field of their subnet, and an
// incomplete subnet keys nothing. A submission is admitted within its network's
// hotkey links and the new generation bound, and a fork is refused first.
// Pure: no database.
package model

import (
	"errors"
	"testing"

	"github.com/urfoundation/sn/protocol"
)

func TestHotkeyWalletMappingSubnetHashSeparatesSubnets(t *testing.T) {
	subnet := protocol.HotkeyWalletMappingSubnet{ChainID: 945, GenesisHash: [32]byte{1}, Netuid: 25}
	hash, err := hotkeyWalletMappingSubnetHash(subnet)
	if err != nil {
		t.Fatal(err)
	}
	if again, err := hotkeyWalletMappingSubnetHash(subnet); err != nil || again != hash {
		t.Fatal("the subnet key is not deterministic", err)
	}
	chain, genesis, netuid := subnet, subnet, subnet
	chain.ChainID += 1
	genesis.GenesisHash[31] = 1
	netuid.Netuid += 1
	for _, other := range []protocol.HotkeyWalletMappingSubnet{chain, genesis, netuid} {
		if otherHash, err := hotkeyWalletMappingSubnetHash(other); err != nil || otherHash == hash {
			t.Fatal("two subnets share a chain key", other, err)
		}
	}
	for _, incomplete := range []protocol.HotkeyWalletMappingSubnet{{GenesisHash: [32]byte{1}, Netuid: 25}, {ChainID: 945, Netuid: 25}, {ChainID: 945, GenesisHash: [32]byte{1}}} {
		if _, err := hotkeyWalletMappingSubnetHash(incomplete); !errors.Is(err, protocol.ErrWalletMappingIntegrity) {
			t.Fatal("an incomplete subnet keyed a chain", incomplete, err)
		}
	}
}

// A network links at most MaxHotkeyWalletMappingNetworkHotkeys hotkeys and a
// submission adds at most MaxHotkeyWalletMappingNewGenerations generations. A
// hotkey the network already links is never refused for the links, a chain the
// operator retains still takes a link from a network that submits it first,
// and a longer chain arrives after its prefix.
func TestHotkeyWalletMappingSubmissionBoundsHotkeysAndNewGenerations(t *testing.T) {
	hashes := func(count int, seed byte) [][32]byte {
		result := make([][32]byte, count)
		for index := range result {
			result[index] = [32]byte{seed, byte(index), byte(index >> 8)}
		}
		return result
	}
	full := hashes(MaxHotkeyWalletMappingNewGenerations+1, 1)
	forked := hashes(2, 1)
	forked[1][0] = 2
	for _, c := range []struct {
		name      string
		retained  [][32]byte
		submitted [][32]byte
		linked    bool
		links     int
		appended  int
		err       error
	}{
		{name: "first hotkey", submitted: full[:1], appended: 1},
		{name: "last free link", submitted: full[:1], links: MaxHotkeyWalletMappingNetworkHotkeys - 1, appended: 1},
		{name: "new hotkey past the links", submitted: full[:1], links: MaxHotkeyWalletMappingNetworkHotkeys, err: ErrHotkeyWalletMappingNetworkHotkeys},
		{name: "retained chain from an unlinked network past the links", retained: full[:1], submitted: full[:1], links: MaxHotkeyWalletMappingNetworkHotkeys, err: ErrHotkeyWalletMappingNetworkHotkeys},
		{name: "replay of a linked hotkey at the links", retained: full[:2], submitted: full[:2], linked: true, links: MaxHotkeyWalletMappingNetworkHotkeys},
		{name: "prefix replay", retained: full[:3], submitted: full[:1], linked: true, links: 1},
		{name: "extension of a linked hotkey at the links", retained: full[:1], submitted: full[:2], linked: true, links: MaxHotkeyWalletMappingNetworkHotkeys, appended: 1},
		{name: "most new generations", submitted: full[:MaxHotkeyWalletMappingNewGenerations], appended: MaxHotkeyWalletMappingNewGenerations},
		{name: "too many new generations", submitted: full, err: ErrHotkeyWalletMappingNewGenerations},
		{name: "long chain after its prefix", retained: full[:1], submitted: full, linked: true, links: 1, appended: MaxHotkeyWalletMappingNewGenerations},
		{name: "fork", retained: full[:2], submitted: forked, linked: true, links: 1, err: protocol.ErrWalletMappingIntegrity},
		{name: "fork past the links", retained: full[:2], submitted: forked, links: MaxHotkeyWalletMappingNetworkHotkeys, err: protocol.ErrWalletMappingIntegrity},
	} {
		if appended, err := hotkeyWalletMappingSubmission(c.retained, c.submitted, c.linked, c.links); appended != c.appended || !errors.Is(err, c.err) {
			t.Errorf("%s: appended %d, %v; want %d, %v", c.name, appended, err, c.appended, c.err)
		}
	}
}

package client

import (
	"os"
	"slices"
	"testing"

	"github.com/go-playground/assert/v2"
)

func TestNetworkSpaceManager(t *testing.T) {
	storagePath, err := os.MkdirTemp("", "test_network_space_manager")
	assert.Equal(t, err, nil)

	networkSpaceManager := NewNetworkSpaceManager(storagePath)
	assert.Equal(t, networkSpaceManager.GetNetworkSpaces().Len(), 0)
	assert.Equal(t, networkSpaceManager.GetActiveNetworkSpace(), nil)

	networkSpace := networkSpaceManager.updateNetworkSpace(
		NewNetworkSpaceKey("ur.network", "main"),
		func(values *NetworkSpaceValues) {
		},
	)
	assert.Equal(t, networkSpaceManager.GetNetworkSpaces().Len(), 1)
	assert.Equal(t, networkSpaceManager.GetNetworkSpaces().Get(0) == networkSpace, true)
	assert.Equal(t, networkSpaceManager.GetActiveNetworkSpace(), nil)

	networkSpaceManager.SetActiveNetworkSpace(networkSpace)
	assert.Equal(t, networkSpaceManager.GetActiveNetworkSpace() == networkSpace, true)

	networkSpaceManager.RemoveNetworkSpace(networkSpace)
	assert.Equal(t, networkSpaceManager.GetNetworkSpaces().Len(), 1)
	assert.Equal(t, networkSpaceManager.GetNetworkSpaces().Get(0) == networkSpace, true)
	assert.Equal(t, networkSpaceManager.GetActiveNetworkSpace() == networkSpace, true)

	networkSpaceManager.Close()

	networkSpaceManager2 := NewNetworkSpaceManager(storagePath)
	assert.Equal(t, networkSpaceManager2.GetNetworkSpaces().Len(), 1)
	networkSpace = networkSpaceManager2.GetNetworkSpaces().Get(0)
	assert.Equal(t, networkSpaceManager2.GetActiveNetworkSpace() == networkSpace, true)

	networkSpace2 := networkSpaceManager2.updateNetworkSpace(
		NewNetworkSpaceKey("bringyour.com", "main"),
		func(values *NetworkSpaceValues) {
		},
	)
	m1 := map[*NetworkSpace]bool{}
	m1List := networkSpaceManager2.GetNetworkSpaces()
	for i := 0; i < m1List.Len(); i += 1 {
		m1[m1List.Get(i)] = true
	}
	m2 := map[*NetworkSpace]bool{
		networkSpaceManager2.GetNetworkSpace(NewNetworkSpaceKey("ur.network", "main")):    true,
		networkSpaceManager2.GetNetworkSpace(NewNetworkSpaceKey("bringyour.com", "main")): true,
	}
	assert.Equal(t, m1, m2)
	assert.Equal(t, networkSpaceManager2.GetActiveNetworkSpace() == networkSpace, true)
	networkSpaceManager2.SetActiveNetworkSpace(networkSpace2)
	assert.Equal(t, networkSpaceManager2.GetActiveNetworkSpace() == networkSpace2, true)
	networkSpaceManager2.Close()

	networkSpaceManager3 := NewNetworkSpaceManager(storagePath)
	assert.Equal(t, networkSpaceManager3.GetNetworkSpaces().Len(), 2)
	networkSpaces3 := []*NetworkSpace{}
	networkSpaces3List := networkSpaceManager3.GetNetworkSpaces()
	for i := 0; i < networkSpaces3List.Len(); i += 1 {
		networkSpaces3 = append(networkSpaces3, networkSpaces3List.Get(i))
	}
	assert.Equal(t, slices.Contains(networkSpaces3, networkSpaceManager3.GetActiveNetworkSpace()), true)

	networkSpace3 := networkSpaceManager3.updateNetworkSpace(
		NewNetworkSpaceKey("ur.io", "main"),
		func(values *NetworkSpaceValues) {
		},
	)
	m1 = map[*NetworkSpace]bool{}
	m1List = networkSpaceManager3.GetNetworkSpaces()
	for i := 0; i < m1List.Len(); i += 1 {
		m1[m1List.Get(i)] = true
	}
	m2 = map[*NetworkSpace]bool{
		networkSpaceManager3.GetNetworkSpace(NewNetworkSpaceKey("ur.network", "main")):    true,
		networkSpaceManager3.GetNetworkSpace(NewNetworkSpaceKey("bringyour.com", "main")): true,
		networkSpaceManager3.GetNetworkSpace(NewNetworkSpaceKey("ur.io", "main")):         true,
	}
	assert.Equal(t, m1, m2)
	networkSpaceManager3.SetActiveNetworkSpace(networkSpace3)
	assert.Equal(t, networkSpaceManager3.GetActiveNetworkSpace() == networkSpace3, true)

	networkSpaceManager3.RemoveNetworkSpace(networkSpaceManager3.GetNetworkSpace(NewNetworkSpaceKey("ur.network", "main")))
	assert.Equal(t, networkSpaceManager3.GetNetworkSpaces().Len(), 2)
	networkSpaceManager3.RemoveNetworkSpace(networkSpaceManager3.GetNetworkSpace(NewNetworkSpaceKey("bringyour.com", "main")))
	assert.Equal(t, networkSpaceManager3.GetNetworkSpaces().Len(), 1)
	// cannot remove active network space
	networkSpaceManager3.RemoveNetworkSpace(networkSpaceManager3.GetNetworkSpace(NewNetworkSpaceKey("ur.io", "main")))
	assert.Equal(t, networkSpaceManager3.GetNetworkSpaces().Len(), 1)

	networkSpaceManager3.SetActiveNetworkSpace(nil)
	networkSpaceManager3.RemoveNetworkSpace(networkSpaceManager3.GetNetworkSpace(NewNetworkSpaceKey("ur.io", "main")))
	assert.Equal(t, networkSpaceManager3.GetNetworkSpaces().Len(), 0)

	networkSpaceManager3.Close()
}

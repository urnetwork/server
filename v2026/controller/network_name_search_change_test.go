package controller

// A name change or claim replaces the network's entry in the network name
// search in its own transaction. Renames used to leave the search on the name
// the network was created with, so the fuzzy name checks and the name lookup
// kept finding the old name and missed the new one.

import (
	"context"
	"fmt"
	"testing"

	"github.com/urnetwork/server/v2026"

	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/search"
	"github.com/urnetwork/server/v2026/session"
)

// After a change or a claim, the database index, and an in-memory index loaded
// from it afresh (as another process or a restart loads it), find the network
// under its new name and not under its old one. The process's own in-memory
// index is updated before the write commits and converges from the search's
// update log on a background poll, which a read right after the write races,
// so it is not asserted.
func TestChangeNetworkNameReindexesNetworkName(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		// the database index of the network name search (model networkNameSearch)
		storedSearch := search.NewSearchDb("network_name", search.SearchTypeFull)

		for _, c := range []struct {
			name   string
			change func(args ChangeNetworkNameArgs, clientSession *session.ClientSession) (*ChangeNetworkNameResult, error)
		}{
			{
				name:   "change",
				change: ChangeNetworkName,
			},
			{
				name:   "claim",
				change: ClaimNetworkName,
			},
		} {
			networkId := server.NewId()
			userId := server.NewId()
			oldName := fmt.Sprintf("%s-search-before", c.name)
			model.Testing_CreateNetwork(ctx, networkId, oldName, userId)
			// indexed the way network create indexes it
			storedSearch.Add(ctx, oldName, networkId, 0)

			newName := fmt.Sprintf("%s-renamed-after", c.name)
			result, err := c.change(
				ChangeNetworkNameArgs{
					NetworkName: newName,
				},
				session.Testing_CreateClientSession(ctx, &session.ByJwt{
					NetworkId: networkId,
					UserId:    userId,
				}),
			)
			if err != nil || result == nil || result.Error != nil {
				t.Errorf("%s: the rename answered %+v, %v", c.name, result, err)
				continue
			}

			loadedSearch := search.NewSearchLocalWithDefaults(ctx, storedSearch)
			loadedSearch.WaitForInitialSync(ctx)
			for _, index := range []struct {
				name        string
				networkName string
				indexed     bool
			}{
				{
					name:        "new name",
					networkName: newName,
					indexed:     true,
				},
				{
					name:        "old name",
					networkName: oldName,
					indexed:     false,
				},
			} {
				_, stored := storedSearch.AroundIds(ctx, index.networkName, 0)[networkId]
				_, loaded := loadedSearch.AroundIds(ctx, index.networkName, 0)[networkId]
				if stored != index.indexed || loaded != index.indexed {
					t.Errorf("%s: %s: the search holds %q in the database %t and loaded in memory %t, want %t", c.name, index.name, index.networkName, stored, loaded, index.indexed)
				}
			}
			loadedSearch.Close()
		}
	})
}

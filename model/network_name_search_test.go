package model

// A rename replaces the network's entry in the network name search in the
// rename's own transaction, so the fuzzy name checks and the name lookup find
// the new name and no longer the old one. Renames used to leave the search on
// the name the network was created with. Network create indexes the name as
// it is stored, rather than the request's text.

import (
	"context"
	"testing"

	"github.com/urnetwork/server"

	"github.com/urnetwork/server/search"
	"github.com/urnetwork/server/session"
)

// Whether the network name search holds the network under exactly the name: in
// the database index, and in an in-memory index loaded from it afresh, as
// another process or a restart loads it. The process's own in-memory index is
// not asserted: it is updated before the write commits and converges from the
// search's update log on a background poll, which a read right after a write
// races.
func networkNameIndexed(ctx context.Context, networkName string, networkId server.Id) (stored bool, loaded bool) {
	storedSearch := search.NewSearchDb(networkNameSearch().Realm(), networkNameSearch().SearchType())
	_, stored = storedSearch.AroundIds(ctx, networkName, 0)[networkId]
	loadedSearch := search.NewSearchLocalWithDefaults(ctx, storedSearch)
	defer loadedSearch.Close()
	loadedSearch.WaitForInitialSync(ctx)
	_, loaded = loadedSearch.AroundIds(ctx, networkName, 0)[networkId]
	return
}

// After NetworkUpdate, the search finds the network under its new name and not
// under its old one.
func TestNetworkUpdateReindexesNetworkName(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		networkId := server.NewId()
		userId := server.NewId()
		oldName := "search-before-update"
		Testing_CreateNetwork(ctx, networkId, oldName, userId)
		// indexed the way network create indexes it
		networkNameSearch().Add(ctx, oldName, networkId, 0)

		newName := "renamed-after-update"
		result, err := NetworkUpdate(
			NetworkUpdateArgs{
				NetworkName: newName,
			},
			session.Testing_CreateClientSession(ctx, &session.ByJwt{
				NetworkId: networkId,
				UserId:    userId,
			}),
		)
		if err != nil || result == nil || result.Error != nil {
			t.Fatalf("the update answered %+v, %v", result, err)
		}

		for _, c := range []struct {
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
			stored, loaded := networkNameIndexed(ctx, c.networkName, networkId)
			if stored != c.indexed || loaded != c.indexed {
				t.Errorf("%s: the search holds %q in the database %t and loaded in memory %t, want %t", c.name, c.networkName, stored, loaded, c.indexed)
			}
		}
	})
}

// Network create indexes the validated name it stores. It indexed the
// request's text, so a name sent with spaces was indexed with spaces while the
// network stored dashes, and the checks compared against the wrong name.
func TestNetworkCreateIndexesStoredNetworkName(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		userAuth := "indexed-create@example.com"
		password := "synthetic-password-1"

		result, err := NetworkCreate(
			NetworkCreateArgs{
				UserName:    "indexed-create",
				UserAuth:    &userAuth,
				Password:    &password,
				NetworkName: "Indexed Create Name",
				Terms:       true,
			},
			session.Testing_CreateClientSession(ctx, nil),
		)
		if err != nil || result == nil || result.Error != nil || result.Network == nil {
			t.Fatalf("the create answered %+v, %v", result, err)
		}
		networkId := result.Network.NetworkId
		if networkName := networkNameOf(ctx, networkId); networkName != "indexed-create-name" {
			t.Fatalf("the create stored %q, want the validated name", networkName)
		}

		stored, loaded := networkNameIndexed(ctx, "indexed-create-name", networkId)
		if !stored || !loaded {
			t.Fatalf("the search holds the stored name in the database %t and loaded in memory %t, want both", stored, loaded)
		}
	})
}

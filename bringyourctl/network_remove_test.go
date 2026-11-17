package main

import (
	"strings"
	"testing"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// `bringyourctl network remove` (networkRemove) calls the model directly: what
// it prints for each removal, the last Owner's refusal naming the team and what
// to do first (§7.5's last Owner, owner decision 329).

// Pure: a removal, and each refusal with its outcome.
func TestNetworkRemoveTextAnswersEachOutcome(t *testing.T) {
	networkId := server.NewId()
	for _, c := range []struct {
		outcome model.RemoveNetworkOutcome
		text    string
		removed bool
	}{
		{outcome: model.RemoveNetworkRemoved, text: "network " + networkId.String() + " removed\n", removed: true},
		{outcome: model.RemoveNetworkNotFound, text: "network " + networkId.String() + " was not removed: not_found\n"},
		{outcome: model.RemoveNetworkStripeRenewalActive, text: "network " + networkId.String() + " was not removed: stripe_renewal_active\n"},
		{outcome: model.RemoveNetworkStoreRenewalUnchecked, text: "network " + networkId.String() + " was not removed: store_renewal_unchecked\n"},
		// a last Owner's refusal without its team still refuses
		{outcome: model.RemoveNetworkLastTeamOwner, text: "network " + networkId.String() + " was not removed: last_team_owner\n"},
	} {
		text, removed := networkRemoveText(networkId, &model.RemoveNetworkResult{Outcome: c.outcome})
		if text != c.text || removed != c.removed {
			t.Errorf("%s: printed %q removed %v, want %q %v", c.outcome, text, removed, c.text, c.removed)
		}
	}
}

// Pure: the last Owner's refusal names the team, by id and name, and says to
// make another member Owner or delete the team first.
func TestNetworkRemoveTextNamesTheLastOwnersTeam(t *testing.T) {
	networkId := server.NewId()
	teamNetworkId := server.NewId()
	text, removed := networkRemoveText(networkId, &model.RemoveNetworkResult{
		Outcome: model.RemoveNetworkLastTeamOwner,
		LastOwnerTeam: &model.LastOwnerTeam{
			NetworkId:   teamNetworkId,
			NetworkName: "synthetic-team",
		},
	})
	if removed {
		t.Fatal("the last Owner's refusal reads as removed")
	}
	for _, part := range []string{
		"network " + networkId.String() + " was not removed: last_team_owner",
		"the user is the last Owner of team " + teamNetworkId.String() + " (synthetic-team)",
		"make another member Owner, or delete the team, first",
	} {
		if !strings.Contains(text, part) {
			t.Fatalf("printed %q, missing %q", text, part)
		}
	}
}

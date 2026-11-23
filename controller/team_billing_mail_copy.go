// The copy of the mails about a network's billing and payouts that reach a
// Team's Owners and Billing admins (QD-1223): the failed-payment notice
// (subscription_payment_failed) and the missing-wallet notice
// (subscription_missing_wallet), which QD-1219 sends to them, and the epoch
// earnings email (subscription_epoch_earnings) and the USDC payout-sent notice
// (subscription_usdc_payout_sent), which QD-1222 does. Each such person has a
// network of its own and may be in other Teams, and the copy of all four was
// written for a personal network's owner reading about its own account and UR
// Pro, so a Team's billing member could pay or connect a wallet for the wrong
// network, or ignore a Team's failed renewal taken for a UR Pro it does not
// have.
//
// A Team's mail names the Team, "your URnetwork Team <name>" (QD-1216's rule
// for the security notices, carried to these four); says the Team, or the
// Team's subscription, where it said you, your network or UR Pro (a Team's
// subscription is base, seats and overage, §20.9); says in its footer that it
// went to the Team's owners and billing admins; and tells the reader to act
// signed in to the Team, since the ur.io pages it links (Manage Subscription,
// Earnings, the claim) act on the network the reader is signed in to. A
// personal network's mail is unchanged.
//
// Whether the network is a Team, and its name, are read when the mail goes out
// and never stored with it: the outbox's two mails are named at delivery from
// the outbox row's network (nameAccountMessageNetwork), and the earnings email
// and the payout notice by the reads that find their recipients.
package controller

import (
	"context"

	"github.com/urnetwork/server"
)

// The network a billing or payout mail is about, as its copy names it. Neither
// field is stored with the mail: both are unexported, so the outbox's json
// neither holds nor reads them.
type billingMailNetwork struct {
	// the network is a Team; false until named, for a personal network, for a
	// network that is gone, whose mail goes out as a personal network's did,
	// and in a binary that does not link MG6, which has no Teams
	team bool
	// the Team's name as the mail shows it, on one line; "" when it has none
	// to show, and for a personal network
	teamName string
}

// Names the network as it stands when the mail goes out. A Team's mail names
// the Team; a personal network's keeps its copy, which names none.
func (self *billingMailNetwork) nameBillingMailNetwork(networkName string, team bool) {
	self.team = team
	self.teamName = ""
	if team {
		self.teamName = securityNoticeNetworkName(networkName)
	}
}

// Whether the mail is about a Team, whose copy names it.
func (self *billingMailNetwork) Team() bool {
	return self.team
}

// The Team as the copy names it: "your URnetwork Team <name>", or "your
// URnetwork Team" when it has no name to show. "" for a personal network,
// whose copy names no network.
func (self *billingMailNetwork) TeamNetwork() string {
	switch {
	case !self.team:
		return ""
	case self.teamName == "":
		return "your URnetwork Team"
	default:
		return "your URnetwork Team " + self.teamName
	}
}

// A template that names its outbox row's network as the billing and payout
// mails do: whether it is a Team, and its name.
type accountMessageBillingNamer interface {
	nameBillingMailNetwork(networkName string, team bool)
}

// The names of the networks, read in the caller's transaction in one query, as
// they stand. A network that is gone has no entry.
func billingMailNetworkNamesInTx(ctx context.Context, tx server.PgTx, networkIds []server.Id) map[server.Id]string {
	networkIdNames := map[server.Id]string{}
	if len(networkIds) == 0 {
		return networkIdNames
	}
	result, err := tx.Query(
		ctx,
		`SELECT network_id, network_name FROM network WHERE network_id = ANY($1)`,
		networkIds,
	)
	server.WithPgResult(result, err, func() {
		for result.Next() {
			var networkId server.Id
			var networkName string
			server.Raise(result.Scan(&networkId, &networkName))
			networkIdNames[networkId] = networkName
		}
	})
	return networkIdNames
}

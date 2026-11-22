// bringyourctl network remove removes a network and its users for support,
// as the account deletion does but without its store steps: no store snapshot
// is passed, so any active Stripe, App Store or Google Play renewal refuses
// the removal (cancel it at the store, or let it end, first). The removal
// queues the Brevo contact removal of every sign-in address in its own
// transaction (controller.RemoveNetworkAndContacts); until the 2026-11-13
// review this command dropped the addresses and queued nothing, so the
// contacts stayed in Brevo.
package main

import (
	"context"
	"fmt"
	"io"
	"os"

	"github.com/docopt/docopt-go"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/controller"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/session"
)

func networkRemove(opts docopt.Opts) {
	networkIdStr, _ := opts.String("--network_id")
	userIdStr, _ := opts.String("--user_id")

	networkId, err := server.ParseId(networkIdStr)
	if err != nil {
		panic(err)
	}
	userId, err := server.ParseId(userIdStr)
	if err != nil {
		panic(err)
	}
	if code := runNetworkRemove(context.Background(), os.Stdout, networkId, userId); code != 0 {
		os.Exit(code)
	}
}

// Removes the network and returns the exit code. Prints the outcome and how
// many sign-in addresses were queued for removal from Brevo, never the
// addresses.
func runNetworkRemove(ctx context.Context, stdout io.Writer, networkId server.Id, userId server.Id) int {
	// an in-process owner of no user and no address, as the queued removal
	// tasks need one
	clientSession := session.NewLocalClientSession(ctx, "", nil)
	defer clientSession.Cancel()
	outcome, userAuths := controller.RemoveNetworkAndContacts(clientSession, networkId, &userId, nil)
	if outcome != model.RemoveNetworkRemoved {
		fmt.Fprintf(stdout, "network %s was not removed: %s\n", networkId, outcome)
		return 1
	}
	fmt.Fprintf(stdout, "network %s removed; %d sign-in address(es) queued for removal from Brevo\n", networkId, len(userAuths))
	return 0
}

// bringyourctl network sign-out-all signs a network out of every device
// (model/network_sign_out_model.go; owner decision 2026-10-29, support inbox
// 7881). Without --commit it is a read-only dry run that prints the network,
// its user, its active clients, live sessions and auth codes, what a commit
// changes, and the digest a commit must name.
package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/docopt/docopt-go"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

func networkSignOutAllCommand(opts docopt.Opts) int {
	networkIdStr, _ := opts.String("--network_id")
	removeClients, _ := opts.Bool("--remove-clients")
	commit, _ := opts.Bool("--commit")
	expect, _ := opts.String("--expect")
	networkId, err := server.ParseId(networkIdStr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "network sign-out-all: --network_id: %s\n", err)
		return 1
	}
	// The dry run writes nothing to the database: its process is read only, so
	// PostgreSQL refuses any write on its path.
	if !commit {
		server.RequirePgReadOnly()
	}
	return runNetworkSignOutAll(context.Background(), os.Stdout, os.Stderr, networkId, removeClients, commit, expect)
}

// Runs the dry run or the commit and returns the exit code.
func runNetworkSignOutAll(
	ctx context.Context,
	stdout io.Writer,
	stderr io.Writer,
	networkId server.Id,
	removeClients bool,
	commit bool,
	expect string,
) int {
	if !commit {
		plan, err := model.PlanNetworkSignOut(ctx, networkId, removeClients)
		if err != nil {
			fmt.Fprintf(stderr, "%s\n", err)
			return 1
		}
		fmt.Fprintf(stdout, "network sign-out-all DRY RUN: nothing was changed\n")
		writeNetworkSignOutPlan(stdout, plan)
		if plan.Refusal != "" {
			fmt.Fprintf(stderr, "network sign-out-all: refused: %s\n", plan.Refusal)
			return 1
		}
		removeClientsFlag := ""
		if removeClients {
			removeClientsFlag = " --remove-clients"
		}
		fmt.Fprintf(stdout, "to commit exactly this: bringyourctl network sign-out-all --network_id=%s%s --commit --expect=%s\n", networkId, removeClientsFlag, plan.Digest)
		return 0
	}

	result, err := model.CommitNetworkSignOut(ctx, networkId, removeClients, expect)
	if err != nil {
		if result != nil && result.Plan != nil {
			fmt.Fprintf(stdout, "network sign-out-all COMMIT REFUSED: nothing was committed; the network as it is now:\n")
			writeNetworkSignOutPlan(stdout, result.Plan)
		}
		fmt.Fprintf(stderr, "%s\n", err)
		return 1
	}
	if result.AlreadyApplied {
		fmt.Fprintf(
			stdout,
			"network sign-out-all: digest %s was already committed by operation %s (cutoff %s, session retirement %s); nothing changed\n",
			expect,
			result.OperationId,
			formatNetworkSignOutTime(result.Cutoff),
			result.Status,
		)
		fmt.Fprintf(stdout, "%s\n", result.AuditLine())
		return 0
	}

	plan := result.Plan
	fmt.Fprintf(stdout, "network sign-out-all COMMIT\n")
	fmt.Fprintf(stdout, "signed out network %s %q: operation %s, cutoff %s\n", plan.NetworkId, plan.NetworkName, result.OperationId, formatNetworkSignOutTime(result.Cutoff))
	fmt.Fprintf(stdout, "  user %s credential_change_time %s -> %s\n", plan.UserId, formatNetworkSignOutTime(result.PreviousCredentialChangeTime), formatNetworkSignOutTime(result.Cutoff))
	fmt.Fprintf(stdout, "  credential cutoff raised in the network's 64 Redis revocation records\n")
	fmt.Fprintf(stdout, "  auth codes retired: %d\n", result.RetiredAuthCodeCount)
	if plan.RemoveClients {
		fmt.Fprintf(stdout, "  clients deactivated: %d\n", result.DeactivatedClientCount)
	}
	if result.EnforcementError != nil {
		fmt.Fprintf(stdout, "  WARNING: the session retirement is journaled but did not finish here (%s); the session maintenance task finishes it, and the rotation already refuses those sessions' tokens\n", result.EnforcementError)
	} else if result.SessionCleanupComplete {
		fmt.Fprintf(stdout, "  sessions revoked: %d; their clients' cleanup is complete\n", result.RevokedSessionCount)
	} else {
		fmt.Fprintf(stdout, "  sessions revoked: %d; the session maintenance task finishes their clients' cleanup\n", result.RevokedSessionCount)
	}
	fmt.Fprintf(stdout, "%s\n", result.AuditLine())
	return 0
}

func formatNetworkSignOutTime(t time.Time) string {
	return t.UTC().Format(time.RFC3339Nano)
}

// The census and what a commit of it changes.
func writeNetworkSignOutPlan(out io.Writer, plan *model.NetworkSignOutPlan) {
	fmt.Fprintf(out, "network %s %q\n", plan.NetworkId, plan.NetworkName)
	fmt.Fprintf(out, "user %s (the network's admin, its only user)\n", plan.UserId)
	fmt.Fprintf(out, "  account: %s %s\n", plan.UserAuthType, plan.UserAuth)
	if len(plan.SignIns) == 0 {
		fmt.Fprintf(out, "  sign-in methods: none\n")
	}
	for _, signIn := range plan.SignIns {
		fmt.Fprintf(out, "  sign-in method: %s\n", signIn)
	}
	fmt.Fprintf(out, "  credential_change_time: %s (tokens created before it are already refused)\n", formatNetworkSignOutTime(plan.CredentialChangeTime))
	if len(plan.OtherNetworkIds) == 0 {
		fmt.Fprintf(out, "  administers no other network\n")
	}
	for _, otherNetworkId := range plan.OtherNetworkIds {
		fmt.Fprintf(out, "  also administers network %s\n", otherNetworkId)
	}

	topLevelClients := plan.TopLevelClients()
	fmt.Fprintf(out, "active clients: %d (%d top-level, %d child)\n", len(plan.Clients), len(topLevelClients), plan.ChildClientCount())
	for _, client := range topLevelClients {
		sessionId := "-"
		if client.SessionId != nil {
			sessionId = client.SessionId.String()
		}
		fmt.Fprintf(out, "  client %s top-level %q, created %s, last auth %s, session %s\n", client.ClientId, client.Description, formatNetworkSignOutTime(client.CreateTime), formatNetworkSignOutTime(client.AuthTime), sessionId)
	}
	if 0 < plan.ChildClientCount() {
		fmt.Fprintf(out, "  and %d child clients, not listed: the devices create and end them as they run, so the digest binds only the top-level clients\n", plan.ChildClientCount())
	}
	fmt.Fprintf(out, "live tracked sessions: %d\n", len(plan.Sessions))
	for _, liveSession := range plan.Sessions {
		fmt.Fprintf(out, "  session %s accepted until %s\n", liveSession.SessionId, formatNetworkSignOutTime(liveSession.AcceptUntil))
	}
	fmt.Fprintf(out, "active auth codes: %d\n", len(plan.AuthCodeIds))

	if plan.Refusal != "" {
		return
	}
	fmt.Fprintf(out, "a commit, in one transaction under the network's exclusive session lifecycle lock:\n")
	fmt.Fprintf(out, "  - sets user %s credential_change_time to the commit time: every API and connect token created before it is refused, by every instance's revocation record check and by the SQL state query\n", plan.UserId)
	fmt.Fprintf(out, "  - raises the credential cutoff in the network's 64 Redis revocation records before it commits, as a password reset does\n")
	fmt.Fprintf(out, "  - journals session operation %s (reset); after the commit it revokes the %d live sessions created before the cutoff, as the session maintenance task would, and deactivates the %d active clients bound to them\n", plan.OperationId, len(plan.Sessions), plan.SessionBoundClientCount())
	fmt.Fprintf(out, "  - retires the %d active auth codes\n", len(plan.AuthCodeIds))
	if plan.RemoveClients {
		fmt.Fprintf(out, "  - deactivates every client active at the commit (now %d, at most %d) as /network/remove-clients does: revocation record tombstones, provide keys removed, connections and contracts closed\n", len(plan.Clients), model.NetworkSignOutMaxRemovedClients)
	} else {
		fmt.Fprintf(out, "  - leaves the other client rows active (add --remove-clients to deactivate them); their tokens are refused, so each device must sign in again\n")
	}
	fmt.Fprintf(out, "it never deletes the account, its sign-in methods or its data, and changes no other network. A new sign-in after the commit works as before.\n")
	fmt.Fprintf(out, "digest %s\n", plan.Digest)
}

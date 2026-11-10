package server

// The admin_user_id audit (TEAM-M6-02; R-TEAM-2, D-TEAM-2, D-TEAM-3, §7.18's
// top risk): a team network's admin_user_id is a synthetic owner of record
// that never signs in, so every read of it decides whether a team member could
// pass as the network's owner. Each read, a line of non-test Go source naming
// admin_user_id or AdminUserId outside a comment, is classified here by its
// file and the function that holds it, with how many such lines that function
// has:
//
//	owner     the owner of record: the account a network bills, mails and
//	          deletes, its creation and its fixtures. Right in a team too: the
//	          team's own account is the synthetic owner
//	network   the network a user owns, its personal network: what a sign-in or
//	          a legacy credential's repair finds. A team member reaches a team
//	          only through a credential that names it (member_id), never here
//	member    who acts in the network: each such read is member-aware (a team
//	          member's credential reads network_member) or fails closed for a
//	          team, since the owner of record never matches a member's user
//
// TestAdminUserIdReadsClassified scans the source, so a new read, or one more
// in a function already listed, fails until it is classified here.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

type adminUserIdClass string

const (
	adminUserIdOwner   adminUserIdClass = "owner"
	adminUserIdNetwork adminUserIdClass = "network"
	adminUserIdMember  adminUserIdClass = "member"
)

// One function's reads: how many lines, their class and why.
type adminUserIdRead struct {
	count int
	class adminUserIdClass
	why   string
}

// Every read, by file (from the module root) and function ("<decl>" for a
// declaration outside a function, "Type.Method" for a method).
var adminUserIdReads = map[string]map[string]adminUserIdRead{
	"apikey/apikey.go": {
		"GetNetworkByApiKey":          {count: 2, class: adminUserIdMember, why: "a personal network's key acts as its owner; MG6 binaries take getTeamAwareNetworkByApiKey"},
		"getTeamAwareNetworkByApiKey": {count: 1, class: adminUserIdMember, why: "a team key acts as its creator's live membership, else is no key"},
	},
	"bringyourctl/balance_move_redeemed.go": {
		"redeemedBalanceMoveText": {count: 1, class: adminUserIdOwner, why: "an operator's report of the account"},
	},
	"connect/sim-latency/fleet.go": {
		"Fleet.newSimProvider": {count: 2, class: adminUserIdOwner, why: "a simulation's provider networks"},
	},
	"connect/sim-latency/provision.go": {
		"provisionIdentityBatch": {count: 1, class: adminUserIdOwner, why: "a simulation creates its networks"},
	},
	"controller/auth_controller.go": {
		"NetworkRefreshToken": {count: 1, class: adminUserIdMember, why: "a member's token is checked against its membership, an owner's against admin_user_id"},
	},
	"controller/config_log_controller.go": {
		"configGenesisOwner": {count: 2, class: adminUserIdMember, why: "base 1's owner: a team's live Owner membership, else the personal network's owner"},
	},
	"controller/network_controller.go": {
		"NetworkRemove": {count: 1, class: adminUserIdMember, why: "deletion is the owner's: refused to every team member until TEAM-M6-03's Owner permission"},
	},
	"controller/product_updates_controller.go": {
		"mirrorNetworkMarketingSuppressions":       {count: 1, class: adminUserIdOwner, why: "the account's marketing mail"},
		"scheduleSyncProductUpdatesForNetworkInTx": {count: 1, class: adminUserIdOwner, why: "the account's marketing mail"},
	},
	"db_migrations.go": {
		"<decl>": {count: 5, class: adminUserIdOwner, why: "the column and its indexes"},
	},
	"model/abuse_action_model.go": {
		"AbuseNoticeRecipientsInTx": {count: 3, class: adminUserIdOwner, why: "§21's notices go to the account"},
	},
	"model/account_api_key_model.go": {
		"Test_GetNetworkByApiKey": {count: 1, class: adminUserIdOwner, why: "a test helper reading the key's network"},
	},
	"model/account_model.go": {
		"FindNetworksByName":              {count: 1, class: adminUserIdOwner, why: "an operator's search for the account"},
		"FindNetworksByUserAuth":          {count: 1, class: adminUserIdNetwork, why: "the network a sign-in owns"},
		"RemoveNetworkWithStoreSnapshot":  {count: 3, class: adminUserIdOwner, why: "the account's deletion"},
		"lockPaymentNetworkForRemoveInTx": {count: 3, class: adminUserIdOwner, why: "the account's deletion"},
	},
	"model/account_preferences_model.go": {
		"AccountProductUpdatesSetForEmail":         {count: 1, class: adminUserIdOwner, why: "the account's mail preferences"},
		"AccountProductUpdatesUnsubscribeForEmail": {count: 1, class: adminUserIdOwner, why: "the account's mail preferences"},
	},
	"model/auth_code_session.go": {
		"authCodeLoginSession": {count: 1, class: adminUserIdMember, why: "a code logs in as its network's owner only: a member's code fails closed"},
	},
	"model/auth_model.go": {
		"<decl>":                {count: 1, class: adminUserIdNetwork, why: "the network a sign-in owns"},
		"AuthLoginWithPassword": {count: 1, class: adminUserIdNetwork, why: "a password sign-in mints for the user's own network; a team is reached by switching"},
		"AuthPasswordSet":       {count: 1, class: adminUserIdNetwork, why: "the user's own network's credential cutoff"},
		"AuthVerify":            {count: 1, class: adminUserIdNetwork, why: "a verification sign-in mints for the user's own network"},
		"GetUserAuthsInTx":      {count: 1, class: adminUserIdOwner, why: "the account's sign-ins"},
		"getUserAuth":           {count: 1, class: adminUserIdOwner, why: "the account's sign-in"},
		"handleLoginWallet":     {count: 1, class: adminUserIdNetwork, why: "a wallet sign-in mints for the user's own network"},
	},
	"model/device_association_model.go": {
		"DeviceConfirmAdopt": {count: 1, class: adminUserIdOwner, why: "the adopting account's sign-in kind"},
	},
	"model/lag_guest_model.go": {
		"CreateLagGuestNetwork": {count: 1, class: adminUserIdOwner, why: "a guest network's creation"},
		"removeExpiredLagGuest": {count: 1, class: adminUserIdOwner, why: "a guest network's removal"},
	},
	"model/network_member_auth_model.go": {
		"GetNetworkSignInsOfUser": {count: 1, class: adminUserIdNetwork, why: "the user's own network beside its memberships"},
		"ResolveNetworkSignIn":    {count: 1, class: adminUserIdMember, why: "a personal network's owner, else a team's live membership"},
	},
	"model/network_model.go": {
		"<decl>":                           {count: 1, class: adminUserIdOwner, why: "Network's field"},
		"GetNetwork":                       {count: 2, class: adminUserIdOwner, why: "the network's row"},
		"Testing_CreateGuestNetwork":       {count: 1, class: adminUserIdOwner, why: "a fixture"},
		"Testing_CreateLegacyGuestNetwork": {count: 1, class: adminUserIdOwner, why: "a fixture"},
		"Testing_CreateNetwork":            {count: 1, class: adminUserIdOwner, why: "a fixture"},
		"Testing_CreateNetworkByWallet":    {count: 1, class: adminUserIdOwner, why: "a fixture"},
		"Testing_CreateNetworkSso":         {count: 1, class: adminUserIdOwner, why: "a fixture"},
		"getNetworkAdminUserAuth":          {count: 1, class: adminUserIdOwner, why: "the account's sign-in"},
		"networkCreateAuthJwt":             {count: 1, class: adminUserIdOwner, why: "a personal network's creation"},
		"networkCreateSeedphrase":          {count: 1, class: adminUserIdOwner, why: "a personal network's creation"},
		"networkCreateUserAuth":            {count: 1, class: adminUserIdOwner, why: "a personal network's creation"},
		"networkCreateWalletAuth":          {count: 1, class: adminUserIdOwner, why: "a personal network's creation"},
	},
	"model/network_rename_model.go": {
		"networkRenamer.rename": {count: 1, class: adminUserIdMember, why: "the owner renames: refused to every team member until TEAM-M6-03's Admin permission"},
	},
	"model/network_sign_out_model.go": {
		"planNetworkSignOutInTx": {count: 3, class: adminUserIdOwner, why: "the account's sign-out of every session"},
	},
	"model/network_team_model.go": {
		"networkTeams.create": {count: 1, class: adminUserIdOwner, why: "a team's synthetic owner of record (D-TEAM-2)"},
	},
	"model/network_user_model.go": {
		"FindNetworkIdByWalletAddress": {count: 1, class: adminUserIdNetwork, why: "the network a wallet sign-in owns"},
		"GetNetworkUser":               {count: 1, class: adminUserIdNetwork, why: "the user's account and the name of the network it owns"},
	},
	"model/network_wallet_mapping_consent.go": {
		"checkNetworkWalletMappingOwner": {count: 1, class: adminUserIdOwner, why: "the account's wallet consent"},
	},
	"model/onboarding_campaign_model.go": {
		"NetworkEnrollmentFacts": {count: 1, class: adminUserIdOwner, why: "the account's onboarding"},
	},
	"model/prober_identity_model.go": {
		"createProberNetwork":   {count: 1, class: adminUserIdOwner, why: "an internal prober network"},
		"getNetworkAdminUserId": {count: 2, class: adminUserIdOwner, why: "an internal prober network"},
	},
	"model/prober_shard_model.go": {
		"BeginProberShard": {count: 1, class: adminUserIdOwner, why: "an internal prober network"},
		"reapProberShard":  {count: 1, class: adminUserIdOwner, why: "an internal prober network"},
	},
	"model/product_updates_model.go": {
		"<decl>":                    {count: 1, class: adminUserIdOwner, why: "the account's marketing mail"},
		"GetNetworkAdminUserIdInTx": {count: 2, class: adminUserIdOwner, why: "the account's marketing mail"},
		"GetNetworkUserEmailsForProductUpdatesSync": {count: 2, class: adminUserIdOwner, why: "the account's marketing mail"},
		"GetProductUpdateUserEmailsForUser":         {count: 1, class: adminUserIdOwner, why: "the account's marketing mail"},
		"GetUserEmailsForProductUpdatesSync":        {count: 2, class: adminUserIdOwner, why: "the account's marketing mail"},
		"ListMarketingSuppressedNetworkAdmins":      {count: 2, class: adminUserIdOwner, why: "the account's marketing mail"},
	},
	"model/reengagement_model.go": {
		"<decl>": {count: 1, class: adminUserIdOwner, why: "the account's reengagement mail"},
	},
	"model/security_notice_model.go": {
		"<decl>":                    {count: 3, class: adminUserIdOwner, why: "security notices go to the account's sign-ins"},
		"securityNoticeNetworkInTx": {count: 1, class: adminUserIdNetwork, why: "the network whose account a user's sign-in change concerns"},
	},
	"model/seedphrase_auth_model.go": {
		"LoginWithSeedphrase": {count: 1, class: adminUserIdNetwork, why: "a seedphrase sign-in mints for the user's own network"},
	},
	"model/sn_fleet_auto.go": {
		"SetSnFleetAutoAuthorization": {count: 1, class: adminUserIdOwner, why: "the account's fleet authorization"},
	},
	"model/stripe_invoice_payment_failure_model.go": {
		"GetAccountEmailInTx": {count: 2, class: adminUserIdOwner, why: "the account's billing mail"},
	},
	"model/test_balance_drain_model.go": {
		"<decl>": {count: 2, class: adminUserIdOwner, why: "the account's balance"},
	},
	"model/transfer_balance_code_move_model.go": {
		"<decl>":                         {count: 1, class: adminUserIdOwner, why: "the account's balance move"},
		"readRedeemedBalanceMoveNetwork": {count: 3, class: adminUserIdOwner, why: "the account's balance move"},
	},
	"session/by_jwt.go": {
		"LoadByJwtFromClientId":   {count: 1, class: adminUserIdMember, why: "a client's owner credential: refused for a team's client"},
		"getNetworkIdForUser":     {count: 1, class: adminUserIdNetwork, why: "a legacy credential's repair; never a member credential"},
		"validateByJwtStateQuery": {count: 2, class: adminUserIdMember, why: "an owner's credential; a member credential takes validateMemberByJwtStateQuery"},
	},
	"session/revocation_record.go": {
		"RotateCredentialRecordsInTx": {count: 1, class: adminUserIdOwner, why: "the owner's records; member credentials stay on the query"},
		"readRevocationRecordFacts":   {count: 1, class: adminUserIdMember, why: "an owner's record; a member credential never uses a record"},
	},
	"session/session_operation.go": {
		"EnforceSessionOperation": {count: 1, class: adminUserIdMember, why: "an owner's operation: a member actor is cancelled, failing closed"},
	},
}

var adminUserIdPattern = regexp.MustCompile(`admin_user_id|AdminUserId`)

// The reads in the module's non-test Go source, by file and function.
func scanAdminUserIdReads(t *testing.T, root string) map[string]map[string]int {
	t.Helper()
	reads := map[string]map[string]int{}
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			name := entry.Name()
			if path != root && (name == "vendor" || name == "testdata" || strings.HasPrefix(name, ".")) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		src, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if !adminUserIdPattern.Match(src) {
			return nil
		}
		fileSet := token.NewFileSet()
		file, err := parser.ParseFile(fileSet, path, src, parser.SkipObjectResolution)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		for i, line := range strings.Split(string(src), "\n") {
			if !adminUserIdPattern.MatchString(line) || strings.HasPrefix(strings.TrimSpace(line), "//") {
				continue
			}
			lineNumber := i + 1
			holder := "<file>"
			for _, decl := range file.Decls {
				if lineNumber < fileSet.Position(decl.Pos()).Line || fileSet.Position(decl.End()).Line < lineNumber {
					continue
				}
				switch d := decl.(type) {
				case *ast.FuncDecl:
					holder = d.Name.Name
					if d.Recv != nil && 0 < len(d.Recv.List) {
						receiver := d.Recv.List[0].Type
						if star, ok := receiver.(*ast.StarExpr); ok {
							receiver = star.X
						}
						if ident, ok := receiver.(*ast.Ident); ok {
							holder = ident.Name + "." + holder
						}
					}
				case *ast.GenDecl:
					holder = "<decl>"
				}
			}
			if reads[rel] == nil {
				reads[rel] = map[string]int{}
			}
			reads[rel][holder] += 1
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return reads
}

// Every read of admin_user_id is classified, with its count, and every
// classified read still exists.
func TestAdminUserIdReadsClassified(t *testing.T) {
	root, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	reads := scanAdminUserIdReads(t, root)
	files := []string{}
	for file := range reads {
		files = append(files, file)
	}
	sort.Strings(files)
	total := 0
	for _, file := range files {
		for holder, count := range reads[file] {
			total += count
			classified, ok := adminUserIdReads[file][holder]
			if !ok {
				t.Errorf("%s %s reads admin_user_id %d times, unclassified: say whether it is the owner of record, the user's own network or who acts in the network, and make a member read member-aware or fail closed for a team", file, holder, count)
				continue
			}
			if classified.count != count {
				t.Errorf("%s %s reads admin_user_id %d times, classified for %d: classify the new read", file, holder, count, classified.count)
			}
			switch classified.class {
			case adminUserIdOwner, adminUserIdNetwork, adminUserIdMember:
			default:
				t.Errorf("%s %s has no class", file, holder)
			}
			if classified.why == "" {
				t.Errorf("%s %s says nothing of why", file, holder)
			}
		}
	}
	for file, holders := range adminUserIdReads {
		for holder := range holders {
			if reads[file][holder] == 0 {
				t.Errorf("%s %s is classified and reads admin_user_id no more: take it out", file, holder)
			}
		}
	}
	if total == 0 {
		t.Fatal("the scan found no read, so it read no source")
	}
}

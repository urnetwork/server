package main

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"math/big"
	"os"
	"strconv"

	"slices"
	"strings"
	"time"

	"golang.org/x/exp/maps"

	"github.com/docopt/docopt-go"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/search"
	"github.com/urnetwork/server/session"
	"github.com/urnetwork/server/task"

	"github.com/urnetwork/proxy"
)

func main() {
	usage := `BringYour control.

Usage:
    bringyourctl db version
    bringyourctl db migrate
    bringyourctl db maintenance <epoch> [--reindex] [--cleanup] [--analyze]
    bringyourctl search --realm=<realm> --type=<type> add <value>
    bringyourctl search --realm=<realm> --type=<type> around --distance=<distance> <value>
    bringyourctl search --realm=<realm> --type=<type> remove <value>
    bringyourctl search --realm=<realm> --type=<type> clear
    bringyourctl stats compute
    bringyourctl stats export
    bringyourctl stats import
    bringyourctl stats add
    bringyourctl locations add-default [-a]
    bringyourctl network find [--user_auth=<user_auth>] [--network_name=<network_name>]
    bringyourctl network remove --network_id=<network_id> --user_id=<user_id>
    bringyourctl balance-code create --duration=<duration> --balance=<balance> --cost=<usd> --email=<email> [--count=<count>]
    bringyourctl balance-code check --secret=<secret>
    bringyourctl send network-welcome --user_auth=<user_auth>
    bringyourctl send auth-verify --user_auth=<user_auth>
    bringyourctl send auth-password-reset --user_auth=<user_auth>
    bringyourctl send auth-password-set --user_auth=<user_auth>
    bringyourctl send subscription-transfer-balance-code --user_auth=<user_auth>
    bringyourctl send payout-email --user_auth=<user_auth>
    bringyourctl send network-user-interview-request-1 --user_auth=<user_auth>
    bringyourctl payout single --account_payment_id=<account_payment_id>
    bringyourctl payout pending
    bringyourctl payouts list-pending [--plan_id=<plan_id>]
    bringyourctl payouts apply-bonus --plan_id=<plan_id> --amount_usd=<amount_usd>
    bringyourctl payouts plan [--send] [--dry-run] [--max_duration=<max_duration>]
    bringyourctl payouts populate-tx-hashes
    bringyourctl wallet estimate-fee --amount_usd=<amount_usd> --destination_address=<destination_address> --blockchain=<blockchain>
    bringyourctl wallet transfer --amount_usd=<amount_usd> --destination_address=<destination_address> --blockchain=<blockchain>
		bringyourctl wallets sync-circle
    bringyourctl contracts close-expired [-c <count>]
    bringyourctl contracts close --contract_id=<contract_id> --target_id=<target_id> --used_transfer_byte_count=<used_transfer_byte_count>
    bringyourctl contracts reconcile-net-escrow [--network_id=<network_id>] [--dry-run]
    bringyourctl task ls
    bringyourctl task rm <task_id>
    bringyourctl auth login <auth_code>
    bringyourctl account-points populate --plan_id=<plan_id>
    bringyourctl client connection fix
    bringyourctl migrate-user-auth
    bringyourctl reliability set-multipliers
    bringyourctl product-updates sync
    bringyourctl query location <query>
    bringyourctl upgrade-plan --network_id=<network_id>
    bringyourctl proxy parse-id <signed_proxy_id>
    bringyourctl proxy keygen
    bringyourctl proxy reset-client-ipv4
    bringyourctl proxy inspect <proxy_id>
    bringyourctl model migrate provide-mode
    bringyourctl model migrate proxy-device-config
    bringyourctl refresh-transfer-balances
    bringyourctl st status [--epoch=<epoch>]
    bringyourctl st deposit [--alpha_rao=<alpha_rao>]
    bringyourctl st commit --epoch=<epoch>
    bringyourctl st finalize --epoch=<epoch>
    bringyourctl grafana load-defaults [--grafana_url=<grafana_url>]

Options:
    -h --help     Show this screen.
    --version     Show version.
    -r --realm=<realm>  Search realm.
    -t --type=<type>    Search type.
    -d, --distance=<distance>  Search distance.
    -a            All locations.
    --user_auth=<user_auth>
    --network_name=<network_name>
    --network_id=<network_id>
    --user_id=<user_id>
    --secret=<secret>

    --contract_id=<contract_id> The contract to close
    --target_id=<target_id>     The contract_close.party (either "destination" or "source")
    --used_transfer_byte_count=<used_transfer_byte_count>   Bytes used
    --amount_usd=<amount_usd>   Amount in USD.
    --destination_address=<destination_address>  Destination address.
    --blockchain=<blockchain>  Blockchain.
    --max_duration=<max_duration>  Bound a payout plan to the first <max_duration> of contract close time after the most recent subsidy epoch, draining a backlog forward one slice per run, e.g. 14d, 1.5d, 336h.
    -c --count=<count>	Number to process [default: 1000].`

	opts, err := docopt.ParseArgs(usage, os.Args[1:], server.RequireVersion())
	if err != nil {
		panic(err)
	}

	if db, _ := opts.Bool("db"); db {
		if version, _ := opts.Bool("version"); version {
			dbVersion(opts)
		} else if migrate, _ := opts.Bool("migrate"); migrate {
			dbMigrate(opts)
		} else if maintenance, _ := opts.Bool("maintenance"); maintenance {
			dbMaintenance(opts)
		}
	} else if search, _ := opts.Bool("search"); search {
		if add, _ := opts.Bool("add"); add {
			searchAdd(opts)
		} else if around, _ := opts.Bool("around"); around {
			searchAround(opts)
		} else if remove, _ := opts.Bool("remove"); remove {
			searchRemove(opts)
		} else if clear, _ := opts.Bool("clear"); clear {
			searchClear(opts)
		}
	} else if stats, _ := opts.Bool("stats"); stats {
		if compute, _ := opts.Bool("compute"); compute {
			statsCompute(opts)
		} else if export, _ := opts.Bool("export"); export {
			statsExport(opts)
		} else if import_, _ := opts.Bool("import"); import_ {
			statsImport(opts)
		} else if add, _ := opts.Bool("add"); add {
			statsAdd(opts)
		}
	} else if locations, _ := opts.Bool("locations"); locations {
		if addDefault, _ := opts.Bool("add-default"); addDefault {
			locationsAddDefault(opts)
		}
	} else if network, _ := opts.Bool("network"); network {
		if find, _ := opts.Bool("find"); find {
			networkFind(opts)
		} else if remove, _ := opts.Bool("remove"); remove {
			networkRemove(opts)
		}
	} else if network, _ := opts.Bool("balance-code"); network {
		if create, _ := opts.Bool("create"); create {
			balanceCodeCreate(opts)
		} else if check, _ := opts.Bool("check"); check {
			balanceCodeCheck(opts)
		}
	} else if send, _ := opts.Bool("send"); send {
		if networkWelcome, _ := opts.Bool("network-welcome"); networkWelcome {
			sendNetworkWelcome(opts)
		} else if authVerify, _ := opts.Bool("auth-verify"); authVerify {
			sendAuthVerify(opts)
		} else if authPasswordReset, _ := opts.Bool("auth-password-reset"); authPasswordReset {
			sendAuthPasswordReset(opts)
		} else if authPasswordSet, _ := opts.Bool("auth-password-set"); authPasswordSet {
			sendAuthPasswordSet(opts)
		} else if subscriptionTransferBalanceCode, _ := opts.Bool("subscription-transfer-balance-code"); subscriptionTransferBalanceCode {
			sendSubscriptionTransferBalanceCode(opts)
		} else if payoutEmail, _ := opts.Bool("payout-email"); payoutEmail {
			sendPayoutEmail(opts)
		} else if networkUserInterviewRequest1, _ := opts.Bool("network-user-interview-request-1"); networkUserInterviewRequest1 {
			sendNetworkUserInterviewRequest1(opts)
		}
	} else if payout, _ := opts.Bool("payout"); payout {
		if single, _ := opts.Bool("single"); single {
			payoutByPaymentId(opts)
		} else if pending, _ := opts.Bool("pending"); pending {
			payoutPending()
		}
	} else if payouts, _ := opts.Bool("payouts"); payouts {
		if listPending, _ := opts.Bool("list-pending"); listPending {
			listPendingPayouts(opts)
		} else if plan, _ := opts.Bool("plan"); plan {
			planPayouts(opts)
		} else if applyBonus, _ := opts.Bool("apply-bonus"); applyBonus {
			payoutPlanApplyBonus(opts)
		} else if ptxh, _ := opts.Bool("populate-tx-hashes"); ptxh {
			populateTxHashes()
		}
	} else if wallet, _ := opts.Bool("wallet"); wallet {
		if send, _ := opts.Bool("transfer"); send {
			adminWalletTransfer(opts)
		}
		if estimate, _ := opts.Bool("estimate-fee"); estimate {
			adminWalletEstimateFee(opts)
		}
	} else if contracts, _ := opts.Bool("contracts"); contracts {
		if closeExpired, _ := opts.Bool("close-expired"); closeExpired {
			closeExpiredContracts(opts)
		}
		if close, _ := opts.Bool("close"); close {
			closeContract(opts)
		}
		if reconcile, _ := opts.Bool("reconcile-net-escrow"); reconcile {
			reconcileNetEscrow(opts)
		}
	} else if wallets, _ := opts.Bool("wallets"); wallets {
		if syncCircle, _ := opts.Bool("sync-circle"); syncCircle {
			populateMissingCircleAccountWallets()
		}
	} else if task, _ := opts.Bool("task"); task {
		if ls, _ := opts.Bool("ls"); ls {
			taskLs(opts)
		} else if rm, _ := opts.Bool("rm"); rm {
			taskRm(opts)
		}
	} else if auth, _ := opts.Bool("auth"); auth {
		if login, _ := opts.Bool("login"); login {
			authLogin(opts)
		}
	} else if accountPoints, _ := opts.Bool("account-points"); accountPoints {
		if populate, _ := opts.Bool("populate"); populate {
			accountPointsPopulate(opts)
		}
		// } else if accountPoints, _ := opts.Bool("client"); accountPoints {
		// 	if populate, _ := opts.Bool("connection"); populate {
		// 		if fix, _ := opts.Bool("fix"); fix {
		// 			fixClientConnections(opts)
		// 		}
		// 	}
		// }
	} else if auth, _ := opts.Bool("migrate-user-auth"); auth {
		migrateUserAuth(opts)
	} else if reliability, _ := opts.Bool("reliability"); reliability {
		if setMultipliers, _ := opts.Bool("set-multipliers"); setMultipliers {
			reliabilitySetMultipliers(opts)
		}
	} else if productUdpates, _ := opts.Bool("product-updates"); productUdpates {
		if sync_, _ := opts.Bool("sync"); sync_ {
			productUpdatesSync(opts)
		}
	} else if search_, _ := opts.Bool("query"); search_ {
		searchQuery(opts)
	} else if upgradePlan_, _ := opts.Bool("upgrade-plan"); upgradePlan_ {
		upgradePlan(opts)
	} else if proxy, _ := opts.Bool("proxy"); proxy {
		if parseId, _ := opts.Bool("parse-id"); parseId {
			proxyParseId(opts)
		} else if keygen, _ := opts.Bool("keygen"); keygen {
			proxyKeygen(opts)
		} else if r, _ := opts.Bool("reset-client-ipv4"); r {
			proxyResetClientIpv4(opts)
		} else if inspect, _ := opts.Bool("inspect"); inspect {
			proxyInspect(opts)
		}
	} else if model_, _ := opts.Bool("model"); model_ {
		if migrate, _ := opts.Bool("migrate"); migrate {
			if provideMode, _ := opts.Bool("provide-mode"); provideMode {
				modelMigrateProvideMode(opts)
			} else if proxyDeviceConfig, _ := opts.Bool("proxy-device-config"); proxyDeviceConfig {
				modelMigrateProxyDeviceConfig(opts)
			}
		}
	} else if refreshTransferBalances_, _ := opts.Bool("refresh-transfer-balances"); refreshTransferBalances_ {
		refreshTransferBalances(opts)
	} else if st_, _ := opts.Bool("st"); st_ {
		// manual ops fallback for the st epoch pipeline (D-3/D-11)
		if status, _ := opts.Bool("status"); status {
			stStatus(opts)
		} else if deposit, _ := opts.Bool("deposit"); deposit {
			stDeposit(opts)
		} else if commit, _ := opts.Bool("commit"); commit {
			stCommit(opts)
		} else if finalize, _ := opts.Bool("finalize"); finalize {
			stFinalize(opts)
		}
	} else if grafana_, _ := opts.Bool("grafana"); grafana_ {
		if loadDefaults, _ := opts.Bool("load-defaults"); loadDefaults {
			grafanaLoadDefaults(opts)
		}
	} else {
		fmt.Println(usage)
	}
}

func dbVersion(opts docopt.Opts) {
	version := server.DbVersion(context.Background())
	fmt.Printf("Current DB version: %d\n", version)
}

func dbMigrate(opts docopt.Opts) {
	fmt.Printf("Applying DB migrations ...\n")
	server.DbMigrationVerbose = true
	server.ApplyDbMigrations(context.Background())
}

func dbMaintenance(opts docopt.Opts) {
	epoch, _ := opts.Int("<epoch>")

	reindex, _ := opts.Bool("--reindex")
	cleanup, _ := opts.Bool("--cleanup")
	analyze, _ := opts.Bool("--analyze")

	dbMaintenanceOpts := &server.DbMaintenanceOptions{
		Reindex: reindex,
		Cleanup: cleanup,
		Analyze: analyze,
	}

	fmt.Printf("Running DB maintenance ...\n")
	// server.DbMigrationVerbose = true
	server.DbMaintenance(context.Background(), uint64(epoch), dbMaintenanceOpts)
}

func searchAdd(opts docopt.Opts) {
	realm, _ := opts.String("--realm")
	searchType, _ := opts.String("--type")

	searchService := search.NewSearchDb(
		realm,
		search.SearchType(searchType),
	)

	value, _ := opts.String("<value>")
	valueId := server.NewId()
	searchService.Add(context.Background(), value, valueId, 0)
}

func searchAround(opts docopt.Opts) {
	realm, _ := opts.String("--realm")
	searchType, _ := opts.String("--type")
	distance, _ := opts.Int("--distance")

	searchService := search.NewSearchDb(
		realm,
		search.SearchType(searchType),
	)

	value, _ := opts.String("<value>")
	searchResults := searchService.Around(context.Background(), value, distance)
	for _, searchResult := range searchResults {
		fmt.Printf("%d %s %s\n", searchResult.ValueDistance, searchResult.Value, searchResult.ValueId)
	}
}

func searchRemove(opts docopt.Opts) {
	// fixme
}

func searchClear(opts docopt.Opts) {
	// fixme
}

func statsCompute(opts docopt.Opts) {
	stats := model.ComputeStats90(context.Background())
	statsJson, err := json.MarshalIndent(stats, "", "  ")
	server.Raise(err)
	fmt.Printf("%s\n", statsJson)
}

func statsExport(opts docopt.Opts) {
	ctx := context.Background()
	stats := model.ComputeStats(ctx, 90)
	model.ExportStats(ctx, stats)
}

func statsImport(opts docopt.Opts) {
	stats := model.GetExportedStats(context.Background(), 90)
	if stats != nil {
		statsJson, err := json.MarshalIndent(stats, "", "  ")
		server.Raise(err)
		fmt.Printf("%s\n", statsJson)
	}
}

func statsAdd(opts docopt.Opts) {
	controller.AddSampleEvents(context.Background(), 4*60)
}

func locationsAddDefault(opts docopt.Opts) {
	ctx := context.Background()
	cityLimit := 0
	if all, _ := opts.Bool("-a"); all {
		cityLimit = -1
	}
	model.AddDefaultLocations(ctx, cityLimit)
}

func networkFind(opts docopt.Opts) {
	ctx := context.Background()

	userAuth, _ := opts.String("--user_auth")
	networkName, _ := opts.String("--network_name")

	list := func(results []*model.FindNetworkResult) {
		for _, result := range results {
			fmt.Printf("%s %s\n", result.NetworkId, result.NetworkName)
			for i, a := range result.UserAuths {
				fmt.Printf("    user[%2d] %s\n", i, a)
			}
		}
	}

	if userAuth != "" {
		results, err := model.FindNetworksByUserAuth(ctx, userAuth)
		if err != nil {
			panic(err)
		}
		list(results)
	}
	if networkName != "" {
		results, err := model.FindNetworksByName(ctx, networkName)
		if err != nil {
			panic(err)
		}
		list(results)
	}
}

func networkRemove(opts docopt.Opts) {
	ctx := context.Background()

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
	model.RemoveNetwork(ctx, networkId, &userId)
}

func balanceCodeCreate(opts docopt.Opts) {
	ctx := context.Background()

	count := 1
	if countStr, err := opts.String("--count"); err == nil {
		_, err = fmt.Sscanf(countStr, "%d", &count)
		if err != nil {
			panic(err)
		}
	}

	durationStr, _ := opts.String("--duration")

	duration, err := time.ParseDuration(durationStr)
	if err != nil {
		panic(err)
	}

	balanceStr, _ := opts.String("--balance")

	balanceByteCount, err := model.ParseByteCount(balanceStr)
	if err != nil {
		panic(err)
	}

	usdStr, _ := opts.String("--cost")

	var usd float64
	_, err = fmt.Sscanf(usdStr, "%f", &usd)
	if err != nil {
		panic(err)
	}

	costNanoCents := model.UsdToNanoCents(usd)

	email, _ := opts.String("--email")

	for range count {
		balanceCode, err := model.CreateBalanceCode(
			ctx,
			balanceByteCount,
			duration,
			costNanoCents,
			server.NewId().String(),
			server.NewId().String(),
			email,
		)
		if err != nil {
			panic(err)
		}
		fmt.Printf("%s\n", balanceCode.Secret)
	}
}

func balanceCodeCheck(opts docopt.Opts) {
	secret, _ := opts.String("--secret")

	ctx := context.Background()

	clientSession := session.NewLocalClientSession(ctx, "0.0.0.0:0", nil)

	result, err := model.CheckBalanceCode(
		&model.CheckBalanceCodeArgs{
			Secret: secret,
		},
		clientSession,
	)
	if err != nil {
		panic(err)
	}
	if b, err := json.MarshalIndent(result, "", "  "); err == nil {
		fmt.Printf("%s\n", b)
	} else {
		fmt.Printf("%+v\n", result)
	}
}

func sendNetworkWelcome(opts docopt.Opts) {
	userAuth, _ := opts.String("--user_auth")

	awsMessageSender := controller.GetAWSMessageSender()

	err := awsMessageSender.SendAccountMessageTemplate(
		userAuth,
		&controller.NetworkWelcomeTemplate{},
	)
	if err != nil {
		panic(err)
	}
	fmt.Printf("Sent\n")
}

func sendAuthVerify(opts docopt.Opts) {
	userAuth, _ := opts.String("--user_auth")

	awsMessageSender := controller.GetAWSMessageSender()

	code := model.Testing_CreateVerifyCode()

	err := awsMessageSender.SendAccountMessageTemplate(
		userAuth,
		&controller.AuthVerifyTemplate{
			VerifyCode: code,
		},
	)
	if err != nil {
		panic(err)
	}
	fmt.Printf("Sent\n")
}

func sendAuthPasswordReset(opts docopt.Opts) {
	userAuth, _ := opts.String("--user_auth")

	awsMessageSender := controller.GetAWSMessageSender()

	err := awsMessageSender.SendAccountMessageTemplate(
		userAuth,
		&controller.AuthPasswordResetTemplate{
			ResetCode: "abcdefghij",
		},
	)
	if err != nil {
		panic(err)
	}
	fmt.Printf("Sent\n")
}

func sendAuthPasswordSet(opts docopt.Opts) {
	userAuth, _ := opts.String("--user_auth")

	awsMessageSender := controller.GetAWSMessageSender()

	err := awsMessageSender.SendAccountMessageTemplate(
		userAuth,
		&controller.AuthPasswordSetTemplate{},
	)
	if err != nil {
		panic(err)
	}
	fmt.Printf("Sent\n")
}

// func sendSubscriptionTransferBalanceCode(opts docopt.Opts) {
//     userAuth, _ := opts.String("--user_auth")

//     awsMessageSender := controller.GetAWSMessageSender()

//     err := awsMessageSender.SendAccountMessageTemplate(
//         userAuth,
//         &controller.SubscriptionTransferBalanceCodeTemplate{
//             Secret: "hi there bar now",
//         },
//     )
//     if err != nil {
//         panic(err)
//     }
//     fmt.Printf("Sent\n")
// }

func sendSubscriptionTransferBalanceCode(opts docopt.Opts) {

	ctx := context.Background()

	userAuth, _ := opts.String("--user_auth")

	awsMessageSender := controller.GetAWSMessageSender()

	balanceByteCount := model.ByteCount(10 * 1024 * 1024 * 1024 * 1024)
	netRevenue := model.UsdToNanoCents(100.0)

	balanceCode, err := model.CreateBalanceCode(
		ctx,
		balanceByteCount,
		365*24*time.Hour,
		netRevenue,
		server.NewId().String(),
		server.NewId().String(),
		userAuth,
	)

	if err != nil {
		panic(err)
	}

	err = awsMessageSender.SendAccountMessageTemplate(
		userAuth,
		&controller.SubscriptionTransferBalanceCodeTemplate{
			Secret:           balanceCode.Secret,
			BalanceByteCount: balanceByteCount,
		},
	)
	if err != nil {
		panic(err)
	}
	fmt.Printf("Sent\n")
}

func sendPayoutEmail(opts docopt.Opts) {
	userAuth, _ := opts.String("--user_auth")

	awsMessageSender := controller.GetAWSMessageSender()

	err := awsMessageSender.SendAccountMessageTemplate(
		userAuth,
		&controller.SendPaymentTemplate{
			PaymentId:          server.NewId(),
			TxHash:             "0x1234567890",
			ExplorerBasePath:   "https://explorer.solana.com/tx",
			ReferralCode:       server.NewId().String(),
			Blockchain:         "Solana",
			DestinationAddress: "0x1234567890",
			AmountUsd:          "5.00",
			PaymentCreatedAt:   time.Now().UTC(),
		},
	)
	if err != nil {
		panic(err)
	}
	fmt.Printf("Sent\n")
}

func sendNetworkUserInterviewRequest1(opts docopt.Opts) {
	userAuth, _ := opts.String("--user_auth")

	awsMessageSender := controller.GetAWSMessageSender()

	err := awsMessageSender.SendAccountMessageTemplate(
		userAuth,
		&controller.NetworkUserInterviewRequest1Template{},
		controller.SenderEmail("brien@brienyour.com"),
	)
	if err != nil {
		panic(err)
	}
	fmt.Printf("Sent\n")
}

func planPayouts(opts docopt.Opts) {
	send, _ := opts.Bool("--send")
	dryRun, _ := opts.Bool("--dry-run")
	maxDurationStr, _ := opts.String("--max_duration")

	if send && dryRun {
		fmt.Println("--dry-run cannot be combined with --send")
		return
	}

	// --max_duration bounds the planning window to keep a single plan small.
	// --send runs the automated payout path, which is intentionally unbounded,
	// so the two cannot be combined.
	var maxDuration time.Duration
	if maxDurationStr != "" {
		if send {
			fmt.Println("--max_duration cannot be combined with --send")
			return
		}
		var err error
		maxDuration, err = server.ParseDurationExtended(maxDurationStr)
		if err != nil {
			fmt.Printf("invalid --max_duration %q: %s\n", maxDurationStr, err)
			return
		}
		// Each slice advances the frontier only by forming a new subsidy epoch,
		// and an epoch shorter than the minimum subsidy duration is dropped. A
		// window at or below that minimum could never advance, so reject it up
		// front instead of silently replanning the same slice forever.
		minDuration := model.EnvSubsidyConfig().MinDurationPerPayout()
		if maxDuration <= minDuration {
			fmt.Printf("--max_duration must be greater than the minimum subsidy duration %s so each slice forms a subsidy epoch and the payout frontier advances\n", minDuration)
			return
		}
	}

	ctx := context.Background()

	if send {
		glog.Infof("[payouts]send\n")
		clientSession := session.NewLocalClientSession(ctx, "0.0.0.0:0", nil)
		controller.SendPayments(clientSession)
		return
	}

	// A real plan (no --dry-run) persists the plan: it creates the payments,
	// marks the swept contracts paid, and applies points. A dry run computes the
	// same plan but persists nothing, so it can be previewed first.
	if dryRun {
		// A dry run rolls its transaction back, so the frontier never advances —
		// looping would replan the same slice forever. Preview a single slice.
		plan, err := model.PlanPaymentsDryRunWithMaxDuration(ctx, maxDuration)
		if err != nil {
			fmt.Printf("payout plan err = %s\n", err)
			return
		}
		printPayoutPlan(plan, true)
		return
	}

	// When maxDuration is set, drain the backlog slice by slice — each slice is
	// its own committed plan — until the frontier reaches now, so one command
	// call catches up instead of needing one invocation per slice. maxDuration of
	// 0 (flag omitted) plans the whole backlog as a single plan.
	sliceCount := 0
	plans, err := model.PlanPaymentsWithMaxDurationLoop(ctx, maxDuration, func(p *model.PaymentPlan) {
		sliceCount += 1
		if maxDuration > 0 {
			fmt.Printf("\n===== slice %d =====\n", sliceCount)
		}
		printPayoutPlan(p, false)
	})
	if err != nil {
		fmt.Printf("payout plan err = %s\n", err)
		// fall through to summarize whatever slices did commit
	}
	if len(plans) > 1 {
		printPayoutLoopSummary(plans)
	}
}

func printPayoutPlan(plan *model.PaymentPlan, dryRun bool) {
	if dryRun {
		fmt.Println("DRY RUN - nothing was persisted. Preview of the payment plan:")
		fmt.Println("Payout Plan (preview): ", plan.PaymentPlanId)
	} else {
		fmt.Println("Payout Plan Created: ", plan.PaymentPlanId)
	}

	// largest payout first, so the plan reads consistently across runs
	payments := maps.Values(plan.NetworkPayments)
	slices.SortFunc(payments, func(a, b *model.AccountPayment) int {
		return cmp.Compare(b.Payout, a.Payout)
	})

	fmt.Printf("%-40s %-40s %16s\n", "Network ID", "Wallet ID", "Payout (USD)")
	fmt.Println(strings.Repeat("-", 98))

	total := model.NanoCents(0)
	missingWallet := 0
	for _, payment := range payments {
		walletStr := "(no payout wallet)"
		if payment.WalletId != nil {
			walletStr = payment.WalletId.String()
		} else {
			missingWallet += 1
		}
		fmt.Printf("%-40s %-40s %16.4f\n", payment.NetworkId, walletStr, model.NanoCentsToUsd(payment.Payout))
		total += payment.Payout
	}

	fmt.Println(strings.Repeat("-", 98))
	fmt.Printf("%d payment(s), %.4f USD total\n", len(payments), model.NanoCentsToUsd(total))
	if missingWallet > 0 {
		fmt.Printf("%d payment(s) have no payout wallet and will be held until a wallet is set\n", missingWallet)
	}
	if len(plan.WithheldNetworkIds) > 0 {
		fmt.Printf("%d network(s) withheld below the minimum payout threshold\n", len(plan.WithheldNetworkIds))
	}
	if s := plan.SubsidyPayment; s != nil {
		fmt.Printf(
			"subsidy: %.4f USD net payout over %s .. %s (%d active users)\n",
			model.NanoCentsToUsd(s.NetPayout),
			s.StartTime.Format(time.RFC3339),
			s.EndTime.Format(time.RFC3339),
			s.ActiveUserCount,
		)
	}
}

// printPayoutLoopSummary prints the grand total across all slices a bounded
// drain committed (PlanPaymentsWithMaxDurationLoop), after each slice's own
// plan has been printed.
func printPayoutLoopSummary(plans []*model.PaymentPlan) {
	totalPayments := 0
	total := model.NanoCents(0)
	for _, plan := range plans {
		for _, payment := range plan.NetworkPayments {
			totalPayments += 1
			total += payment.Payout
		}
	}
	fmt.Println()
	fmt.Println(strings.Repeat("=", 98))
	fmt.Printf(
		"drained %d slice(s): %d payment(s), %.4f USD total\n",
		len(plans), totalPayments, model.NanoCentsToUsd(total),
	)
}

func listPendingPayouts(opts docopt.Opts) {
	ctx := context.Background()

	planIdStr, _ := opts.String("--plan_id")

	var payouts []*model.AccountPayment

	if planIdStr != "" {
		planId, err := server.ParseId(planIdStr)
		if err != nil {
			panic(err)
		}
		payouts = model.GetPendingPaymentsInPlan(ctx, planId)
	} else {
		payouts = model.GetPendingPayments(ctx)
	}

	if len(payouts) == 0 {
		fmt.Println("No pending payouts")
		return
	}
	fmt.Printf("%-40s %-40s %-16s\n", "Payment ID", "Wallet ID", "Payout Amount")
	fmt.Println(strings.Repeat("-", 98))

	for _, payout := range payouts {
		payoutUsd := fmt.Sprintf("%.4f", model.NanoCentsToUsd(payout.Payout))
		fmt.Printf("%-40s %-40s %-16s\n", payout.PaymentId, payout.WalletId, payoutUsd)
	}

}

func payoutPending() {
	ctx := context.Background()
	clientSession := session.NewLocalClientSession(ctx, "0.0.0.0:0", nil)
	controller.SchedulePendingPayments(clientSession)
}

func payoutByPaymentId(opts docopt.Opts) {
	accountPaymentIdStr, err := opts.String("--account_payment_id")
	if err != nil {
		panic(err)
	}

	ctx := context.Background()

	accountPaymentId, err := server.ParseId(accountPaymentIdStr)
	if err != nil {
		panic(err)
	}

	accountPayment, err := model.GetPayment(ctx, accountPaymentId)
	if err != nil {
		panic(err)
	}

	if accountPayment.Completed {
		fmt.Println("Payment already completed at ", accountPayment.CompleteTime.String())
		return
	}

	if accountPayment.Canceled {
		fmt.Println("Payment canceled at ", accountPayment.CancelTime.String())
		return
	}

	clientSession := session.NewLocalClientSession(ctx, "0.0.0.0:0", nil)

	res, err := controller.AdvancePayment(&controller.AdvancePaymentArgs{
		PaymentId: accountPayment.PaymentId,
	}, clientSession)
	if err != nil {
		panic(err)
	}

	fmt.Printf("Payout to %s processing complete: \n", accountPaymentIdStr)
	fmt.Println("Complete Status: ", res.Complete)
}

func payoutPlanApplyBonus(opts docopt.Opts) {
	ctx := context.Background()

	planIdStr, err := opts.String("--plan_id")
	if err != nil {
		panic(err)
	}

	amountUsd, err := opts.Float64("--amount_usd")
	if err != nil {
		panic(err)
	}

	amountNanoCents := model.UsdToNanoCents(amountUsd)

	planId, err := server.ParseId(planIdStr)
	if err != nil {
		panic(err)
	}

	model.PayoutPlanApplyBonus(ctx, planId, amountNanoCents)
}

func adminWalletEstimateFee(opts docopt.Opts) {
	ctx := context.Background()

	amountUsd, err := opts.Float64("--amount_usd")
	if err != nil {
		panic(err)
	}

	blockchain, err := opts.String("--blockchain")
	if err != nil {
		panic(err)
	}

	destinationAddress, err := opts.String("--destination_address")
	if err != nil {
		panic(err)
	}

	client := controller.NewCircleClient()

	fees, err := client.EstimateTransferFee(ctx, amountUsd, destinationAddress, blockchain)
	if err != nil {
		panic(err)
	}

	fmt.Println("Medium BaseFee: ", fees.Medium.BaseFee)
	fmt.Println("Medium GasLimit: ", fees.Medium.GasLimit)
	fmt.Println("Medium GasPrice: ", fees.Medium.GasPrice)
	fmt.Println("Medium MaxFee: ", fees.Medium.MaxFee)
	fmt.Println("Medium PriorityFee: ", fees.Medium.PriorityFee)

	fee, err := controller.CalculateFee(*fees.Medium, blockchain)
	if err != nil {
		panic(err)
	}

	fmt.Printf("Total Fee: %f\n", *fee)

	usdFee, err := controller.ConvertFeeToUSDC(ctx, blockchain, *fee)
	if err != nil {
		panic(err)
	}

	fmt.Printf("Total Fee in USD: %f\n", usdFee)
}

func adminWalletTransfer(opts docopt.Opts) {
	ctx := context.Background()

	amountUsd, err := opts.Float64("--amount_usd")
	if err != nil {
		panic(err)
	}

	destinationAddress, err := opts.String("--destination_address")
	if err != nil {
		panic(err)
	}

	blockchain, err := opts.String("--blockchain")
	if err != nil {
		panic(err)
	}

	client := controller.NewCircleClient()

	// a one-off manual transfer gets a fresh idempotency key
	res, err := client.CreateTransferTransaction(ctx, server.NewId(), amountUsd, destinationAddress, blockchain)
	if err != nil {
		panic(err)
	}

	fmt.Println("Transaction Successful")
	fmt.Println("Transaction ID: ", res.Id)
	fmt.Println("Transaction State: ", res.State)
}

func closeExpiredContracts(opts docopt.Opts) {
	ctx := context.Background()

	var err error

	maxCount := 1000
	if _, ok := opts["--count"]; ok {
		maxCount, err = opts.Int("--count")
		if err != nil {
			panic(err)
		}
	}

	_, err = model.ForceCloseOpenContractIds(ctx, time.Now().Add(-1*time.Hour), maxCount, 30, 0, 0)
	if err != nil {
		panic(err)
	}
}

func closeContract(opts docopt.Opts) {

	ctx := context.Background()

	contractIdStr, err := opts.String("--contract_id")
	if err != nil {
		panic(err)
	}
	contractId, err := server.ParseId(contractIdStr)
	if err != nil {
		panic(err)
	}

	// this is either destinationId or sourceId from the transfer_contract table
	targetIdStr, err := opts.String("--target_id")
	if err != nil {
		panic(err)
	}
	targetId, err := server.ParseId(targetIdStr)
	if err != nil {
		panic(err)
	}

	usedTransferByteCount, err := opts.Int("--used_transfer_byte_count")
	if err != nil {
		panic(err)
	}

	err = model.CloseContract(
		ctx,
		contractId,
		targetId,
		int64(usedTransferByteCount),
		false,
	)
	if err != nil {
		panic(err)
	}

	fmt.Printf("Contract closed %s \n", contractIdStr)

}

// reconcileNetEscrow resets the redis net escrow counters to the postgres
// source of truth, clearing accumulated drift that causes spurious
// "Insufficient balance" errors, and prints the drift per network so the
// accumulated error can be tracked. With --network_id it targets a single
// network; without it, it sweeps every active balance. With --dry-run it only
// measures and prints the drift without writing anything.
//
// Drift is the signed difference (current counter minus the true reserved
// value). Positive drift is over-reservation: it starves the available balance
// and is what produces "Insufficient balance". Negative drift is
// under-reservation (over-available).
func reconcileNetEscrow(opts docopt.Opts) {
	ctx := context.Background()

	dryRun, _ := opts.Bool("--dry-run")
	apply := !dryRun

	verb := "reconciled"
	if dryRun {
		verb = "measured drift (dry run, not applied)"
	}

	if networkIdStr, _ := opts.String("--network_id"); networkIdStr != "" {
		networkId, err := server.ParseId(networkIdStr)
		if err != nil {
			panic(err)
		}
		drift, balanceCount := model.ReconcileNetEscrowForNetwork(ctx, networkId, apply)
		fmt.Printf("%s: network %s, %d active balance(s), drift %s\n", verb, networkId, balanceCount, signedByteCount(drift))
		return
	}

	driftByNetworkId, balanceCount := model.ReconcileNetEscrow(ctx, apply)

	// largest absolute drift first
	networkIds := maps.Keys(driftByNetworkId)
	absDrift := func(networkId server.Id) int64 {
		d := driftByNetworkId[networkId]
		if d < 0 {
			return -d
		}
		return d
	}
	slices.SortFunc(networkIds, func(a server.Id, b server.Id) int {
		if absDrift(a) == absDrift(b) {
			return a.Cmp(b)
		} else if absDrift(b) < absDrift(a) {
			return -1
		}
		return 1
	})

	overReserved := int64(0)
	underReserved := int64(0)
	for _, networkId := range networkIds {
		drift := driftByNetworkId[networkId]
		if 0 < drift {
			overReserved += drift
		} else {
			underReserved += -drift
		}
		fmt.Printf("  %s  %s\n", networkId, signedByteCount(drift))
	}
	fmt.Printf(
		"%s: %d active balance(s), %d network(s) drifted\n  over-reserved (starves balance): %s\n  under-reserved (over-available): %s\n",
		verb,
		balanceCount,
		len(driftByNetworkId),
		model.ByteCountHumanReadable(overReserved),
		model.ByteCountHumanReadable(underReserved),
	)
}

// signedByteCount formats a signed drift with an explicit + or - sign.
func signedByteCount(byteCount int64) string {
	if byteCount < 0 {
		return "-" + model.ByteCountHumanReadable(-byteCount)
	}
	return "+" + model.ByteCountHumanReadable(byteCount)
}

func populateMissingCircleAccountWallets() {

	ctx := context.Background()

	clientSession := session.NewLocalClientSession(ctx, "0.0.0.0:0", nil)

	controller.PopulateAccountWallets(&controller.PopulateAccountWalletsArgs{}, clientSession)
}

func taskLs(opts docopt.Opts) {

	ctx := context.Background()
	taskIds := task.ListPendingTasks(ctx)
	tasks := task.GetTasks(ctx, taskIds...)

	orderedTaskIds := maps.Keys(tasks)
	slices.SortFunc(orderedTaskIds, func(a server.Id, b server.Id) int {
		taskA := tasks[a]
		taskB := tasks[b]
		if taskA.RunAt == taskB.RunAt {
			return a.Cmp(b)
		} else if taskA.RunAt.Before(taskB.RunAt) {
			return -1
		} else {
			return 1
		}
	})

	fmt.Printf("%d pending tasks:\n", len(tasks))
	now := server.NowUtc()
	for _, taskId := range orderedTaskIds {
		task := tasks[taskId]
		remaining := (task.RunAt.Sub(now) / time.Second) * time.Second
		runAtStr := fmt.Sprintf("%s", remaining)
		if task.RescheduleError != "" {
			fmt.Printf("[%8s]  %s %s: rescheduled err = %s (%s)\n", runAtStr, taskId, task.FunctionName, task.RescheduleError, task.ArgsJson)
		} else {
			fmt.Printf("[%8s]  %s %s\n", runAtStr, taskId, task.FunctionName)
		}
	}

}

func taskRm(opts docopt.Opts) {
	ctx := context.Background()
	taskIdStr, _ := opts.String("<task_id>")
	taskId := server.RequireParseId(taskIdStr)

	task.RemovePendingTask(ctx, taskId)
}

func authLogin(opts docopt.Opts) {

	authCode, _ := opts.String("<auth_code>")

	ctx := context.Background()

	clientSession := session.NewLocalClientSession(ctx, "0.0.0.0:0", nil)

	authCodeLogin := &model.AuthCodeLoginArgs{
		AuthCode: authCode,
	}

	authCodeLoginResult, err := model.AuthCodeLogin(authCodeLogin, clientSession)
	if err != nil {
		panic(err)
	}

	fmt.Printf("RESULT: %+v\n", authCodeLoginResult)

}

func accountPointsPopulate(opts docopt.Opts) {
	ctx := context.Background()

	// Payment Plans since 5/1
	// [x] 0197760f-b8c5-b2f3-b932-47689f9bb6eb
	// [x] 019747af-e95b-73b8-61bd-217edd39ff34
	// [x] 019728d3-d5a2-25eb-40f4-e50b326637cf
	// [x] 0196f54a-2c73-3645-8b91-b25efe9c0792
	// [x] 0196d13d-a58c-dd9f-611b-ea4384015ddf
	// [x] 0196a80a-c8cf-74c7-1e20-d7c72958de80
	// [x] 01968d61-7a3c-b3a9-41c3-810bce4be30f
	//
	planIdStr, _ := opts.String("--plan_id")
	planId := server.RequireParseId(planIdStr)
	model.PopulatePlanAccountPoints(ctx, planId)
}

// remove this after we check next payout correctly populates tx hashes
func populateTxHashes() {
	ctx := context.Background()
	controller.PopulateTxHashes(ctx)
}

// func fixClientConnections(opts docopt.Opts) {
// 	ctx := context.Background()

// 	minTime := time.Now().Add(-30 * time.Second)

// 	controller.SetMissingConnectionLocations(ctx, minTime)
// }

/**
 * delete me after migration
 */
func migrateUserAuth(opts docopt.Opts) {
	ctx := context.Background()
	model.MigrateNetworkUserChildAuths(ctx)

	fmt.Println("User auth migration completed successfully.")
}

func reliabilitySetMultipliers(opts docopt.Opts) {
	ctx := context.Background()

	countryReliabilityMultipliers1 := model.GetAllClientLocationReliabilityMultipliers(ctx)

	model.UpdateClientLocationReliabilityMultipliersWithDefaults(ctx)

	countryReliabilityMultipliers2 := model.GetAllClientLocationReliabilityMultipliers(ctx)

	countryCodes := map[string]server.Id{}
	for _, m := range countryReliabilityMultipliers1 {
		countryCodes[m.CountryCode] = m.CountryLocationId
	}
	for _, m := range countryReliabilityMultipliers2 {
		countryCodes[m.CountryCode] = m.CountryLocationId
	}

	orderedCountryCodes := maps.Keys(countryCodes)
	slices.Sort(orderedCountryCodes)
	for _, countryCode := range orderedCountryCodes {
		countryLocationId := countryCodes[countryCode]
		m1 := countryReliabilityMultipliers1[countryLocationId]
		m2 := countryReliabilityMultipliers2[countryLocationId]
		if m1 == nil {
			fmt.Printf("%s unset -> %.1f\n", countryCode, m2.ReliabilityMultiplier)
		} else if m2 == nil {
			fmt.Printf("%s %.1f -> unset\n", countryCode, m1.ReliabilityMultiplier)
		} else {
			fmt.Printf("%s %.1f -> %.1f\n", countryCode, m1.ReliabilityMultiplier, m2.ReliabilityMultiplier)
		}
	}
}

func productUpdatesSync(opts docopt.Opts) {
	ctx := context.Background()

	controller.SyncInitialProductUpdates(ctx)
}

func searchQuery(opts docopt.Opts) {

	query, _ := opts.String("<query>")

	ctx := context.Background()

	startTime := time.Now()
	rs := model.SearchLocations(ctx, query, 2)
	endTime := time.Now()

	fmt.Printf("Search took %.2fms (%d)\n", float64(endTime.Sub(startTime)/time.Microsecond)/1000.0, len(rs))

	for i, r := range rs {
		fmt.Printf("[%d] %s\n", i, r.Value)
	}
}

func upgradePlan(opts docopt.Opts) {
	networkIdStr, _ := opts.String("--network_id")

	networkId, err := server.ParseId(networkIdStr)
	if err != nil {
		panic(err)
	}

	ctx := context.Background()

	startTime := server.NowUtc()

	subscriptionGracePeriod := 24 * time.Hour

	subscriptionYearDuration := 365 * 24 * time.Hour

	endTime := startTime.Add(subscriptionYearDuration + subscriptionGracePeriod)

	subscriptionRenewal := model.SubscriptionRenewal{
		NetworkId:          networkId,
		SubscriptionType:   model.SubscriptionTypeSupporter,
		StartTime:          startTime,
		EndTime:            endTime,
		NetRevenue:         0,
		SubscriptionMarket: model.SubscriptionMarketManual,
		// TransactionId:      paymentSearchResult.PaymentReference,
	}

	model.AddSubscriptionRenewal(ctx, &subscriptionRenewal)

	controller.AddRefreshTransferBalance(ctx, networkId)
}

func proxyParseId(opts docopt.Opts) {
	signedProxyId, _ := opts.String("<signed_proxy_id>")

	proxyId, err := model.ParseSignedProxyId(signedProxyId)

	if err != nil {
		panic(err)
	}

	fmt.Printf("%s\n", proxyId)
}

func proxyKeygen(opts docopt.Opts) {
	privateKey, publicKey, err := proxy.WgGenKeyPair()
	if err != nil {
		panic(err)
	}
	fmt.Printf("%s %s\n", privateKey, publicKey)
}

func proxyResetClientIpv4(opts docopt.Opts) {
	ctx := context.Background()
	model.ResetProxyClientIpv4(ctx)
}

func proxyInspect(opts docopt.Opts) {
	ctx := context.Background()

	proxyIdStr, _ := opts.String("<proxy_id>")
	proxyId := server.RequireParseId(proxyIdStr)

	proxyDeviceConfig := model.GetProxyDeviceConfig(ctx, proxyId)
	if proxyDeviceConfig == nil {
		fmt.Printf("Proxy does not exist.")
		return
	}

	fmt.Printf("%v\n", proxyDeviceConfig.InitialDeviceState.Location)
	fmt.Printf("%v\n", proxyDeviceConfig.InitialDeviceState.PerformanceProfile)
	fmt.Printf("%v\n", proxyDeviceConfig.InitialDeviceState.DnsResolverSettings)
}

func refreshTransferBalances(opts docopt.Opts) {
	ctx := context.Background()
	clientSession := session.NewLocalClientSession(ctx, "0.0.0.0:0", nil)

	controller.RefreshTransferBalances(
		&controller.RefreshTransferBalancesArgs{},
		clientSession,
	)
}

func modelMigrateProvideMode(opts docopt.Opts) {
	ctx := context.Background()
	model.MigrateProvideMode(ctx, 50000)
	fmt.Println("Provide mode migration completed successfully.")
}

func modelMigrateProxyDeviceConfig(opts docopt.Opts) {
	ctx := context.Background()
	model.MigrateProxyDeviceConfig(ctx, 50000)
	fmt.Println("Proxy device config migration completed successfully.")
}

// st — manual ops fallback for the subtensor epoch pipeline (sn/PLAN.md §6,
// D-3/D-11). These call the exact controller flows the st_work tasks run,
// so a manual action is recorded/idempotent the same way.

func stStatus(opts docopt.Opts) {
	ctx := context.Background()

	state, err := controller.StGetEpochState(ctx)
	if err != nil {
		panic(err)
	}
	closeBlock := state.EpochStartBlock + state.TEpochBlocks
	fmt.Printf("epoch (rolled):    %d\n", state.Epoch)
	fmt.Printf("epoch (pending):   %d\n", state.PendingEpoch)
	fmt.Printf("head block:        %d (%s)\n", state.HeadBlock, state.HeadBlockTime.Format(time.RFC3339))
	fmt.Printf("epoch start:       block %d\n", state.EpochStartBlock)
	fmt.Printf("intended close:    block %d\n", closeBlock)
	fmt.Printf("windows:           commit +%d, trails +%d, finalize +%d blocks\n",
		state.CommitWindowBlocks, state.TrailsWindowBlocks, state.FinalizeOffsetBlocks)

	// default detail epoch: the last closed epoch (the one in its
	// commit/finalize pipeline), else the current epoch
	epoch := state.Epoch
	if epochStr, err := opts.String("--epoch"); err == nil && epochStr != "" {
		parsed, err := strconv.ParseUint(epochStr, 10, 64)
		if err != nil {
			panic(fmt.Errorf("bad --epoch %q: %s", epochStr, err))
		}
		epoch = parsed
	} else if 0 < state.Epoch {
		epoch = state.Epoch - 1
	}

	pool, err := controller.StGetPoolState(ctx, epoch)
	if err != nil {
		panic(err)
	}
	fmt.Printf("epoch %d on chain:\n", epoch)
	if pool.CommittedRoot == ([32]byte{}) {
		fmt.Printf("  payout root:     (not committed)\n")
	} else {
		fmt.Printf("  payout root:     0x%x\n", pool.CommittedRoot)
	}
	fmt.Printf("  finalized:       %t\n", pool.Finalized)
	// v0.4 (D25): no on-chain DT ledger — deposits are summed from the mirrored
	// Deposited event log
	depositNoId, err := controller.StNoId()
	if err != nil {
		panic(err)
	}
	fmt.Printf("  deposits:        %s rao (Deposited events)\n", model.SumStDepositedRao(ctx, epoch, depositNoId))
	fmt.Printf("  pool total:      %s rao\n", pool.PoolTotalRao)
	fmt.Printf("  claimed:         %s rao\n", pool.ClaimedRao)

	if stEpoch := model.GetStEpoch(ctx, epoch); stEpoch != nil {
		fmt.Printf("epoch %d mirror:\n", epoch)
		fmt.Printf("  status:          %s\n", stEpoch.Status)
		fmt.Printf("  start block:     %d\n", stEpoch.StartBlock)
		fmt.Printf("  commit deadline: block %d\n", stEpoch.CommitDeadlineBlock)
		fmt.Printf("  finalize block:  %d\n", stEpoch.FinalizeBlock)
		leaves := model.GetStPayoutLeaves(ctx, epoch, func() uint64 {
			noId, err := controller.StNoId()
			if err != nil {
				panic(err)
			}
			return noId
		}())
		fmt.Printf("  payout leaves:   %d\n", len(leaves))
	} else {
		fmt.Printf("epoch %d mirror:   (no st_epoch row)\n", epoch)
	}

	publishes := model.GetStPublishes(ctx, epoch)
	fmt.Printf("epoch %d publishes: %d\n", epoch, len(publishes))
	for _, publish := range publishes {
		txHash := ""
		if publish.TxHash != nil {
			txHash = *publish.TxHash
		}
		errorMessage := ""
		if publish.Error != nil {
			errorMessage = *publish.Error
		}
		fmt.Printf("  %s %-12s %-9s %s %s\n",
			publish.CreateTime.Format(time.RFC3339), publish.Kind, publish.Status, txHash, errorMessage)
	}
}

func stDeposit(opts docopt.Opts) {
	ctx := context.Background()

	var overrideRao *big.Int
	if alphaRaoStr, err := opts.String("--alpha_rao"); err == nil && alphaRaoStr != "" {
		parsed, ok := new(big.Int).SetString(alphaRaoStr, 10)
		if !ok || parsed.Sign() <= 0 {
			panic(fmt.Errorf("bad --alpha_rao %q (expected a positive rao amount)", alphaRaoStr))
		}
		overrideRao = parsed
	}

	// deposit() credits the contract's current epoch
	state, err := controller.StGetEpochState(ctx)
	if err != nil {
		panic(err)
	}
	epoch := state.PendingEpoch

	outcome, err := controller.StDepositForEpoch(ctx, epoch, overrideRao)
	if err != nil {
		panic(err)
	}
	fmt.Printf("deposit epoch %d: %s\n", epoch, outcome)
}

func stCommit(opts docopt.Opts) {
	ctx := context.Background()

	epochStr, _ := opts.String("--epoch")
	epoch, err := strconv.ParseUint(epochStr, 10, 64)
	if err != nil {
		panic(fmt.Errorf("bad --epoch %q: %s", epochStr, err))
	}

	// recompute the leaves if the epoch was never closed by the pipeline
	noId, err := controller.StNoId()
	if err != nil {
		panic(err)
	}
	if leaves := model.GetStPayoutLeaves(ctx, epoch, noId); len(leaves) == 0 {
		fmt.Printf("no stored leaves for epoch %d; computing\n", epoch)
		root, leafCount, err := controller.StComputeEpochPayout(ctx, epoch)
		if err != nil {
			panic(err)
		}
		model.SetStEpochStatus(ctx, epoch, model.StEpochStatusClosed)
		fmt.Printf("computed %d leaves, root 0x%x\n", leafCount, root)
	}

	outcome, err := controller.StCommitEpochRoot(ctx, epoch)
	if err != nil {
		panic(err)
	}
	fmt.Printf("commit epoch %d: %s\n", epoch, outcome)
}

func stFinalize(opts docopt.Opts) {
	ctx := context.Background()

	epochStr, _ := opts.String("--epoch")
	epoch, err := strconv.ParseUint(epochStr, 10, 64)
	if err != nil {
		panic(fmt.Errorf("bad --epoch %q: %s", epochStr, err))
	}

	outcome, err := controller.StFinalizeEpochPoke(ctx, epoch)
	if err != nil {
		panic(err)
	}
	fmt.Printf("finalize epoch %d: %s\n", epoch, outcome)
}

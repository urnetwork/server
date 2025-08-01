package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"slices"
	"strings"
	"time"

	"golang.org/x/exp/maps"

	"github.com/docopt/docopt-go"

	"github.com/golang/glog"

	"github.com/urnetwork/server/v2025"
	"github.com/urnetwork/server/v2025/controller"
	"github.com/urnetwork/server/v2025/model"
	"github.com/urnetwork/server/v2025/search"
	"github.com/urnetwork/server/v2025/session"
	"github.com/urnetwork/server/v2025/task"
)

func main() {
	usage := `BringYour control.

Usage:
    bringyourctl db version
    bringyourctl db migrate
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
    bringyourctl network remove --network_id=<network_id>
    bringyourctl balance-code create
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
    bringyourctl payouts plan [--send]
    bringyourctl payouts populate-tx-hashes
    bringyourctl wallet estimate-fee --amount_usd=<amount_usd> --destination_address=<destination_address> --blockchain=<blockchain>
    bringyourctl wallet transfer --amount_usd=<amount_usd> --destination_address=<destination_address> --blockchain=<blockchain>
		bringyourctl wallets sync-circle
    bringyourctl contracts close-expired [-c <count>]
    bringyourctl contracts close --contract_id=<contract_id> --target_id=<target_id> --used_transfer_byte_count=<used_transfer_byte_count>
    bringyourctl task ls
    bringyourctl task rm <task_id>
    bringyourctl auth login <auth_code>
    bringyourctl account-points populate --plan_id=<plan_id>

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
    --secret=<secret>

    --contract_id=<contract_id> The contract to close
    --target_id=<target_id>     The contract_close.party (either "destination" or "source")
    --used_transfer_byte_count=<used_transfer_byte_count>   Bytes used
    --amount_usd=<amount_usd>   Amount in USD.
    --destination_address=<destination_address>  Destination address.
    --blockchain=<blockchain>  Blockchain.
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
	server.ApplyDbMigrations(context.Background())
}

func searchAdd(opts docopt.Opts) {
	realm, _ := opts.String("--realm")
	searchType, _ := opts.String("--type")

	searchService := search.NewSearch(
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

	searchService := search.NewSearch(
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

	networkId, err := server.ParseId(networkIdStr)
	if err != nil {
		panic(err)
	}
	model.RemoveNetwork(ctx, networkId)
}

func balanceCodeCreate(opts docopt.Opts) {
	ctx := context.Background()

	balanceCode, err := model.CreateBalanceCode(
		ctx,
		1024,
		0,
		server.NewId().String(),
		server.NewId().String(),
		"brien@brienyour.com",
	)
	if err != nil {
		panic(err)
	}
	fmt.Printf("%s\n", balanceCode.Secret)
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
	fmt.Printf("%s\n", result)
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

	if send {
		glog.Infof("[payouts]send\n")
		ctx := context.Background()
		clientSession := session.NewLocalClientSession(ctx, "0.0.0.0:0", nil)
		controller.SendPayments(clientSession)
	} else {
		plan, err := model.PlanPayments(context.Background())
		if err != nil {
			fmt.Printf("payout plan err = %s\n", err)
			return
		}
		fmt.Println("Payout Plan Created: ", plan.PaymentPlanId)
		fmt.Printf("%-40s %-16s\n", "Wallet ID", "Payout Amount")
		fmt.Println(strings.Repeat("-", 56))
		for _, payment := range plan.NetworkPayments {
			payoutUsd := fmt.Sprintf("%.4f\n", model.NanoCentsToUsd(payment.Payout))
			fmt.Printf("%-40s %-16s\n", payment.WalletId, payoutUsd)
		}
	}
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

	fees, err := client.EstimateTransferFee(amountUsd, destinationAddress, blockchain)
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

	usdFee, err := controller.ConvertFeeToUSDC(blockchain, *fee)
	if err != nil {
		panic(err)
	}

	fmt.Printf("Total Fee in USD: %f\n", *usdFee)
}

func adminWalletTransfer(opts docopt.Opts) {

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

	res, err := client.CreateTransferTransaction(amountUsd, destinationAddress, blockchain)
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

	_, err = model.ForceCloseOpenContractIds(ctx, time.Now().Add(-1*time.Hour), maxCount, 30)
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
		return a.Cmp(b)
	})

	fmt.Printf("%d pending tasks:\n", len(tasks))
	for _, taskId := range orderedTaskIds {
		task := tasks[taskId]
		if task.RescheduleError != "" {
			fmt.Printf("  %s %s: rescheduled err = %s\n", taskId, task.FunctionName, task.RescheduleError)
		} else {
			fmt.Printf("  %s %s\n", taskId, task.FunctionName)
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

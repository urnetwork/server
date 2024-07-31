package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"strings"
	"time"

	"github.com/docopt/docopt-go"

	"bringyour.com/bringyour"
	"bringyour.com/bringyour/controller"
	"bringyour.com/bringyour/model"
	"bringyour.com/bringyour/search"
	"bringyour.com/bringyour/session"
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
    bringyourctl payouts plan
    bringyourctl wallet estimate-fee --amount_usd=<amount_usd> --destination_address=<destination_address> --blockchain=<blockchain>
    bringyourctl wallet transfer --amount_usd=<amount_usd> --destination_address=<destination_address> --blockchain=<blockchain>
    bringyourctl contracts close-expired

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
    --amount_usd=<amount_usd>   Amount in USD.
    --destination_address=<destination_address>  Destination address.
    --blockchain=<blockchain>  Blockchain.`

    opts, err := docopt.ParseArgs(usage, os.Args[1:], bringyour.RequireVersion())
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
        }
        if plan, _ := opts.Bool("plan"); plan {
            planPayouts()
        }
        if applyBonus, _ := opts.Bool("apply-bonus"); applyBonus {
            payoutPlanAppyBonus(opts)
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
            closeExpiredContracts()
        }
    }
}

func dbVersion(opts docopt.Opts) {
    version := bringyour.DbVersion(context.Background())
    bringyour.Logger().Printf("Current DB version: %d\n", version)
}


func dbMigrate(opts docopt.Opts) {
    bringyour.Logger().Printf("Applying DB migrations ...\n")
    bringyour.ApplyDbMigrations(context.Background())
}


func searchAdd(opts docopt.Opts) {
    realm, _ := opts.String("--realm")
    searchType, _ := opts.String("--type")

    searchService := search.NewSearch(
        realm,
        search.SearchType(searchType),
    )

    value, _ := opts.String("<value>")
    valueId := bringyour.NewId()
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
    bringyour.Raise(err)
    bringyour.Logger().Printf("%s\n", statsJson)
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
        bringyour.Raise(err)
        bringyour.Logger().Printf("%s\n", statsJson)
    }
}

func statsAdd(opts docopt.Opts) {
    controller.AddSampleEvents(context.Background(), 4 * 60)
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

    networkId, err := bringyour.ParseId(networkIdStr)
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
        bringyour.NewId().String(),
        bringyour.NewId().String(),
        "brien@bringyour.com",
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

    err := awsMessageSender.SendAccountMessageTemplate(
        userAuth,
        &controller.AuthVerifyTemplate{
            VerifyCode: "abcdefghij",
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
        bringyour.NewId().String(),
        bringyour.NewId().String(),
        userAuth,
    )
    if err != nil {
        panic(err)
    }

    err = awsMessageSender.SendAccountMessageTemplate(
        userAuth,
        &controller.SubscriptionTransferBalanceCodeTemplate{
            Secret: balanceCode.Secret,
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
            PaymentId: bringyour.NewId(),
            TxHash: "0x1234567890",
            ExplorerBasePath: "https://explorer.solana.com/tx",
            ReferralCode: bringyour.NewId().String(),
            Blockchain: "Solana",
            DestinationAddress: "0x1234567890",
            AmountUsd: "5.00",
            PaymentCreatedAt: time.Now().UTC(),
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
        controller.SenderEmail("brien@bringyour.com"),
    )
    if err != nil {
        panic(err)
    }
    fmt.Printf("Sent\n")
}

func planPayouts() {
    plan := model.PlanPayments(context.Background())
    fmt.Println("Payout Plan Created: ", plan.PaymentPlanId)
    fmt.Printf("%-40s %-16s\n", "Wallet ID", "Payout Amount")
    fmt.Println(strings.Repeat("-", 56))
    for _, payment := range plan.WalletPayments {
        payoutUsd := fmt.Sprintf("%.4f\n", model.NanoCentsToUsd(payment.Payout))
        fmt.Printf("%-40s %-16s\n", payment.WalletId, payoutUsd)
    }
}

func listPendingPayouts(opts docopt.Opts) {
    ctx := context.Background()

    planIdStr, _ := opts.String("--plan_id")

    var payouts []*model.AccountPayment

    if planIdStr != "" {
        planId, err := bringyour.ParseId(planIdStr)
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
    fmt.Printf("%-40s %-16s\n", "Wallet ID", "Payout Amount")
    fmt.Println(strings.Repeat("-", 56))

    for _, payout := range payouts {
        payoutUsd := fmt.Sprintf("%.4f\n", model.NanoCentsToUsd(payout.Payout))
        fmt.Printf("%-40s %-16s\n", payout.WalletId, payoutUsd)
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

    accountPaymentId, err := bringyour.ParseId(accountPaymentIdStr)
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

    res, err := controller.ProviderPayout(accountPayment, clientSession)
    if err != nil {
        panic(err)
    }

    fmt.Printf("Payout to %s processing complete: \n", accountPaymentIdStr)
    fmt.Println("Complete Status: ", res.Complete)
}

func payoutPlanAppyBonus(opts docopt.Opts) {
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

    planId, err := bringyour.ParseId(planIdStr)
    if err != nil {
        panic(err)
    }

    model.PayoutPlanAppyBonus(ctx, planId, amountNanoCents)
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

func closeExpiredContracts() {
    ctx := context.Background()
    expiredContracts := model.GetExpiredTransferContracts(ctx)

    fmt.Println("# of contracts to close: ", len(expiredContracts))

    for _, expiredContract := range expiredContracts {
        fmt.Println("Closing contract: ", expiredContract.ContractId.String())

        var targetId *bringyour.Id
        if expiredContract.Party == model.ContractPartySource {
            targetId = &expiredContract.DestinationId
        } else if expiredContract.Party == model.ContractPartyDestination {
            targetId = &expiredContract.SourceId
        }

        if targetId == nil {
            continue
        }

        // forcing the contract to be closed 
        // due to lack of response from the counterparty
        err := model.CloseContract(
            ctx, 
            expiredContract.ContractId, 
            *targetId,
            expiredContract.UsedTransferByteCount,
            false,
        )
        if err != nil {
            fmt.Println("Error closing contract: ", err)
        }
    }

    fmt.Println("Contracts closed")
}
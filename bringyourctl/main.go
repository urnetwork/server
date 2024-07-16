package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

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
    bringyourctl payout check
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
    --secret=<secret>`

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
        }
    } else if payout, _ := opts.Bool("payout"); payout {
        livePayoutTest()
    }
}

func livePayoutTest() {

    // fmt.Println("Payout test")

    // controller.ListAccounts()
    // controller.GetProfiles()
    // controller.ListTransactions()

    // controller.GenerateEntitySecretCipher()
    // controller.CreateDeveloperWalletSet("URNetwork Payout Wallet")
    // controller.CreateDeveloperWallet("0190aa1d-07b3-7765-a172-aff1c20ea982")
    // feeEstimate, _ := controller.EstimateTransferFee(
    //     controller.EstimateTransferFeeArgs{
    //         Amount: 1,
    //         // DestinationAddress: "4Hvvk6Ax7JXKXUCf8HCosJKMqYyoyNMPMPu4D9x3vTjj",
    //         DestinationAddress: "0x6BC3631A507BD9f664998F4E7B039353Ce415756",
    //         // WalletId: "3eb00864-2064-5daa-95fb-7a75539832b9",
    //         Network: "MATIC",
    //     },
    // )


    // fmt.Println("Fee Estimate High Gas Limit: ", feeEstimate.High.GasLimit)
    // fmt.Println("Fee Estimate High Priority Fee: ", feeEstimate.High.PriorityFee)
    // fmt.Println("Fee Estimate High Base Fee: ", feeEstimate.High.BaseFee)
    // fmt.Println("====================================")
    // fmt.Println("Fee Estimate Medium Gas Limit: ", feeEstimate.Medium.GasLimit)
    // fmt.Println("Fee Estimate Medium Priority Fee: ", feeEstimate.Medium.PriorityFee)
    // fmt.Println("Fee Estimate Medium Base Fee: ", feeEstimate.Medium.BaseFee)
    // fmt.Println("====================================")
    // fmt.Println("Fee Estimate Low Gas Limit: ", feeEstimate.Low.GasLimit)
    // fmt.Println("Fee Estimate Low Priority Fee: ", feeEstimate.Low.PriorityFee)
    // fmt.Println("Fee Estimate Low Base Fee: ", feeEstimate.Low.BaseFee)




    // controller.SendPayment(
    //     controller.SendPaymentArgs{
    //         Amount: .10,
    //         // DestinationAddress: "4Hvvk6Ax7JXKXUCf8HCosJKMqYyoyNMPMPu4D9x3vTjj",
    //         // Network: "SOL",
    //         DestinationAddress: "0x6BC3631A507BD9f664998F4E7B039353Ce415756",
    //         Network: "MATIC",
    //     },
    // )

    // _, err := controller.CalcuateFeePolygon(
    //     controller.FeeEstimate{
    //         GasLimit: "58858",
    //         PriorityFee: "36.499999998",
    //         BaseFee: "0.000000035",
    //     },
    // )
    // if err != nil {
    //     panic(err)
    // }

    // solana
    // ====================================
    // Fee Estimate Medium Gas Limit:  18281
    // Fee Estimate Medium Priority Fee:  2023952
    // Fee Estimate Medium Base Fee:  0.00204928
    // unsure how to estimate fee
    // actual fee was 0.000046574

    // polygon
    // ====================================
    // Fee Estimate Medium Gas Limit:  81441
    // Fee Estimate Medium Priority Fee:  37.518543992
    // Fee Estimate Medium Base Fee:  0.000000031
    // baseFee * gasLimit = estimated fee
    // 0.000000031 * 81441 = 0.002525671
    // actual fee was 0.002307251577677021
    // pretty accurate

    // controller.FetchConvertionRate(
    //     1, "USD", "SOL",
    // )

    // fee, err := controller.ConvertFeeToUSDC("MATIC", 0.002060)
    // if err != nil {
    //     panic(err)
    // }

    // fmt.Printf(fmt.Sprintf("%f\n", *fee))

    client := controller.NewCircleClient()
    txResult, err := client.GetTransaction("2181114a-22a7-527e-b16a-89c798be0799")
    if err != nil {
        panic(err)
    }

    tx := txResult.Transaction

    fmt.Println("tx id: ", tx.Id)
    fmt.Println("tx state: ", tx.State)
    fmt.Println("tx amount in USD: ", tx.AmountInUSD)
    fmt.Println("tx destination address: ", tx.DestinationAddress)
    fmt.Println("tx source address: ", tx.SourceAddress)

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


func sendSubscriptionTransferBalanceCode(opts docopt.Opts) {
    userAuth, _ := opts.String("--user_auth")

    awsMessageSender := controller.GetAWSMessageSender()

    err := awsMessageSender.SendAccountMessageTemplate(
        userAuth,
        &controller.SubscriptionTransferBalanceCodeTemplate{
            Secret: "hi there bar now",
        },
    )
    if err != nil {
        panic(err)
    }
    fmt.Printf("Sent\n")
}


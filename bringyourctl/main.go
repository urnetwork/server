package main

import (
    "context"
    "fmt"
    "os"
    "encoding/json"

    "github.com/docopt/docopt-go"

    "bringyour.com/bringyour"
    "bringyour.com/bringyour/session"
    "bringyour.com/bringyour/model"
    "bringyour.com/bringyour/controller"
    "bringyour.com/bringyour/search"
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
    bringyourctl send network-user-interview-request-1 --user_auth=<user_auth>

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
        } else if networkUserInterviewRequest1, _ := opts.Bool("network-user-interview-request-1"); networkUserInterviewRequest1 {
            sendNetworkUserInterviewRequest1(opts)
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

    err := controller.SendAccountMessageTemplate(
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

    err := controller.SendAccountMessageTemplate(
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

    err := controller.SendAccountMessageTemplate(
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

    err := controller.SendAccountMessageTemplate(
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

    ctx := context.Background()

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

    err = controller.SendAccountMessageTemplate(
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


func sendNetworkUserInterviewRequest1(opts docopt.Opts) {
    userAuth, _ := opts.String("--user_auth")

    err := controller.SendAccountMessageTemplate(
        userAuth,
        &controller.NetworkUserInterviewRequest1Template{},
        controller.SenderEmail("brien@bringyour.com"),
    )
    if err != nil {
        panic(err)
    }
    fmt.Printf("Sent\n")
}

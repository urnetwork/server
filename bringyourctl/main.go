package main

import (
    "context"
    "fmt"
    "os"
    "encoding/json"

    "github.com/docopt/docopt-go"

    "bringyour.com/bringyour"
    "bringyour.com/bringyour/model"
    "bringyour.com/bringyour/controller"
    "bringyour.com/bringyour/search"
)


type CtlArgs struct {
    SearchRealm string `docopt:"--realm"`
    SearchType string `docopt:"--type"`
    SearchDistance int `docopt:"--distance"`
}


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
    bringyourctl locations add-default

Options:
    -h --help     Show this screen.
    --version     Show version.
    -r --realm=<realm>  Search realm.
    -t --type=<type>    Search type.
    -d, --distance=<distance>  Search distance.`

    opts, err := docopt.ParseArgs(usage, os.Args[1:], bringyour.RequireVersion())
    if err != nil {
        panic(err)
    }

    args := CtlArgs{}
    opts.Bind(&args)

    if db, _ := opts.Bool("db"); db {
        if version, _ := opts.Bool("version"); version {
            dbVersion(opts, args)
        } else if migrate, _ := opts.Bool("migrate"); migrate {
            dbMigrate(opts, args)
        }
    } else if search, _ := opts.Bool("search"); search {
        if add, _ := opts.Bool("add"); add {
            searchAdd(opts, args)
        } else if around, _ := opts.Bool("around"); around {
            searchAround(opts, args)
        } else if remove, _ := opts.Bool("remove"); remove {
            searchRemove(opts, args)
        } else if clear, _ := opts.Bool("clear"); clear {
            searchClear(opts, args)
        }
    } else if stats, _ := opts.Bool("stats"); stats {
        if compute, _ := opts.Bool("compute"); compute {
            statsCompute(opts, args)
        } else if export, _ := opts.Bool("export"); export {
            statsExport(opts, args)
        } else if import_, _ := opts.Bool("import"); import_ {
            statsImport(opts, args)
        } else if add, _ := opts.Bool("add"); add {
            statsAdd(opts, args)
        }
    } else if locations, _ := opts.Bool("locations"); locations {
        if addDefault, _ := opts.Bool("add-default"); addDefault {
            locationsAddDefault(opts, args)
        }
    }
}


func dbVersion(opts docopt.Opts, args CtlArgs) {
    version := bringyour.DbVersion(context.Background())
    bringyour.Logger().Printf("Current DB version: %d\n", version)
}


func dbMigrate(opts docopt.Opts, args CtlArgs) {
    bringyour.Logger().Printf("Applying DB migrations ...\n")
    bringyour.ApplyDbMigrations(context.Background())
}


func searchAdd(opts docopt.Opts, args CtlArgs) {
    searchService := search.NewSearch(
        args.SearchRealm,
        search.SearchType(args.SearchType),
    )

    value, _ := opts.String("<value>")
    valueId := bringyour.NewId()
    searchService.Add(context.Background(), value, valueId, 0)
}

func searchAround(opts docopt.Opts, args CtlArgs) {
    searchService := search.NewSearch(
        args.SearchRealm,
        search.SearchType(args.SearchType),
    )

    value, _ := opts.String("<value>")
    searchResults := searchService.Around(context.Background(), value, args.SearchDistance)
    for _, searchResult := range searchResults {
        fmt.Printf("%d %s %s\n", searchResult.ValueDistance, searchResult.Value, searchResult.ValueId)
    }
}

func searchRemove(opts docopt.Opts, args CtlArgs) {
    // fixme
}

func searchClear(opts docopt.Opts, args CtlArgs) {
    // fixme
}


func statsCompute(opts docopt.Opts, args CtlArgs) {
    stats := model.ComputeStats90(context.Background())
    statsJson, err := json.MarshalIndent(stats, "", "  ")
    bringyour.Raise(err)
    bringyour.Logger().Printf("%s\n", statsJson)
}

func statsExport(opts docopt.Opts, args CtlArgs) {
    ctx := context.Background()
    stats := model.ComputeStats(ctx, 90)
    model.ExportStats(ctx, stats)
}

func statsImport(opts docopt.Opts, args CtlArgs) {
    stats := model.GetExportedStats(context.Background(), 90)
    if stats != nil {
        statsJson, err := json.MarshalIndent(stats, "", "  ")
        bringyour.Raise(err)
        bringyour.Logger().Printf("%s\n", statsJson)
    }
}

func statsAdd(opts docopt.Opts, args CtlArgs) {
    controller.AddSampleEvents(context.Background(), 4 * 60)
}


func locationsAddDefault(opts docopt.Opts, args CtlArgs) {
    ctx := context.Background()
    model.AddDefaultLocations(ctx, 0)
}


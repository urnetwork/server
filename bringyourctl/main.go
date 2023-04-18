package main

import (
	// "flag"
    "fmt"
    "os"
    "strconv"

	"bringyour.com/bringyour"
	"bringyour.com/bringyour/search"
	"bringyour.com/bringyour/ulid"
)

// todo db migrate
// todo search <realm> <type> add <value>
// todo search <realm> <type> around <distance> <value>
// todo search <realm> <type> remove <value>
// todo search <realm> <type> clear


func main() {
	if len(os.Args) < 2 {
		usage()
		return
	}

	switch os.Args[1] {
	case "db":
		commandDb()
	case "search":
		commandSearch()
	}
}

func usage() {
	fmt.Printf("Invalid command\n")
}


func commandDb() {
	if len(os.Args) < 3 {
		usage()
		return
	}

	switch os.Args[2] {
	case "migrate":
		bringyour.ApplyDbMigrations()
	default:
		usage()
	}
}

func commandSearch() {

	// remove the router prefix
	args := os.Args[2:]
	fmt.Printf("ARGS %s\n", args)

	if len(args) < 3 {
		usage()
		return
	}

	realm := args[0]
	searchType := args[1]
	searchService := search.NewSearch(
		realm,
		search.SearchType(searchType),
	)

	switch args[2] {
	case "add":
		value := args[3]
		valueId := ulid.Make()
		searchService.Add(value, valueId)
	case "around":
		distance, _ := strconv.Atoi(args[3])
		value := args[4]
		searchResults := searchService.Around(value, distance)
		for _, searchResult := range searchResults {
			fmt.Printf("%d %s %s\n", searchResult.ValueDistance, searchResult.Value, searchResult.ValueId)
		}
	case "remove":
		// value := args[2]
	case "clear":
	default:
		usage()
	}

}


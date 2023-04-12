package main

import (
	"bringyour.com/bringyour"
)

// todo run migrations

func main() {
	bringyour.ApplyDbMigrations()
}

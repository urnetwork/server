package main

import (
	"context"
	"fmt"

	"github.com/docopt/docopt-go"

	"github.com/urnetwork/server/model"
)

// one-shot cleanup for the stream keys written with an effectively-infinite
// ttl by the pre-2026-07-20 duration-as-nanoseconds eval arg (see
// model.ExpireLeakedStreamKeys)
func streamsExpireLeakedTtls(opts docopt.Opts) {
	ctx := context.Background()

	scannedCount, fixedCount, err := model.ExpireLeakedStreamKeys(ctx)
	if err != nil {
		panic(err)
	}
	fmt.Printf("scanned %d stream keys, clamped %d leaked ttls to 8h\n", scannedCount, fixedCount)
}

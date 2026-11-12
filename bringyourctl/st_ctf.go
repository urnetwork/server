package main

import (
	"context"
	"fmt"
	"time"

	"github.com/docopt/docopt-go"

	"github.com/urnetwork/server/controller"
)

// stCtfOutage is the operator's explicit command declaring a platform-wide
// outage for capture the flag's presence (sn CAPTURETHEFLAG.md §4.2): the
// samples whose quarter hours overlap it are excused in every census built
// after it, and listed with the census. Times are RFC 3339. It records who
// declared it and why, and names any epoch whose census was already built and
// so keeps the samples it had.
func stCtfOutage(opts docopt.Opts) {
	parse := func(option string) time.Time {
		text, _ := opts.String(option)
		parsed, err := time.Parse(time.RFC3339, text)
		if err != nil {
			panic(fmt.Errorf("bad %s %q: %s", option, text, err))
		}
		return parsed
	}
	startTime, endTime := parse("--start"), parse("--end")
	reason, _ := opts.String("--reason")
	by, _ := opts.String("--by")
	outageId, late, err := controller.StCtfDeclareOutage(context.Background(), startTime, endTime, reason, by)
	if err != nil {
		panic(err)
	}
	fmt.Printf("outage %s from %s to %s declared by %s\n", outageId, startTime.UTC().Format(time.RFC3339), endTime.UTC().Format(time.RFC3339), by)
	for _, epoch := range late {
		fmt.Printf("epoch %d's census is already built and keeps its samples\n", epoch)
	}
}

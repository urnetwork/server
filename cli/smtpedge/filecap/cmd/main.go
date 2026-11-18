// filecap: the static helper smtpedge's image build runs from a bind mount and never ships, and the release check
// build/all/run.sh runs on the published image (QD-1066, owner decision 597; DESIGN §19.4's smtpedge capability).
//
//	filecap set <path>            write cap_net_admin=ep on path as revision 2, the bytes setcap cap_net_admin+ep
//	                              writes outside a user namespace, and read it back
//	filecap check <path>          refuse unless path holds exactly that attribute: the build's step after the stage
//	                              copy, which must keep it
//	filecap check-image <image>   refuse unless each published platform's /usr/local/sbin/bringyour-smtpedge holds
//	                              it, read as the host's docker unpacks the image (filecap.CheckImage)
//	filecap so-mark               succeed only when this process may set SO_MARK, the call smtpedge's dialer makes;
//	                              run as a binary that carries the attribute, it shows what the attribute grants
//
// The Makefile builds it with CGO_ENABLED=0, so it runs in a build stage with nothing installed, in the image's own
// platform, under emulation for the other one, as the stage's dpkg does. Exit 0 is success, 1 a refusal or a
// failure, 2 a usage error.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/urnetwork/server/cli/smtpedge/filecap"
)

// Runs the one mode its arguments name and exits with its answer.
func main() {
	usage := func() {
		fmt.Fprintln(os.Stderr, "usage: filecap set <path> | check <path> | check-image <image> | so-mark")
		os.Exit(2)
	}
	if len(os.Args) < 2 {
		usage()
	}
	fail := func(err error) {
		fmt.Fprintf(os.Stderr, "filecap %s: %v\n", os.Args[1], err)
		os.Exit(1)
	}
	// the attribute path holds, as the kernel answers it, against the one smtpedge's binary may carry
	check := func(path string) {
		value, err := filecap.ReadFileCapability(path)
		if err == nil {
			err = filecap.CheckNetAdmin(value)
		}
		if err != nil {
			fail(fmt.Errorf("%s: %w", path, err))
		}
		fmt.Printf("%s %s\n", path, filecap.NetAdminFileCapability())
	}

	command, args := os.Args[1], os.Args[2:]
	switch {
	case command == "set" && len(args) == 1:
		if err := filecap.SetFileCapability(args[0], filecap.NetAdminFileCapability()); err != nil {
			fail(err)
		}
		check(args[0])
	case command == "check" && len(args) == 1:
		check(args[0])
	case command == "check-image" && len(args) == 1:
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		lines, err := filecap.CheckImage(ctx, args[0], filecap.DefaultImageCheckSettings())
		stop()
		for _, line := range lines {
			fmt.Println(line)
		}
		if err != nil {
			fail(fmt.Errorf("%s: %w", args[0], err))
		}
	case command == "so-mark" && len(args) == 0:
		if err := filecap.ProbeSoMark(); err != nil {
			fail(err)
		}
		fmt.Println("SO_MARK set")
	default:
		usage()
	}
}

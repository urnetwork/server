// server/cli/mxedge (MAIL-M5-04; §15.4, §15.15, §19.4): the mxedge engine's one binary in two roles on TCP
// 25, each its own warp service on its own lb interfaces (W4, plan-OPS-small-readings #6): --role platform for
// mxedge-platform, rpt.bringyour.com's listener, whose image server/cli/mxedge-platform builds from this main,
// and --role router for mxedge, which MAIL-M7-19 builds and which refuses until then. Each is a block run by
// mxedgehost.RunHost through sidecarhost.Run, with GET /status on port 80, TCP 25 behind the lb's PROXY header,
// {vault: no, config: yes, site: no} and user 65532. It reads config/<env>/mxedge.yml at start, the platform
// role's report intake reads smtpedge.yml beside it (report_intake.go, SMTP-M5-06), and the host rereads
// sidecar.yml's mxedge gate every 60 s.
package main

import (
	"context"
	"errors"
	"flag"
	"os"
	"os/signal"
	"syscall"

	"github.com/urnetwork/glog"
	"github.com/urnetwork/sidecar/mxedgeservice"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/mxedgehost"
	"github.com/urnetwork/server/sidecarhost"
)

var (
	role      = flag.String("role", "", "platform; router lands with MAIL-M7-19")
	port      = flag.Int("port", 80, "the block's status port")
	smtpPort  = flag.Int("smtp_port", 25, "the block's TCP 25 port, behind the lb's PROXY header")
	secretDir = flag.String("secret_dir", sidecarhost.ScopedVaultHome, "where the block's secret_files mount")
)

func main() {
	// before anything can log (server.ScrubCommandLogs)
	logs := server.ScrubCommandLogs()
	defer logs.Flush()

	flag.CommandLine.Init(os.Args[0], flag.ContinueOnError)
	if err := flag.CommandLine.Parse(os.Args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			logs.Exit(0)
		}
		logs.Exit(2)
	}
	if *role != mxedgehost.HostRolePlatform {
		glog.Errorf("[mxedge]--role must be %s; %s lands with MAIL-M7-19\n", mxedgehost.HostRolePlatform, mxedgehost.HostRoleRouter)
		logs.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGQUIT, syscall.SIGTERM)
	defer stop()

	mxEdgeSettings, err := loadMxEdgeSettings()
	if err != nil {
		// an mxedge.yml that cannot be served from never serves (mxedgeservice.LoadMxEdgeSettings)
		glog.Errorf("[mxedge]mxedge.yml: %v\n", err)
		logs.Exit(1)
	}
	settings := mxedgehost.DefaultHostSettings()
	settings.MxEdge = mxEdgeSettings
	settings.SecretDirectory = *secretDir
	// the block's own certificate and stores, and the report intake on smtpedge.yml (report_intake.go), ready
	// only in a binary that links the groups it reads; without one every recipient gets 451
	deps := &mxedgehost.HostDeps{}
	intake, err := newConfiguredReportIntake(mxEdgeSettings)
	if err != nil {
		glog.Errorf("[mxedge]no report intake, so every recipient the listener answers for gets 451 4.3.0: %v\n", err)
	} else {
		deps.Intake = intake
		deps.IntakeGroups = reportIntakeGroups()
	}
	if err := mxedgehost.RunHost(ctx, *role, settings, deps, &mxedgehost.HostPorts{Status: *port, Smtp: *smtpPort}); err != nil {
		panic(err)
	}
}

// mxedge.yml from config.
func loadMxEdgeSettings() (*mxedgeservice.MxEdgeSettings, error) {
	resource, err := server.Config.SimpleResource("mxedge.yml")
	if err != nil {
		return nil, err
	}
	data, err := resource.BytesE()
	if err != nil {
		return nil, err
	}
	return mxedgeservice.LoadMxEdgeSettings(data)
}

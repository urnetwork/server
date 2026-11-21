package main

import (
	"fmt"
	"io"

	"github.com/urnetwork/server"
)

// Read all five cells' prerequisites from one snapshot with the service reader.
// Columns are gate, on/off/error, and an optional matrix detail.
func readT6Gates(args []string, out, errOut io.Writer) int {
	if len(args) != 2 {
		fmt.Fprintln(errOut, "usage: read-t6-gates FILE NETWORK")
		return 2
	}
	cfg, err := server.ReadSidecarConfigFile(args[0])
	if err != nil {
		fmt.Fprintf(errOut, "cannot read sidecar gate file: %v\n", err)
		return 1
	}
	network, networkErr := server.ParseId(args[1])
	for _, name := range []string{server.SidecarGateEvs, server.SidecarGateGkm, server.SidecarGateSockets, server.SidecarGateTlsedge, server.SidecarGateMxedge, server.SidecarGateSmtpedge, server.SidecarGateDns, server.SidecarSwitchBridge, server.SidecarSwitchCerts} {
		if name == server.SidecarSwitchBridge || name == server.SidecarSwitchCerts {
			if cfg.Switch(name) {
				fmt.Fprintf(out, "%s\ton\n", name)
			} else {
				fmt.Fprintf(out, "%s\toff\tthe %s gate is disabled\n", name, name)
			}
			continue
		}
		gate := cfg.Service(name)
		switch {
		case !gate.Enabled():
			fmt.Fprintf(out, "%s\toff\tthe %s gate is disabled\n", name, name)
		case (name == server.SidecarGateEvs || name == server.SidecarGateDns) && !gate.Writes():
			fmt.Fprintf(out, "%s\toff\tthe %s gate has writes: false, the incident switch\n", name, name)
		case name == server.SidecarGateDns:
			fmt.Fprintf(out, "%s\ton\n", name)
		case args[1] == "":
			fmt.Fprintf(out, "%s\terror\tthe %s gate is enabled and no acceptance network is named (UR_ACCEPT_SIDECAR_NETWORK)\n", name, name)
		case networkErr != nil:
			fmt.Fprintf(out, "%s\terror\tthe acceptance network id is invalid\n", name)
		case gate.Admits(network):
			fmt.Fprintf(out, "%s\ton\n", name)
		default:
			fmt.Fprintf(out, "%s\toff\tthe %s gate does not admit the acceptance network: not listed, and its bucket %d is not under percent %d\n", name, name, server.SidecarGateBucket(network), gate.Percent())
		}
	}
	return 0
}

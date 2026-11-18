//go:build tools

// Holds smtpproof in the graph of every module that builds this package (§4.7). go mod tidy reads
// every build tag, so this import keeps its require here and in sn, which builds this package, and no
// build sets the tag, so this package links no smtpproof. cli/smtpedge imports it for real, for the
// edge's verifier (OPS-M9-02), which keeps this module's require by itself; this file stays for sn's,
// until sn's go.mod is tidied without it. The root needs no line here since controller's sidecar
// identity routes import it.
package server

import (
	_ "github.com/urnetwork/sidecar/smtpproof"
)

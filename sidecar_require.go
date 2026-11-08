//go:build tools

// Holds smtpproof in this module's graph until server's host of the smtp edge imports it for real
// (§4.7). go mod tidy reads every build tag, so this import keeps its require here and in sn, which
// builds this package, and no build sets the tag, so nothing links smtpproof yet. The root needs no
// line here since controller's sidecar identity routes import it. Delete this file in the change
// that adds smtpproof's first real import.
package server

import (
	_ "github.com/urnetwork/sidecar/smtpproof"
)

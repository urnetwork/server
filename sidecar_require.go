//go:build tools

// Holds sidecar and smtpproof in this module's graph until a server/cli/<svc> imports them for real
// (§4.7). go mod tidy reads every build tag, so this import keeps both requires here and in sn,
// which builds this package, and no build sets the tag, so no service links either yet. Delete this
// file in the change that adds the first real imports of both.
package server

import (
	_ "github.com/urnetwork/sidecar"
	_ "github.com/urnetwork/sidecar/smtpproof"
)

// Prints what this build of the server package links, or panics at start as a
// binary built with sidecar_pending must outside local and canary. Run by
// TestSidecarPendingBuildTags, which needs a binary that is not a test: a test
// binary is always allowed to link a pending group.
package main

import (
	"fmt"

	"github.com/urnetwork/server"
)

func main() {
	fmt.Println(server.MigrationCount(), server.SidecarMg0Linked())
}

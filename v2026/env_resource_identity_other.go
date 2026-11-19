//go:build !linux && !darwin

// Without a known stat layout there is no change time to observe, so every
// lookup resolves again (see env_resource_cache.go).
package server

import (
	"os"
)

func resolverIdentityOf(info os.FileInfo) (resolverIdentity, bool) {
	return resolverIdentity{}, false
}

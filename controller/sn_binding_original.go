// Binding consent requires a registered key even when the key index is empty.
package controller

import "github.com/urnetwork/server"

// Missing ownership cannot authenticate an otherwise valid self-signed binding.
func snBindingClientKeyMatches(keys map[server.Id][32]byte, clientId server.Id, claimed [32]byte) bool {
	registered, ok := keys[clientId]
	return ok && registered != ([32]byte{}) && registered == claimed
}

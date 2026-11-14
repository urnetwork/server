// What a provider declares it can serve beyond its provide keys (support inbox
// 7918, 7843, https://github.com/urnetwork/connect/issues/220).
//
// A client that requires post-quantum encryption runs every window client
// fail closed, so a provider that cannot seal a post-quantum session can never
// carry its traffic; offered one anyway, the client dials it, evaluates it and
// waits out its failure before it tries the next. A provider declares the
// sessions in each provide (`Provide.post_quantum_encryption`), and
// SetProvideWithCapabilities keeps the declaration of its latest provide that
// registered a Public or Network key in provide_capability.
//
// No selection reads the declaration. A provider counts as post-quantum, or
// modern, only with a probe's positive evidence (FP2-PROTOCOL.md §12.4,
// egress_modern_provider.go), whatever its keys; its own claim never
// qualifies it (owner, 2026-11-13: "They must have a positive quality probe
// handshake to count"). The row stays the record of what the provider
// announced.
package model

// The capabilities one provide declares. The zero value declares none, as
// every provide before a capability existed does.
type ProvideCapabilities struct {
	// the provider serves the per-peer post-quantum encryption sessions a
	// client that requires post-quantum encryption opens
	PostQuantumEncryption bool
}

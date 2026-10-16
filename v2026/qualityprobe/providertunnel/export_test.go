// Bridges for the external capability measurement test, which runs
// server/capprobe's pass over a real tunnel and so cannot be an internal test:
// capprobe imports this package.
package providertunnel

// The local platform and its opportunistic provider (capabilityTestStack).
type CapabilityTestStack = capabilityTestStack

var NewCapabilityTestStack = newCapabilityTestStack

// Keeps every client a tunnel generates gathering WebRTC candidates on
// loopback only, until the returned restore runs.
var UseHermeticTunnelClientSettings = useHermeticTunnelClientSettings

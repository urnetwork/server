package monitor

// SIGNALS.md §23.2 (proxy-h1plus): hosted proxy device RPC must negotiate
// urnetwork-framerxl/1; compact urnetwork-framer/1 is not an XL success.
func NewProxyH1PlusSignal() Signal {
	return &signalAdapter{number: "23.2", key: "proxy-h1plus", name: "Proxy device-RPC H1+ carrier activity", probe: h1PlusProbe{service: "proxy", protocol: "urnetwork-framerxl/1"}}
}

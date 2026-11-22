package nginx

// The front's tunables in one place (LBALT-IMPL.md §2.7): every tunable of
// D7, D8, D12, D19 and D21, and nginx's timeouts. Each group is a settings
// type kept with the unit that owns it, with its own Default...Settings();
// FrontSettings only gathers them, so a test or an owner adjusts one group
// without a second copy of any default.

// Every tunable of one front, by owner. Read-only once passed to NewFront.
type FrontSettings struct {
	// TLS versions, suites and key exchange groups for the fronted names
	// (tls_settings.go)
	Tls *TlsSettings
	// upstream timeouts, attempts and response limits (forward_settings.go;
	// D7)
	Forward *ForwardSettings
	// request body buffering, the memory budget and the spill caps
	// (body_settings.go; D7, D19, D21)
	Body *BodySettings
	// the lifetime of per-address limit state (limit_settings.go; D8)
	Limit *LimitSettings
	// client timeouts, header limits, keepalive, the drain and gzip
	// (route_http_settings.go; D12)
	Http *HttpSettings
}

// A new instance on every call, holding a new instance of each owner's
// defaults, so no two fronts share mutable settings.
func DefaultFrontSettings() *FrontSettings {
	return &FrontSettings{
		Tls:     DefaultTlsSettings(),
		Forward: DefaultForwardSettings(),
		Body:    DefaultBodySettings(),
		Limit:   DefaultLimitSettings(),
		Http:    DefaultHttpSettings(),
	}
}

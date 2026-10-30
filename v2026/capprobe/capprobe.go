// Package capprobe collects the evidence a provider is modern by
// (FP2-MODERN.md, and FP2FIX.md's "Modern provider gate"): whether a
// provider negotiates an end-to-end post-quantum session with a client and
// advertises resizable transfer windows. It is a recurring taskworker task,
// separate from the URL probe: its own prober shard (network, credential
// parent and balance), its own tunnels, each with a transport budget of its
// own, its own concurrency and metrics, none of the URL probe's due queue,
// quota or history.
//
// Each pass continues a sweep over the connected providers (Sweep), a
// bounded page at a time, and selects the providers the egress rules keep in
// Online, as the score export decides it, whose evidence is missing or due
// for a refresh (SelectPass, from model.GetProviderCapabilityProbePage and
// model.FilterProviderCapabilityProbeEligible): the population modern-first
// priority and modern-only requests draw from. FinishPass measures each in
// a short tunnel of its own (providertunnel.CapabilityObservation): the
// tunnel's window pings the provider, its clients attempt opportunistic
// end-to-end sessions after their key registration, and connect's
// PeerCapabilityRecorder records what the provider did. A measurement
// ends once the reading is conclusive or its time is up (measure), and a
// conclusive reading is recorded through model.RecordProviderModernEvidence;
// a capability that was not tested is never recorded (reading).
//
// The URL tunnel's encryption stays off: sealing a URL measurement would
// stall it whenever a provider cannot resolve the probe client's key.
package capprobe

package controller

// sn_ctf_api_source.go — the capture the flag routes' view over the server's
// records of the CTF pool: its mirrored roots, the census, draw and epoch
// results the driver fixed, and the epoch clock.

// newSnCtfCoreSource is the source over the server's records of the CTF pool.
func newSnCtfCoreSource() (snCtfSource, error) {
	return nil, errSnCtfNotConfigured
}

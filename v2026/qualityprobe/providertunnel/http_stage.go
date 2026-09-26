package providertunnel

import "errors"

// Diagnostic metadata at the owned HTTP boundary. The original error text,
// unwrap identity and timeout behavior stay intact for existing callers.
type providerHttpStageError struct {
	stage string
	err   error
}

func (self *providerHttpStageError) Error() string { return self.err.Error() }
func (self *providerHttpStageError) Unwrap() error { return self.err }

// Returns only an owning phase, never the destination or a free-form error.
// A dial includes private DNS and TCP races; this layer cannot split them.
func (self *providerHttpStageError) ProviderHttpStage() string { return self.stage }

// net/http's URL error queries Timeout directly, not through errors.As.
func (self *providerHttpStageError) Timeout() bool {
	var timed interface{ Timeout() bool }
	return errors.As(self.err, &timed) && timed.Timeout()
}

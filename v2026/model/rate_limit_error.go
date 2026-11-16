package model

// rateLimitError is a refusal the client caused rather than a server fault.
//
// It carries the "429 " prefix router.RaiseHttpError peels into the HTTP
// status, plus the retry hint that same function turns into a Retry-After
// header. The header is read off this type through a one-method interface, so
// model does not have to import the router.
type rateLimitError struct {
	message string
	// seconds until the caller's budget is expected to have room. Zero means
	// the window is not known and no Retry-After is sent.
	retryAfterSeconds int
}

func (self *rateLimitError) Error() string {
	return self.message
}

func (self *rateLimitError) RetryAfterSeconds() int {
	return self.retryAfterSeconds
}

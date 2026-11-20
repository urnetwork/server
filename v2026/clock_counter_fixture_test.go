package server

import "testing"

// Installs in-memory clients for the consumer regression without adding a
// production API or making the server package import its model subpackage.
// Tests using this process-wide fixture must not run in parallel.
func TestingClockCounterClients(t *testing.T, ordinaryClient RedisClient, onceClient RedisClient) {
	t.Helper()
	originalClient, originalOnceClient := safeClient, safeNoCommandRetryClient
	safeClient = &safeRedisClient{client: ordinaryClient}
	safeNoCommandRetryClient = &safeRedisClient{client: onceClient, disableCommandRetry: true}
	t.Cleanup(func() {
		safeClient, safeNoCommandRetryClient = originalClient, originalOnceClient
	})
}

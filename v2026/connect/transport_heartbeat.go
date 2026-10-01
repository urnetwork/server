// Application heartbeat cadence follows the peer within the configured bounds.
package connect

import "time"

// Reads the peer's synchronized timing sample without changing its history.
func (self *ConnectHandlerSettings) heartbeatInterval(tracker *PingTracker) time.Duration {
	interval := max(self.MinPingTimeout, tracker.MinPingTimeout())
	// Local receive pressure may stretch an observed peer interval. It must
	// not stretch our next heartbeat past the explicitly configured maximum.
	if 0 < self.MaxPingTimeout {
		interval = min(interval, self.MaxPingTimeout)
	}
	return interval
}

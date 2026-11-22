package connect

import (
	"context"
	"time"
)

// A channel send: waits until the queue has room. Takes on success.
func (self *messageQueue) sendForTest(message []byte) messageQueueSendResult {
	return self.sendWait(message, nil, nil, nil)
}

// A channel receive: waits for the oldest message, and reports false once the
// queue is closed and every message it held was received.
func (self *messageQueue) receiveForTest() ([]byte, bool) {
	message, result := self.receiveWait(nil)
	return message, result == messageQueueReceived
}

// Waits up to timeout for the oldest message. A wait that times out reports
// messageQueueReceiveCanceled.
func (self *messageQueue) receiveWithinForTest(timeout time.Duration) ([]byte, messageQueueReceiveResult) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return self.receiveWait(ctx.Done())
}

// A bounded FIFO queue of pooled messages that allocates as it fills, in place
// of a channel made at its bound (LBALTOPT2 R18 follow-up, R23).
//
// A connect session's exchange queues were channels of ExchangeBufferSize or
// ForwardBufferSize slots, 4096 each, made in full when the session started:
// six of 98 KB, 590 KB of a session's 698 KB setup allocation, for sessions
// that last a median 4.4 s and mostly hold a few messages at a time. A queue
// keeps a channel's semantics:
//
//   - its bound: a queue holds at most bound messages, and a send to a full
//     queue waits or is refused as a channel send would, so backpressure is
//     unchanged;
//   - order: messages leave in the order they entered, and senders waiting on
//     a full queue enter it in the order they began to wait, as a channel's
//     blocked senders do;
//   - close: receivers take every message the queue held, then see it closed,
//     as with a closed channel. A send to a closed queue is refused with
//     messageQueueClosed where a channel send would panic; owners still stop
//     their senders before they close a queue, as they did a channel;
//   - select: a receiver selects on head(), a channel that holds the queue's
//     oldest message, and calls received() after each receive from it.
//
// A queue of bound 0 or 1 is exactly a channel of that capacity, its head.
// Above 1, a message in head is older than every message in the ring, which
// holds the rest in order. A send goes into head when nothing is queued ahead
// of it, else to the ring's end; a receive takes head's message, else the
// ring's first. Only a receiver about to wait on head, through head() and
// received(), moves the ring's first message into head, so a busy link moves
// its messages through the ring under the lock alone, without channel
// operations. The ring is made when the queue first holds two messages and
// doubles from one slot as it fills, up to bound slots, as append grows a
// slice, so what a queue allocates over its life is at most about twice its
// peak. It does not shrink: a queue holds no more than the channel did, and a
// link that fills and drains again and again does not allocate again.
//
// Waits keep their callers' budgets: a sender waits until the queue has room,
// one of its done channels fires, or its timeout fires, as a select on a
// channel send does. A sender admitted to the queue is sent even when its
// timeout fires before it resumes, since the queue then owns its message.
//
// Safe for concurrent use.
package connect

import (
	"sync"
	"time"

	"github.com/urnetwork/connect/v2026"
)

// The outcome of a send to a queue.
type messageQueueSendResult int

const (
	// the queue took the message
	messageQueueSent messageQueueSendResult = iota
	// trySend only: the queue was full
	messageQueueFull
	// a done channel fired before the queue had room
	messageQueueCanceled
	// the timeout fired before the queue had room
	messageQueueTimedOut
	// the queue is closed
	messageQueueClosed
)

// The outcome of a receive from a queue.
type messageQueueReceiveResult int

const (
	// the queue's oldest message was received
	messageQueueReceived messageQueueReceiveResult = iota
	// tryReceive only: no message was ready
	messageQueueEmpty
	// the queue is closed and every message it held was received
	messageQueueDrained
	// receiveWait only: done fired first
	messageQueueReceiveCanceled
)

type messageQueueWaiterState int

const (
	messageQueueWaiterWaiting messageQueueWaiterState = iota
	// the queue took the waiter's message
	messageQueueWaiterAdmitted
	// the queue closed before it had room for the message
	messageQueueWaiterRefused
)

// A sender waiting for room in a full queue. Its state changes under the
// queue's lock, which then signals wake.
type messageQueueWaiter struct {
	message []byte
	state   messageQueueWaiterState
	wake    chan struct{}
}

type messageQueue struct {
	bound int
	// the channel a receiver selects on, with capacity min(bound, 1). Above
	// bound 1, a message in it is older than the ring's, it is filled under
	// the lock, and it closes once the queue is closed and the ring is empty.
	headMessages chan []byte

	stateLock sync.Mutex
	closed    bool
	// whether headMessages is closed, which waits for the ring to empty
	headClosed bool
	// the messages after head's, oldest at ringStart
	ring      [][]byte
	ringStart int
	ringCount int
	// senders waiting for room, oldest first
	waiters []*messageQueueWaiter
	// a waiter kept for reuse, so a sender waiting on a full queue again and
	// again does not allocate
	spareWaiter *messageQueueWaiter
}

// A negative bound is 0.
func newMessageQueue(bound int) *messageQueue {
	bound = max(0, bound)
	return &messageQueue{
		bound:        bound,
		headMessages: make(chan []byte, min(bound, 1)),
	}
}

// Bound 0 or 1: the queue is its head channel.
func (self *messageQueue) isChannel() bool {
	return self.bound <= 1
}

// The messages the queue holds.
func (self *messageQueue) len() int {
	if self.isChannel() {
		return len(self.headMessages)
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.lenWithLock()
}

func (self *messageQueue) capacity() int {
	return self.bound
}

// Head's count only falls outside the lock, as receivers take from it, so the
// count is never more than the queue holds.
func (self *messageQueue) lenWithLock() int {
	return len(self.headMessages) + self.ringCount
}

// The channel a receiver selects on. It holds the queue's oldest message, and
// a receive from it that reports closed means the queue is closed and every
// message it held was received. Call received() after each receive from it.
// Evaluate head() at each select, rather than once before a loop: it first
// moves the oldest queued message into the channel. One receiver at a time
// waits on head; others take messages with tryReceive and tryReceiveBatch,
// which do not refill it.
func (self *messageQueue) head() <-chan []byte {
	if !self.isChannel() {
		func() {
			self.stateLock.Lock()
			defer self.stateLock.Unlock()
			self.admitWithLock()
			self.settleWithLock()
		}()
	}
	return self.headMessages
}

// Follows a receive from head(): gives the room it left to the oldest waiting
// sender, as a channel receive does, and moves the next message into head.
func (self *messageQueue) received() {
	if self.isChannel() {
		return
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.admitWithLock()
	self.settleWithLock()
}

// Receives the oldest message without waiting.
func (self *messageQueue) tryReceive() ([]byte, messageQueueReceiveResult) {
	if self.isChannel() {
		select {
		case message, ok := <-self.headMessages:
			if !ok {
				return nil, messageQueueDrained
			}
			return message, messageQueueReceived
		default:
			return nil, messageQueueEmpty
		}
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	message, result := self.takeWithLock()
	if result == messageQueueReceived {
		self.admitWithLock()
	}
	return message, result
}

// Takes the oldest message: head's, which is older than the ring's, else the
// ring's first. Closes head once a closed queue has given its last message.
func (self *messageQueue) takeWithLock() ([]byte, messageQueueReceiveResult) {
	select {
	case message, ok := <-self.headMessages:
		if !ok {
			// head closes only once the ring is empty
			return nil, messageQueueDrained
		}
		return message, messageQueueReceived
	default:
	}
	if self.ringCount == 0 {
		if self.closed {
			self.settleWithLock()
			return nil, messageQueueDrained
		}
		return nil, messageQueueEmpty
	}
	message := self.ring[self.ringStart]
	self.ring[self.ringStart] = nil
	self.ringStart = (self.ringStart + 1) % len(self.ring)
	self.ringCount -= 1
	if self.closed {
		self.settleWithLock()
	}
	return message, messageQueueReceived
}

// Appends the queue's ready messages to batch, oldest first, while batch holds
// fewer than maxCount messages, byteCount (the bytes in batch so far) is under
// maxByteCount, and stop has not fired: the gather of a batch writer, under
// one lock. Returns the batch, its bytes, and whether the queue was found
// closed with every message received. A nil stop never fires.
func (self *messageQueue) tryReceiveBatch(
	batch [][]byte,
	byteCount ByteCount,
	maxCount int,
	maxByteCount ByteCount,
	stop <-chan struct{},
) ([][]byte, ByteCount, bool) {
	if self.isChannel() {
		return receiveReadyChannelBatch(self.headMessages, batch, byteCount, maxCount, maxByteCount, stop)
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	defer self.admitWithLock()
	for len(batch) < maxCount && byteCount < maxByteCount {
		select {
		case <-stop:
			return batch, byteCount, false
		default:
		}
		message, result := self.takeWithLock()
		switch result {
		case messageQueueEmpty:
			return batch, byteCount, false
		case messageQueueDrained:
			return batch, byteCount, true
		}
		batch = append(batch, message)
		byteCount += ByteCount(len(message))
	}
	return batch, byteCount, false
}

// Waits for the oldest message until there is one, the queue is closed and
// every message it held was received, or done fires. done takes priority over
// a ready message. A nil done never fires.
func (self *messageQueue) receiveWait(done <-chan struct{}) ([]byte, messageQueueReceiveResult) {
	select {
	case <-done:
		return nil, messageQueueReceiveCanceled
	default:
	}
	if message, result := self.tryReceive(); result != messageQueueEmpty {
		return message, result
	}
	// tryReceive found the ring empty, so head needs no settle before the wait
	select {
	case <-done:
		return nil, messageQueueReceiveCanceled
	case message, ok := <-self.headMessages:
		self.received()
		if !ok {
			return nil, messageQueueDrained
		}
		return message, messageQueueReceived
	}
}

// Sends without waiting. Takes on success: a sent message is the queue's, and
// any other result leaves it with the caller.
func (self *messageQueue) trySend(message []byte) messageQueueSendResult {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.closed {
		return messageQueueClosed
	}
	if self.isChannel() {
		select {
		case self.headMessages <- message:
			return messageQueueSent
		default:
			return messageQueueFull
		}
	}
	// room the receivers left goes to the senders already waiting
	self.admitWithLock()
	if 0 < len(self.waiters) || self.bound <= self.lenWithLock() {
		return messageQueueFull
	}
	self.pushWithLock(message)
	return messageQueueSent
}

// Sends, waiting while the queue is full until it has room, done or peerDone
// fires, or timeout fires, as a select on a channel send. A nil channel never
// fires. Takes on success: a sent message is the queue's, and any other result
// leaves it with the caller.
func (self *messageQueue) sendWait(
	message []byte,
	done <-chan struct{},
	peerDone <-chan struct{},
	timeout <-chan time.Time,
) messageQueueSendResult {
	if self.isChannel() {
		// Close must not race a waiting send, as with a channel
		if self.isClosed() {
			return messageQueueClosed
		}
		select {
		case <-done:
			return messageQueueCanceled
		case <-peerDone:
			return messageQueueCanceled
		case self.headMessages <- message:
			return messageQueueSent
		case <-timeout:
			return messageQueueTimedOut
		}
	}
	result, waiter := self.beginSendWait(message)
	if waiter == nil {
		return result
	}
	// wake is signaled only once the queue admitted or refused the waiter,
	// and its state says which
	stopped := messageQueueSent
	select {
	case <-waiter.wake:
	case <-done:
		stopped = messageQueueCanceled
	case <-peerDone:
		stopped = messageQueueCanceled
	case <-timeout:
		stopped = messageQueueTimedOut
	}
	return self.endSendWait(waiter, stopped)
}

func (self *messageQueue) isClosed() bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.closed
}

// Sends when there is room, or enters the waiters. Returns a waiter when the
// sender must wait.
func (self *messageQueue) beginSendWait(message []byte) (messageQueueSendResult, *messageQueueWaiter) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.closed {
		return messageQueueClosed, nil
	}
	self.admitWithLock()
	if len(self.waiters) == 0 && self.lenWithLock() < self.bound {
		self.pushWithLock(message)
		return messageQueueSent, nil
	}
	waiter := self.spareWaiter
	if waiter == nil {
		waiter = &messageQueueWaiter{
			wake: make(chan struct{}, 1),
		}
	} else {
		self.spareWaiter = nil
	}
	waiter.message = message
	waiter.state = messageQueueWaiterWaiting
	self.waiters = append(self.waiters, waiter)
	return messageQueueSent, waiter
}

// Ends a wait. A waiter the queue admitted or refused takes that outcome even
// when its select chose another case first, since the queue then owns, or has
// refused, its message. Otherwise the waiter leaves the queue with stopped.
func (self *messageQueue) endSendWait(
	waiter *messageQueueWaiter,
	stopped messageQueueSendResult,
) messageQueueSendResult {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	result := stopped
	switch waiter.state {
	case messageQueueWaiterAdmitted:
		result = messageQueueSent
	case messageQueueWaiterRefused:
		result = messageQueueClosed
	default:
		for i, w := range self.waiters {
			if w == waiter {
				copy(self.waiters[i:], self.waiters[i+1:])
				self.waiters[len(self.waiters)-1] = nil
				self.waiters = self.waiters[:len(self.waiters)-1]
				break
			}
		}
	}
	// out of the queue, nothing signals the waiter again: drain a signal that
	// raced its select, and keep it for the next wait
	select {
	case <-waiter.wake:
	default:
	}
	waiter.message = nil
	waiter.state = messageQueueWaiterWaiting
	if self.spareWaiter == nil {
		self.spareWaiter = waiter
	}
	return result
}

// Closes the queue. Receivers take the messages it holds, then see it closed.
// Waiting senders return messageQueueClosed, keeping their messages, and so do
// later sends. Closing again does nothing.
func (self *messageQueue) close() {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.closed {
		return
	}
	self.closed = true
	for _, waiter := range self.waiters {
		waiter.state = messageQueueWaiterRefused
		select {
		case waiter.wake <- struct{}{}:
		default:
		}
	}
	clear(self.waiters)
	self.waiters = self.waiters[:0]
	self.settleWithLock()
}

// Gives the queue's room to the oldest waiting senders.
func (self *messageQueue) admitWithLock() {
	for 0 < len(self.waiters) && self.lenWithLock() < self.bound {
		waiter := self.waiters[0]
		copy(self.waiters, self.waiters[1:])
		self.waiters[len(self.waiters)-1] = nil
		self.waiters = self.waiters[:len(self.waiters)-1]
		self.pushWithLock(waiter.message)
		waiter.message = nil
		waiter.state = messageQueueWaiterAdmitted
		select {
		case waiter.wake <- struct{}{}:
		default:
		}
	}
}

// Appends a message after the queue's others: into head when nothing is
// queued, so a receiver waiting on head takes it at once, else at the ring's
// end. The caller checked for room.
func (self *messageQueue) pushWithLock(message []byte) {
	if self.ringCount == 0 {
		select {
		case self.headMessages <- message:
			return
		default:
		}
	}
	if self.ringCount == len(self.ring) {
		// double, as append grows a slice, up to the bound
		ring := make([][]byte, min(max(1, 2*len(self.ring)), self.bound))
		for i := range self.ringCount {
			ring[i] = self.ring[(self.ringStart+i)%len(self.ring)]
		}
		self.ring = ring
		self.ringStart = 0
	}
	self.ring[(self.ringStart+self.ringCount)%len(self.ring)] = message
	self.ringCount += 1
}

// Moves the ring's oldest messages into head while it has room: to a receiver
// waiting on head, or into its slot. Closes head once the queue is closed and
// the ring is empty.
func (self *messageQueue) settleWithLock() {
	for 0 < self.ringCount {
		select {
		case self.headMessages <- self.ring[self.ringStart]:
			self.ring[self.ringStart] = nil
			self.ringStart = (self.ringStart + 1) % len(self.ring)
			self.ringCount -= 1
		default:
			return
		}
	}
	if self.closed && !self.headClosed {
		self.headClosed = true
		close(self.headMessages)
	}
}

// The gather of a batch writer from a channel: ready messages, without
// waiting, under the limits of tryReceiveBatch.
func receiveReadyChannelBatch(
	messages <-chan []byte,
	batch [][]byte,
	byteCount ByteCount,
	maxCount int,
	maxByteCount ByteCount,
	stop <-chan struct{},
) ([][]byte, ByteCount, bool) {
	for len(batch) < maxCount && byteCount < maxByteCount {
		select {
		case <-stop:
			return batch, byteCount, false
		default:
		}
		select {
		case message, ok := <-messages:
			if !ok {
				return batch, byteCount, true
			}
			batch = append(batch, message)
			byteCount += ByteCount(len(message))
		default:
			return batch, byteCount, false
		}
	}
	return batch, byteCount, false
}

// Returns every pooled message the queue holds to the pool. The queue may be
// open or closed, and may have another receiver during teardown.
func returnQueuedPooledMessages(queue *messageQueue) {
	for {
		message, result := queue.tryReceive()
		if result != messageQueueReceived {
			return
		}
		connect.MessagePoolReturn(message)
	}
}

// Where a writer takes pooled messages from: a messageQueue, or a channel as
// pooledMessageChannel.
type pooledMessageSource interface {
	head() <-chan []byte
	received()
	tryReceive() ([]byte, messageQueueReceiveResult)
	tryReceiveBatch(batch [][]byte, byteCount ByteCount, maxCount int, maxByteCount ByteCount, stop <-chan struct{}) ([][]byte, ByteCount, bool)
	len() int
	capacity() int
}

// Where a pooled message is sent: a messageQueue, or a channel as
// pooledMessageChannel. Both sends take on success.
type pooledMessageDestination interface {
	trySend(message []byte) messageQueueSendResult
	sendWait(message []byte, done <-chan struct{}, peerDone <-chan struct{}, timeout <-chan time.Time) messageQueueSendResult
}

// A channel as a pooledMessageSource and pooledMessageDestination, with a
// channel's own semantics: a resident's routes, which the connect client's
// route manager owns as channels, and a resident's forward queue.
type pooledMessageChannel chan []byte

func (self pooledMessageChannel) head() <-chan []byte {
	return self
}

func (self pooledMessageChannel) received() {
}

func (self pooledMessageChannel) tryReceive() ([]byte, messageQueueReceiveResult) {
	select {
	case message, ok := <-self:
		if !ok {
			return nil, messageQueueDrained
		}
		return message, messageQueueReceived
	default:
		return nil, messageQueueEmpty
	}
}

func (self pooledMessageChannel) tryReceiveBatch(
	batch [][]byte,
	byteCount ByteCount,
	maxCount int,
	maxByteCount ByteCount,
	stop <-chan struct{},
) ([][]byte, ByteCount, bool) {
	return receiveReadyChannelBatch(self, batch, byteCount, maxCount, maxByteCount, stop)
}

func (self pooledMessageChannel) len() int {
	return len(self)
}

func (self pooledMessageChannel) capacity() int {
	return cap(self)
}

func (self pooledMessageChannel) trySend(message []byte) messageQueueSendResult {
	select {
	case self <- message:
		return messageQueueSent
	default:
		return messageQueueFull
	}
}

func (self pooledMessageChannel) sendWait(
	message []byte,
	done <-chan struct{},
	peerDone <-chan struct{},
	timeout <-chan time.Time,
) messageQueueSendResult {
	select {
	case <-done:
		return messageQueueCanceled
	case <-peerDone:
		return messageQueueCanceled
	case self <- message:
		return messageQueueSent
	case <-timeout:
		return messageQueueTimedOut
	}
}

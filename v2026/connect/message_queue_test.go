// Tests of messageQueue against a channel's semantics: its bound,
// backpressure, order, growth, close and drain, select, the two orderings of a
// timed wait, and concurrent producers and consumers (meant for -race). Waits
// are ordered with testing/synctest, so a sender is known to be waiting before
// a receiver frees room, and timeouts run on the bubble's clock.
package connect

import (
	"context"
	"encoding/binary"
	"fmt"
	"runtime"
	"sync"
	"testing"
	"testing/synctest"
	"time"
	"weak"

	clientconnect "github.com/urnetwork/connect/v2026"
)

// An 8-byte message that carries its index.
func newMessageQueueTestMessage(index int) []byte {
	message := make([]byte, 8)
	binary.BigEndian.PutUint64(message, uint64(index))
	return message
}

func messageQueueTestMessageIndex(message []byte) int {
	return int(binary.BigEndian.Uint64(message))
}

// Takes count messages without waiting and checks they carry the indexes
// first, first+1, ...
func requireMessageQueueOrder(t *testing.T, queue *messageQueue, first int, count int) {
	t.Helper()
	for i := range count {
		message, result := queue.tryReceive()
		if result != messageQueueReceived {
			t.Fatalf("message %d of %d: result=%d, want received", i, count, result)
		}
		if got := messageQueueTestMessageIndex(message); got != first+i {
			t.Fatalf("message %d of %d has index %d, want %d", i, count, got, first+i)
		}
	}
}

// The ring's slots.
func (self *messageQueue) ringCapacityForTest() int {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return len(self.ring)
}

func (self *messageQueue) waiterCountForTest() int {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return len(self.waiters)
}

// A queue takes exactly bound messages without waiting, then is full, and gives
// them back in order.
func TestMessageQueueHoldsExactlyItsBound(t *testing.T) {
	for _, bound := range []int{1, 2, 3, 5, 64, 4096} {
		queue := newMessageQueue(bound)
		for i := range bound {
			if result := queue.trySend(newMessageQueueTestMessage(i)); result != messageQueueSent {
				t.Fatalf("bound %d: send %d result=%d, want sent", bound, i, result)
			}
		}
		if result := queue.trySend(newMessageQueueTestMessage(bound)); result != messageQueueFull {
			t.Fatalf("bound %d: send past the bound result=%d, want full", bound, result)
		}
		if queue.len() != bound || queue.capacity() != bound {
			t.Fatalf("bound %d: len=%d capacity=%d", bound, queue.len(), queue.capacity())
		}
		if ring := queue.ringCapacityForTest(); bound < ring {
			t.Fatalf("bound %d: ring of %d slots exceeds the bound", bound, ring)
		}
		requireMessageQueueOrder(t, queue, 0, bound)
		if _, result := queue.tryReceive(); result != messageQueueEmpty {
			t.Fatalf("bound %d: drained queue result=%d, want empty", bound, result)
		}
	}
}

// A negative bound is an unbuffered queue, where a channel would panic.
func TestMessageQueueNegativeBoundIsUnbuffered(t *testing.T) {
	queue := newMessageQueue(-3)
	if queue.capacity() != 0 || cap(queue.headMessages) != 0 {
		t.Fatalf("negative bound: capacity=%d head=%d, want 0", queue.capacity(), cap(queue.headMessages))
	}
}

// Bound 0 is a rendezvous, as an unbuffered channel: a send without waiting
// succeeds only into a receiver already waiting, and a waiting send completes
// only when a receiver takes its message.
func TestMessageQueueUnbufferedIsARendezvous(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		queue := newMessageQueue(0)
		if result := queue.trySend(newMessageQueueTestMessage(0)); result != messageQueueFull {
			t.Fatalf("no receiver: result=%d, want full", result)
		}

		received := make(chan []byte, 1)
		go func() {
			message, _ := queue.receiveForTest()
			received <- message
		}()
		synctest.Wait()
		if result := queue.trySend(newMessageQueueTestMessage(1)); result != messageQueueSent {
			t.Fatalf("waiting receiver: result=%d, want sent", result)
		}
		if got := messageQueueTestMessageIndex(<-received); got != 1 {
			t.Fatalf("receiver got %d, want 1", got)
		}

		sent := make(chan messageQueueSendResult, 1)
		go func() {
			sent <- queue.sendWait(newMessageQueueTestMessage(2), nil, nil, nil)
		}()
		synctest.Wait()
		select {
		case result := <-sent:
			t.Fatalf("send completed without a receiver: %d", result)
		default:
		}
		message, ok := queue.receiveForTest()
		if !ok || messageQueueTestMessageIndex(message) != 2 || <-sent != messageQueueSent {
			t.Fatal("waiting send did not hand its message to the receiver")
		}
	})
}

// Senders waiting on a full queue enter it in the order they began to wait,
// as a channel's blocked senders do, and a later send without waiting does not
// take the room ahead of them.
func TestMessageQueueWaitingSendersEnterInOrder(t *testing.T) {
	for _, bound := range []int{1, 2, 3, 16} {
		synctest.Test(t, func(t *testing.T) {
			queue := newMessageQueue(bound)
			for i := range bound {
				queue.sendForTest(newMessageQueueTestMessage(i))
			}
			results := make([]chan messageQueueSendResult, 3)
			for w := range results {
				results[w] = make(chan messageQueueSendResult, 1)
				go func() {
					results[w] <- queue.sendWait(newMessageQueueTestMessage(bound+w), nil, nil, nil)
				}()
				// each waits before the next begins
				synctest.Wait()
			}
			for w := range results {
				select {
				case result := <-results[w]:
					t.Fatalf("bound %d: waiter %d returned %d on a full queue", bound, w, result)
				default:
				}
			}

			// the first receive's room goes to the first waiter, not to a new send
			requireMessageQueueOrder(t, queue, 0, 1)
			if result := queue.trySend(newMessageQueueTestMessage(-1)); result != messageQueueFull {
				t.Fatalf("bound %d: a new send took room ahead of waiting senders: %d", bound, result)
			}
			synctest.Wait()
			if result := <-results[0]; result != messageQueueSent {
				t.Fatalf("bound %d: first waiter result=%d, want sent", bound, result)
			}
			// a receive through head() and received() admits the next waiter
			select {
			case message := <-queue.head():
				queue.received()
				if got := messageQueueTestMessageIndex(message); got != 1 {
					t.Fatalf("bound %d: head held %d, want 1", bound, got)
				}
			default:
				t.Fatalf("bound %d: head held no message", bound)
			}
			synctest.Wait()
			if result := <-results[1]; result != messageQueueSent {
				t.Fatalf("bound %d: second waiter result=%d, want sent", bound, result)
			}
			requireMessageQueueOrder(t, queue, 2, bound)
			synctest.Wait()
			if result := <-results[2]; result != messageQueueSent {
				t.Fatalf("bound %d: third waiter result=%d, want sent", bound, result)
			}
			requireMessageQueueOrder(t, queue, bound+2, 1)
		})
	}
}

// Messages leave in order while the ring wraps and grows under them.
func TestMessageQueueKeepsOrderAcrossRingWrapAndGrowth(t *testing.T) {
	queue := newMessageQueue(64)
	next, expected := 0, 0
	send := func(count int) {
		for range count {
			if result := queue.trySend(newMessageQueueTestMessage(next)); result != messageQueueSent {
				t.Fatalf("send %d result=%d", next, result)
			}
			next += 1
		}
	}
	receive := func(count int) {
		requireMessageQueueOrder(t, queue, expected, count)
		expected += count
	}
	send(10)
	receive(7)
	// the ring's start is past its first slot: this wraps, then grows
	send(30)
	receive(20)
	send(41)
	receive(54)
	if queue.len() != 0 {
		t.Fatalf("queue holds %d, want 0", queue.len())
	}
}

// The ring is made when the queue first holds two messages, doubles from one
// slot as it fills, stops at the bound, and keeps its slots after it drains,
// so a link that fills again does not allocate.
func TestMessageQueueRingGrowsByDoublingUpToItsBound(t *testing.T) {
	const bound = 100
	queue := newMessageQueue(bound)
	queue.trySend(newMessageQueueTestMessage(0))
	if ring := queue.ringCapacityForTest(); ring != 0 {
		t.Fatalf("one message made a ring of %d slots", ring)
	}
	for held := 2; held <= bound; held += 1 {
		queue.trySend(newMessageQueueTestMessage(held - 1))
		// head holds one, the ring the rest
		want := 1
		for want < held-1 {
			want *= 2
		}
		want = min(want, bound)
		if ring := queue.ringCapacityForTest(); ring != want {
			t.Fatalf("holding %d: ring of %d slots, want %d", held, ring, want)
		}
	}
	requireMessageQueueOrder(t, queue, 0, bound)
	if ring := queue.ringCapacityForTest(); ring != bound {
		t.Fatalf("drained ring has %d slots, want %d kept", ring, bound)
	}
	messages := make([][]byte, bound)
	for i := range messages {
		messages[i] = newMessageQueueTestMessage(i)
	}
	refill := func() {
		for _, message := range messages {
			queue.trySend(message)
		}
		for range messages {
			queue.tryReceive()
		}
	}
	if allocations := testing.AllocsPerRun(20, refill); allocations != 0 {
		t.Fatalf("refilling a grown queue allocated %.1f times", allocations)
	}
}

// A queue that passes one message at a time never makes a ring, and neither a
// send nor a receive allocates.
func TestMessageQueueSteadyStateDoesNotAllocate(t *testing.T) {
	for _, bound := range []int{1, 2, 4096} {
		queue := newMessageQueue(bound)
		message := newMessageQueueTestMessage(0)
		allocations := testing.AllocsPerRun(1000, func() {
			if queue.trySend(message) != messageQueueSent {
				panic("send refused")
			}
			if _, result := queue.tryReceive(); result != messageQueueReceived {
				panic("receive empty")
			}
		})
		if allocations != 0 {
			t.Fatalf("bound %d: a send and a receive allocated %.1f times", bound, allocations)
		}
		if ring := queue.ringCapacityForTest(); ring != 0 {
			t.Fatalf("bound %d: one message at a time made a ring of %d slots", bound, ring)
		}
	}
	// a sender that waits again and again reuses its waiter
	queue := newMessageQueue(2)
	queue.trySend(newMessageQueueTestMessage(0))
	queue.trySend(newMessageQueueTestMessage(1))
	message := newMessageQueueTestMessage(2)
	allocations := testing.AllocsPerRun(1000, func() {
		_, waiter := queue.beginSendWait(message)
		if waiter == nil || queue.endSendWait(waiter, messageQueueTimedOut) != messageQueueTimedOut {
			panic("full queue did not keep the sender waiting")
		}
	})
	if allocations != 0 {
		t.Fatalf("a repeated wait allocated %.1f times", allocations)
	}
}

// A received message leaves no reference in the queue: its ring slot and head
// are cleared, so a live queue does not keep a drained burst reachable.
func TestMessageQueueReleasesReceivedPayloads(t *testing.T) {
	const bound = 8
	queue := newMessageQueue(bound)
	witnesses := make([]weak.Pointer[byte], bound)
	func() {
		for i := range witnesses {
			payload := make([]byte, 16*1024)
			witnesses[i] = weak.Make(&payload[0])
			queue.trySend(payload)
		}
	}()
	for range bound {
		queue.tryReceive()
	}
	runtime.GC()
	runtime.GC()
	for i, witness := range witnesses {
		if witness.Value() != nil {
			t.Fatalf("payload %d is still reachable from the drained queue", i)
		}
	}
	runtime.KeepAlive(queue)
}

// A closed queue gives every message it held, in order, then reports drained
// through each receive; a send to it is refused.
func TestMessageQueueCloseDeliversHeldMessagesThenDrained(t *testing.T) {
	for _, bound := range []int{0, 1, 2, 5} {
		queue := newMessageQueue(bound)
		for i := range bound {
			queue.trySend(newMessageQueueTestMessage(i))
		}
		queue.close()
		queue.close()
		if result := queue.trySend(newMessageQueueTestMessage(-1)); result != messageQueueClosed {
			t.Fatalf("bound %d: send to a closed queue result=%d", bound, result)
		}
		if result := queue.sendWait(newMessageQueueTestMessage(-1), nil, nil, nil); result != messageQueueClosed {
			t.Fatalf("bound %d: waiting send to a closed queue result=%d", bound, result)
		}
		if queue.len() != bound {
			t.Fatalf("bound %d: closed queue holds %d", bound, queue.len())
		}
		requireMessageQueueOrder(t, queue, 0, bound)
		if _, result := queue.tryReceive(); result != messageQueueDrained {
			t.Fatalf("bound %d: tryReceive result=%d, want drained", bound, result)
		}
		if _, result := queue.receiveWait(nil); result != messageQueueDrained {
			t.Fatalf("bound %d: receiveWait result=%d, want drained", bound, result)
		}
		if _, ok := <-queue.head(); ok {
			t.Fatalf("bound %d: head gave a message after the drain", bound)
		}
		if _, _, drained := queue.tryReceiveBatch(nil, 0, 8, 1024, nil); !drained {
			t.Fatalf("bound %d: batch did not report drained", bound)
		}
	}
}

// Close wakes a receiver waiting on head, and only once the ring has drained.
func TestMessageQueueCloseWakesWaitingReceiversAfterTheDrain(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		queue := newMessageQueue(4)
		results := make(chan messageQueueReceiveResult, 1)
		go func() {
			_, result := queue.receiveWait(nil)
			results <- result
		}()
		synctest.Wait()
		queue.close()
		synctest.Wait()
		if result := <-results; result != messageQueueDrained {
			t.Fatalf("waiting receiver result=%d, want drained", result)
		}

		// a batch that takes the last message reports the drain
		queue = newMessageQueue(4)
		for i := range 3 {
			queue.trySend(newMessageQueueTestMessage(i))
		}
		queue.close()
		batch, _, drained := queue.tryReceiveBatch(nil, 0, 2, 1024, nil)
		if len(batch) != 2 || drained {
			t.Fatalf("first batch took %d drained=%t, want 2 and not drained", len(batch), drained)
		}
		batch, _, drained = queue.tryReceiveBatch(nil, 0, 8, 1024, nil)
		if len(batch) != 1 || messageQueueTestMessageIndex(batch[0]) != 2 || !drained {
			t.Fatalf("last batch took %d drained=%t, want the last message and drained", len(batch), drained)
		}
	})
}

// Close refuses waiting senders, which keep their messages.
func TestMessageQueueCloseRefusesWaitingSenders(t *testing.T) {
	// the pool starts its stats goroutine on first use, which must not be in
	// the bubble
	clientconnect.MessagePoolReturn(clientconnect.MessagePoolGet(64))
	for _, bound := range []int{2, 7} {
		synctest.Test(t, func(t *testing.T) {
			queue := newMessageQueue(bound)
			for i := range bound {
				queue.trySend(newMessageQueueTestMessage(i))
			}
			message := clientconnect.MessagePoolGet(64)
			witness := retainResidentPoolWitness(message)
			results := make(chan messageQueueSendResult, 1)
			go func() {
				results <- queue.sendWait(message, nil, nil, nil)
			}()
			synctest.Wait()
			queue.close()
			synctest.Wait()
			if result := <-results; result != messageQueueClosed {
				t.Fatalf("bound %d: waiting sender result=%d, want closed", bound, result)
			}
			requireMessageQueueOrder(t, queue, 0, bound)
			if _, result := queue.tryReceive(); result != messageQueueDrained {
				t.Fatalf("bound %d: the refused message entered the queue", bound)
			}
			// the sender still owns its message
			clientconnect.MessagePoolReturn(message)
			requireResidentPoolOwnerReturned(t, witness, "refused waiting send")
			if queue.waiterCountForTest() != 0 {
				t.Fatalf("bound %d: close left waiters", bound)
			}
		})
	}
}

// A send that finds no room within its budget ends exactly at its timeout,
// and its message stays the caller's.
func TestMessageQueueStalledSendEndsAtItsTimeout(t *testing.T) {
	for _, bound := range []int{0, 1, 2, 9} {
		synctest.Test(t, func(t *testing.T) {
			queue := newMessageQueue(bound)
			for i := range bound {
				queue.trySend(newMessageQueueTestMessage(i))
			}
			timer := time.NewTimer(time.Second)
			defer timer.Stop()
			start := time.Now()
			if result := queue.sendWait(newMessageQueueTestMessage(bound), nil, nil, timer.C); result != messageQueueTimedOut {
				t.Fatalf("bound %d: stalled send result=%d, want timed out", bound, result)
			}
			if waited := time.Since(start); waited != time.Second {
				t.Fatalf("bound %d: stalled send waited %s, want its 1s budget", bound, waited)
			}
			requireMessageQueueOrder(t, queue, 0, bound)
			if _, result := queue.tryReceive(); result != messageQueueEmpty {
				t.Fatalf("bound %d: a timed-out message entered the queue", bound)
			}
			if 1 < bound && queue.waiterCountForTest() != 0 {
				t.Fatalf("bound %d: a timed-out sender is still waiting", bound)
			}
		})
	}
}

// A send that gets room 1 ns before its timeout is sent: a healthy wait keeps
// its whole budget.
func TestMessageQueueHealthySendKeepsItsFullBudget(t *testing.T) {
	for _, bound := range []int{0, 1, 2, 9} {
		synctest.Test(t, func(t *testing.T) {
			queue := newMessageQueue(bound)
			for i := range bound {
				queue.trySend(newMessageQueueTestMessage(i))
			}
			results := make(chan messageQueueSendResult, 1)
			go func() {
				timer := time.NewTimer(time.Second)
				defer timer.Stop()
				results <- queue.sendWait(newMessageQueueTestMessage(bound), nil, nil, timer.C)
			}()
			time.Sleep(time.Second - time.Nanosecond)
			// room opens at the last instant of the budget
			received := make(chan struct{})
			go func() {
				defer close(received)
				for i := range bound + 1 {
					message, _ := queue.receiveForTest()
					if got := messageQueueTestMessageIndex(message); got != i {
						t.Errorf("bound %d: received %d, want %d", bound, got, i)
					}
				}
			}()
			if result := <-results; result != messageQueueSent {
				t.Fatalf("bound %d: send with room inside its budget result=%d, want sent", bound, result)
			}
			<-received
		})
	}
}

// The two orderings of a wait that ends: a waiter the queue admitted is sent
// even when its timeout or cancellation won the race to resume it, since the
// queue owns its message; a waiter not admitted leaves the queue with the
// cause.
func TestMessageQueueWaitEndOrderings(t *testing.T) {
	for _, stopped := range []messageQueueSendResult{messageQueueTimedOut, messageQueueCanceled} {
		// admitted, then the timeout or cancellation resumes it
		queue := newMessageQueue(2)
		queue.trySend(newMessageQueueTestMessage(0))
		queue.trySend(newMessageQueueTestMessage(1))
		_, waiter := queue.beginSendWait(newMessageQueueTestMessage(2))
		if waiter == nil {
			t.Fatal("full queue did not keep the sender waiting")
		}
		requireMessageQueueOrder(t, queue, 0, 1)
		if result := queue.endSendWait(waiter, stopped); result != messageQueueSent {
			t.Fatalf("admitted waiter ended by %d result=%d, want sent", stopped, result)
		}
		requireMessageQueueOrder(t, queue, 1, 2)

		// still waiting when the timeout or cancellation resumes it
		queue = newMessageQueue(2)
		queue.trySend(newMessageQueueTestMessage(0))
		queue.trySend(newMessageQueueTestMessage(1))
		_, waiter = queue.beginSendWait(newMessageQueueTestMessage(2))
		if result := queue.endSendWait(waiter, stopped); result != stopped {
			t.Fatalf("waiting sender ended by %d result=%d", stopped, result)
		}
		if queue.waiterCountForTest() != 0 {
			t.Fatal("an ended waiter stayed in the queue")
		}
		requireMessageQueueOrder(t, queue, 0, 2)
		if _, result := queue.tryReceive(); result != messageQueueEmpty {
			t.Fatal("an ended waiter's message entered the queue")
		}

		// refused by close, then the timeout or cancellation resumes it
		queue = newMessageQueue(2)
		queue.trySend(newMessageQueueTestMessage(0))
		queue.trySend(newMessageQueueTestMessage(1))
		_, waiter = queue.beginSendWait(newMessageQueueTestMessage(2))
		queue.close()
		if result := queue.endSendWait(waiter, stopped); result != messageQueueClosed {
			t.Fatalf("refused waiter ended by %d result=%d, want closed", stopped, result)
		}
	}
}

// A waiting send ends when either done channel fires.
func TestMessageQueueSendWaitEndsOnDone(t *testing.T) {
	for _, bound := range []int{0, 1, 2} {
		for _, peer := range []bool{false, true} {
			synctest.Test(t, func(t *testing.T) {
				queue := newMessageQueue(bound)
				for i := range bound {
					queue.trySend(newMessageQueueTestMessage(i))
				}
				ctx, cancel := context.WithCancel(context.Background())
				results := make(chan messageQueueSendResult, 1)
				go func() {
					if peer {
						results <- queue.sendWait(newMessageQueueTestMessage(bound), nil, ctx.Done(), nil)
					} else {
						results <- queue.sendWait(newMessageQueueTestMessage(bound), ctx.Done(), nil, nil)
					}
				}()
				synctest.Wait()
				cancel()
				if result := <-results; result != messageQueueCanceled {
					t.Fatalf("bound %d peer %t: result=%d, want canceled", bound, peer, result)
				}
				requireMessageQueueOrder(t, queue, 0, bound)
				if _, result := queue.tryReceive(); result != messageQueueEmpty {
					t.Fatalf("bound %d peer %t: a canceled message entered the queue", bound, peer)
				}
			})
		}
	}
}

// A done receive does not take a ready message.
func TestMessageQueueReceiveWaitPrefersDone(t *testing.T) {
	for _, bound := range []int{1, 3} {
		queue := newMessageQueue(bound)
		queue.trySend(newMessageQueueTestMessage(0))
		done := make(chan struct{})
		close(done)
		if _, result := queue.receiveWait(done); result != messageQueueReceiveCanceled {
			t.Fatalf("bound %d: done receive result=%d, want canceled", bound, result)
		}
		requireMessageQueueOrder(t, queue, 0, 1)
	}
}

// A receiver selecting on head beside other channels takes every message in
// order, whichever case wakes it.
func TestMessageQueueReceiverSelectsWithOtherChannels(t *testing.T) {
	for _, bound := range []int{0, 1, 2, 16} {
		synctest.Test(t, func(t *testing.T) {
			const count = 200
			queue := newMessageQueue(bound)
			kick := make(chan struct{}, 1)
			go func() {
				for i := range count {
					if result := queue.sendForTest(newMessageQueueTestMessage(i)); result != messageQueueSent {
						t.Errorf("send %d result=%d", i, result)
					}
					if i%7 == 0 {
						select {
						case kick <- struct{}{}:
						default:
						}
					}
				}
				queue.close()
			}()
			next, kicks := 0, 0
			for {
				select {
				case <-kick:
					kicks += 1
					continue
				case message, ok := <-queue.head():
					queue.received()
					if !ok {
						if next != count {
							t.Fatalf("bound %d: received %d of %d", bound, next, count)
						}
						return
					}
					if got := messageQueueTestMessageIndex(message); got != next {
						t.Fatalf("bound %d: received %d, want %d", bound, got, next)
					}
					next += 1
				}
			}
		})
	}
}

// A batch takes ready messages while it is under its count and its bytes so
// far are under the byte limit, as a writer's gather does, and stops when its
// stop channel has fired. A channel gives the same batches.
func TestMessageQueueTryReceiveBatchLimits(t *testing.T) {
	cases := []struct {
		name         string
		maxCount     int
		maxByteCount ByteCount
		stop         bool
		want         int
	}{
		{name: "count", maxCount: 4, maxByteCount: 1 << 20, want: 4},
		// 0, 100, 200 are under 250: the third message is taken and crosses it
		{name: "bytes", maxCount: 64, maxByteCount: 250, want: 3},
		{name: "all", maxCount: 64, maxByteCount: 1 << 20, want: 10},
		{name: "stop", maxCount: 64, maxByteCount: 1 << 20, stop: true, want: 0},
	}
	for _, c := range cases {
		queue := newMessageQueue(16)
		channel := make(chan []byte, 16)
		for i := range 10 {
			message := make([]byte, 100)
			binary.BigEndian.PutUint64(message, uint64(i))
			queue.trySend(message)
			channel <- message
		}
		var stop chan struct{}
		if c.stop {
			stop = make(chan struct{})
			close(stop)
		}
		for _, source := range []pooledMessageSource{queue, pooledMessageChannel(channel)} {
			batch, byteCount, drained := source.tryReceiveBatch(nil, 0, c.maxCount, c.maxByteCount, stop)
			if len(batch) != c.want || byteCount != ByteCount(100*c.want) || drained {
				t.Fatalf("%s %T: took %d (%d bytes) drained=%t, want %d", c.name, source, len(batch), byteCount, drained, c.want)
			}
			for i, message := range batch {
				if got := messageQueueTestMessageIndex(message); got != i {
					t.Fatalf("%s %T: batch message %d has index %d", c.name, source, i, got)
				}
			}
			if source.len() != 10-c.want {
				t.Fatalf("%s %T: %d left, want %d", c.name, source, source.len(), 10-c.want)
			}
		}
	}
}

// A channel keeps its own semantics behind the source and destination
// interfaces.
func TestPooledMessageChannelKeepsChannelSemantics(t *testing.T) {
	channel := make(chan []byte, 1)
	var destination pooledMessageDestination = pooledMessageChannel(channel)
	if result := destination.trySend(newMessageQueueTestMessage(0)); result != messageQueueSent {
		t.Fatalf("send to an empty channel result=%d", result)
	}
	if result := destination.trySend(newMessageQueueTestMessage(1)); result != messageQueueFull {
		t.Fatalf("send to a full channel result=%d", result)
	}
	timeout := make(chan time.Time, 1)
	timeout <- time.Now()
	if result := destination.sendWait(newMessageQueueTestMessage(1), nil, nil, timeout); result != messageQueueTimedOut {
		t.Fatalf("waiting send to a full channel result=%d", result)
	}
	done := make(chan struct{})
	close(done)
	if result := destination.sendWait(newMessageQueueTestMessage(1), done, nil, nil); result != messageQueueCanceled {
		t.Fatalf("done send to a full channel result=%d", result)
	}
	var source pooledMessageSource = pooledMessageChannel(channel)
	if message, result := source.tryReceive(); result != messageQueueReceived || messageQueueTestMessageIndex(message) != 0 {
		t.Fatal("channel receive lost its message")
	}
	if _, result := source.tryReceive(); result != messageQueueEmpty {
		t.Fatalf("empty channel receive result=%d", result)
	}
	close(channel)
	if _, result := source.tryReceive(); result != messageQueueDrained {
		t.Fatalf("closed channel receive result=%d", result)
	}
	if source.capacity() != 1 || source.len() != 0 {
		t.Fatalf("channel capacity=%d len=%d", source.capacity(), source.len())
	}
}

// Teardown returns every message an open or closed queue holds to the pool.
func TestReturnQueuedPooledMessagesDrainsQueuedOwnership(t *testing.T) {
	for _, bound := range []int{1, 2, 6} {
		for _, closed := range []bool{false, true} {
			queue := newMessageQueue(bound)
			witnesses := make([][]byte, 0, bound)
			for range bound {
				message := clientconnect.MessagePoolGet(128)
				witnesses = append(witnesses, retainResidentPoolWitness(message))
				queue.trySend(message)
			}
			if closed {
				queue.close()
			}
			returnQueuedPooledMessages(queue)
			if queue.len() != 0 {
				t.Fatalf("bound %d closed %t: %d messages remain", bound, closed, queue.len())
			}
			requireResidentPoolOwnersReturned(t, witnesses, fmt.Sprintf("bound %d closed %t drain", bound, closed))
		}
	}
}

// Producers that send, wait, time out and give up, against consumers that
// receive every way a caller does. Every sent message is received exactly
// once, each consumer sees each producer's messages in order, the queue never
// holds more than its bound, and a message whose send did not succeed is never
// received. Meant for -race.
func TestMessageQueueConcurrentProducersAndConsumers(t *testing.T) {
	const producerCount = 4
	const consumerCount = 4
	const messageCount = 3000
	for _, bound := range []int{0, 1, 2, 7, 64} {
		queue := newMessageQueue(bound)
		ctx, cancel := context.WithCancel(context.Background())
		sentFlags := make([][]bool, producerCount)
		var producers sync.WaitGroup
		for producer := range producerCount {
			sentFlags[producer] = make([]bool, messageCount)
			producers.Add(1)
			go func() {
				defer producers.Done()
				timer := time.NewTimer(time.Hour)
				defer timer.Stop()
				for i := range messageCount {
					message := make([]byte, 16)
					binary.BigEndian.PutUint64(message, uint64(producer))
					binary.BigEndian.PutUint64(message[8:], uint64(i))
					var result messageQueueSendResult
					switch i % 4 {
					case 0:
						result = queue.trySend(message)
						if result == messageQueueFull {
							result = queue.sendWait(message, ctx.Done(), nil, nil)
						}
					case 1:
						timer.Reset(50 * time.Microsecond)
						result = queue.sendWait(message, nil, nil, timer.C)
					default:
						result = queue.sendWait(message, ctx.Done(), nil, nil)
					}
					if result == messageQueueSent {
						sentFlags[producer][i] = true
					} else if result != messageQueueTimedOut {
						t.Errorf("bound %d: producer %d message %d result=%d", bound, producer, i, result)
						return
					}
					if bound < queue.len() {
						t.Errorf("bound %d: queue holds %d", bound, queue.len())
					}
				}
			}()
		}

		receivedFlags := make([][]bool, producerCount)
		for producer := range receivedFlags {
			receivedFlags[producer] = make([]bool, messageCount)
		}
		var receivedLock sync.Mutex
		var consumers sync.WaitGroup
		for consumer := range consumerCount {
			consumers.Add(1)
			go func() {
				defer consumers.Done()
				last := make([]int, producerCount)
				for producer := range last {
					last[producer] = -1
				}
				take := func(message []byte) {
					producer := int(binary.BigEndian.Uint64(message))
					i := int(binary.BigEndian.Uint64(message[8:]))
					if i <= last[producer] {
						t.Errorf("bound %d: consumer %d got producer %d's %d after %d", bound, consumer, producer, i, last[producer])
					}
					last[producer] = i
					func() {
						receivedLock.Lock()
						defer receivedLock.Unlock()
						if receivedFlags[producer][i] {
							t.Errorf("bound %d: producer %d's %d received twice", bound, producer, i)
						}
						receivedFlags[producer][i] = true
					}()
				}
				var batch [][]byte
				for round := 0; ; round += 1 {
					switch (consumer + round) % 4 {
					case 0:
						message, result := queue.receiveWait(nil)
						if result == messageQueueDrained {
							return
						}
						take(message)
					case 1:
						message, ok := <-queue.head()
						queue.received()
						if !ok {
							return
						}
						take(message)
					case 2:
						message, result := queue.tryReceive()
						switch result {
						case messageQueueDrained:
							return
						case messageQueueReceived:
							take(message)
						default:
							runtime.Gosched()
						}
					default:
						var drained bool
						batch, _, drained = queue.tryReceiveBatch(batch[:0], 0, 5, 1<<20, nil)
						for _, message := range batch {
							take(message)
						}
						clear(batch)
						if drained {
							return
						}
						if len(batch) == 0 {
							runtime.Gosched()
						}
					}
				}
			}()
		}

		producers.Wait()
		queue.close()
		consumers.Wait()
		cancel()
		for producer := range producerCount {
			for i := range messageCount {
				if sentFlags[producer][i] != receivedFlags[producer][i] {
					t.Fatalf("bound %d: producer %d message %d sent=%t received=%t", bound, producer, i, sentFlags[producer][i], receivedFlags[producer][i])
				}
			}
		}
		if bound < queue.ringCapacityForTest() {
			t.Fatalf("bound %d: ring grew to %d slots", bound, queue.ringCapacityForTest())
		}
	}
}

// One message through and back on one goroutine, against a channel of the
// same bound: the per-message cost when a link keeps up.
func BenchmarkMessageQueueSendReceive(b *testing.B) {
	queue := newMessageQueue(4096)
	message := newMessageQueueTestMessage(0)
	b.ReportAllocs()
	for b.Loop() {
		queue.trySend(message)
		queue.tryReceive()
	}
}

func BenchmarkChannelSendReceive(b *testing.B) {
	channel := make(chan []byte, 4096)
	message := newMessageQueueTestMessage(0)
	b.ReportAllocs()
	for b.Loop() {
		select {
		case channel <- message:
		default:
		}
		select {
		case <-channel:
		default:
		}
	}
}

// A producer goroutine and a consumer that waits for each message: the cost
// when a link is idle between messages.
func BenchmarkMessageQueueHandoff(b *testing.B) {
	queue := newMessageQueue(4096)
	message := newMessageQueueTestMessage(0)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			if _, result := queue.receiveWait(nil); result != messageQueueReceived {
				return
			}
		}
	}()
	b.ReportAllocs()
	for b.Loop() {
		queue.sendWait(message, nil, nil, nil)
	}
	queue.close()
	<-done
}

func BenchmarkChannelHandoff(b *testing.B) {
	channel := make(chan []byte, 4096)
	message := newMessageQueueTestMessage(0)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for range channel {
		}
	}()
	b.ReportAllocs()
	for b.Loop() {
		channel <- message
	}
	close(channel)
	<-done
}

// Bursts of 64 into a writer that gathers batches of up to 256, as the
// exchange socket writer does: the cost when a link is busy.
func BenchmarkMessageQueueBurstGather(b *testing.B) {
	queue := newMessageQueue(4096)
	message := newMessageQueueTestMessage(0)
	batch := make([][]byte, 0, 256)
	b.ReportAllocs()
	for b.Loop() {
		for range 64 {
			queue.trySend(message)
		}
		first, _ := queue.tryReceive()
		batch, _, _ = queue.tryReceiveBatch(append(batch[:0], first), 8, 256, 256*1024, nil)
		clear(batch)
	}
}

func BenchmarkChannelBurstGather(b *testing.B) {
	channel := make(chan []byte, 4096)
	message := newMessageQueueTestMessage(0)
	batch := make([][]byte, 0, 256)
	b.ReportAllocs()
	for b.Loop() {
		for range 64 {
			channel <- message
		}
		first := <-channel
		batch, _, _ = receiveReadyChannelBatch(channel, append(batch[:0], first), 8, 256, 256*1024, nil)
		clear(batch)
	}
}

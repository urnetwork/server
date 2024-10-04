package delayqueue_test

import (
	"context"
	"testing"
	"time"

	"bringyor.com/measure-throughput/delayqueue"
)

func TestDelayQueue(t *testing.T) {
	dq := delayqueue.New(context.Background(), 200)
	startTimer := time.Now()
	ch := make(chan bool)
	dq.GoDelayed(time.Second, func() {
		close(ch)
	})

	<-ch
	if time.Since(startTimer) < time.Second {
		t.Fatal("delay queue did not wait for the correct amount of time")
	}

}

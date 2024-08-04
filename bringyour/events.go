package bringyour

import (
	"context"
	"os"
	"os/signal"
	"syscall"
	"time"
)

type Event struct {
	Ctx    context.Context
	Cancel context.CancelFunc
}

func NewEvent() *Event {
	return NewEventWithContext(context.Background())
}

func NewEventWithContext(ctx context.Context) *Event {
	cancelCtx, cancel := context.WithCancel(ctx)
	return &Event{
		Ctx:    cancelCtx,
		Cancel: cancel,
	}
}

func NewEventWithCancelContext(cancelCtx context.Context, cancel context.CancelFunc) *Event {
	return &Event{
		Ctx:    cancelCtx,
		Cancel: cancel,
	}
}

func (self *Event) Set() {
	self.Cancel()
}

func (self *Event) IsSet() bool {
	select {
	case <-self.Ctx.Done():
		return true
	default:
		return false
	}
}

func (self *Event) WaitForSet(timeout time.Duration) bool {
	select {
	case <-self.Ctx.Done():
		return true
	case <-time.After(timeout):
		return false
	}
}

func (self *Event) SetOnSignals(signalValues ...syscall.Signal) func() {
	stopSignal := make(chan os.Signal, len(signalValues))
	for _, signalValue := range signalValues {
		signal.Notify(stopSignal, signalValue)
	}
	go func() {
		for {
			select {
			case _, ok := <-stopSignal:
				if !ok {
					return
				}
				self.Set()
			}
		}
	}()
	return func() {
		signal.Stop(stopSignal)
		close(stopSignal)
	}
}

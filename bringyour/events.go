package bringyour

import (
    "time"
    "os"
    "os/signal"
    "syscall"
    "context"
)



type Event struct {
    ctx context.Context
    cancel context.CancelFunc
}

func NewEvent() *Event {
    return NewEventWithContext(context.Background())
}

func NewEventWithContext(ctx context.Context) *Event {
    cancelCtx, cancel := context.WithCancel(ctx)
    return &Event{
        ctx: cancelCtx,
        cancel: cancel,
    }
}

func (self *Event) Set() {
    self.cancel()
}

func (self *Event) IsSet() bool {
    select {
    case <- self.ctx.Done():
        return true
    default:
        return false
    }
}

func (self *Event) WaitForSet(timeout time.Duration) bool {
    select {
    case <- self.ctx.Done():
        return true
    case <- time.After(timeout):
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
            case _, ok := <- stopSignal:
                if !ok {
                    return
                }
                self.Set()
            }
        }
    }()
    return func(){
        signal.Stop(stopSignal)
        close(stopSignal)
    }
}

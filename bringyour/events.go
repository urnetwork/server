package bringyour

import (
    "time"
    "os"
    "os/signal"
    "syscall"
)



type Event struct {
    set chan bool
}

func NewEvent() *Event {
    return &Event{
        set: make(chan bool, 0),
    }
}

func (self *Event) Set() {
    close(self.set)
}

func (self *Event) IsSet() bool {
    select {
    case <- self.set:
        return true
    default:
        return false
    }
}

func (self *Event) WaitForSet(timeout time.Duration) bool {
    select {
    case <- self.set:
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

package client


import (
// 	"runtime"
// 	"time"
// 	"sync"
// 	"context"

// 	"golang.org/x/mobile/gl"
)


// fixme
// connects to "connect" endpoint
// supports core operations: subscribe, etc
// on connect, create a new "core connection to an endpoint" that wraps a connect channel
// the "core connection" uses the connect channel and protocol to negotiate the peer to peer with edge
type BringYourClient struct {
}

func NewBringYourClient() *BringYourClient {
	return &BringYourClient{}
}



/*
type Event struct {
}


type ClientKernel struct {
}

func NewClientKernel() *ClientKernel {
	return nil
}


// returns a delay to process the next event
func (self *ClientKernel) Next(events []Event) (nativeEvents []Event, timeout int64) {


	// go thread that pings a url five times in a loop and sets the data in a channel



	return []Event{}, 0
}

func (self *ClientKernel) SetEventCallback(callback *EventCallback) {
	// todo
}


type EventCallback interface {
	Update(events []Event)
}

*/



// type StatusView struct {
// 	glView
// }



package client


import (
	"log"
	"fmt"
// 	"runtime"
// 	"time"
// 	"sync"
	"context"

// 	"golang.org/x/mobile/gl"

	"bringyour.com/connect"
	"bringyour.com/protocol"
)


var logger = log.Default()

func Logger() *log.Logger {
	return logger
}

func LogFn(tag string) func(string, ...any) {
	return func(format string, a ...any) {
		m := fmt.Sprintf(format, a...)
		Logger().Printf("%s: %s\n", tag, m)
	}
}




type SendPacket interface {
    // SendPacketFunction
    Send(packet []byte)
}


// fixme
// connects to "connect" endpoint
// supports core operations: subscribe, etc
// on connect, create a new "core connection to an endpoint" that wraps a connect channel
// the "core connection" uses the connect channel and protocol to negotiate the peer to peer with edge
type BringYourClient struct {
	ctx context.Context
	cancel context.CancelFunc
	remoteUserNat *connect.RemoteUserNat
}

func NewBringYourClient() *BringYourClient {
	cancelCtx, cancel := context.WithCancel(context.Background())

	remoteUserNat := connect.NewRemoteUserNat(cancelCtx, 8)
	go remoteUserNat.Run()

	return &BringYourClient{
		ctx: cancelCtx,
		cancel: cancel,
		remoteUserNat: remoteUserNat,
	}
}

func (self *BringYourClient) LocalReceive(packet []byte, n int32) {
	packetCopy := make([]byte, n)
	copy(packetCopy, packet[0:n])
	self.remoteUserNat.Receive(connect.Path{}, protocol.ProvideMode_STREAM, packetCopy)
}

func (self *BringYourClient) AddLocalSendPacket(sendPacket SendPacket) *Sub {
	// fixme use a callback list of SendPacket and have self.send as the fund
	send := func(destination connect.Path, packet []byte) {
		sendPacket.Send(packet)
	}
	self.remoteUserNat.AddSendPacketCallback(send)
	return &Sub{
		unsubFn: func() {
			self.remoteUserNat.RemoveSendPacketCallback(send)
		},
	}
}

func (self *BringYourClient) Close() {
	self.remoteUserNat.Close()
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



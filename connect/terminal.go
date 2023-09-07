package main

import (
    "net/http"

    // "google.golang.org/protobuf/proto"

    // "bringyour.com/bringyour/model"
    // "bringyour.com/bringyour/router"
    // "bringyour.com/protocol"
)



// quit event
// host ports
// routes
// transits to host:port (tcp connection)


// terminal reads frames from websocket
// if command message, handle it, ack it
// if pack, either send it to a connected client, or forward to another terminal


type Terminal struct {

}


func NewTerminal() *Terminal {
    return &Terminal{}
}


func (self *Terminal) Connect(w http.ResponseWriter, r *http.Request) {
    // websocket
    // parse protobuf
    // read client id
    // listen on internal port
    // public listen host:port

    // FROM CLIENT RECEIVE, send message, lookup destination. if destination does not exist, reply no route
    // FROM TRANSIT RECEIVE, if send message, if destination is local, allow. otherwise reject

}


/*
book := &pb.AddressBook{}
if err := proto.Unmarshal(in, book); err != nil {
    log.Fatalln("Failed to parse address book:", err)
}
*/

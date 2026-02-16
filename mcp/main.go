package main

import (
	"flag"
	"fmt"
	"log"
)

var (
	host = flag.String("host", "0.0.0.0", "host to listen on")
	port = flag.Int("port", 8000, "port number to listen on")
)

func main() {
	flag.Parse()

	addr := fmt.Sprintf("%s:%d", *host, *port)
	log.Printf("Starting MCP server on %s", addr)

	if err := runServer(addr); err != nil {
		log.Fatalf("Server failed: %v", err)
	}
}

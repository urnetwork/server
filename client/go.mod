module bringyour.com/client

go 1.21.0

require (
	bringyour.com/connect v0.0.0
	bringyour.com/protocol v0.0.0
	golang.org/x/mobile v0.0.0-20230427221453-e8d11dd0ba41
)

require (
	github.com/golang-jwt/jwt/v5 v5.0.0 // indirect
	github.com/google/gopacket v1.1.19 // indirect
	github.com/gorilla/websocket v1.5.0 // indirect
	github.com/oklog/ulid/v2 v2.1.0 // indirect
	golang.org/x/exp v0.0.0-20230713183714-613f0c0eb8a1 // indirect
	google.golang.org/protobuf v1.31.0 // indirect
)

replace bringyour.com/connect v0.0.0 => ../../connect/connect

replace bringyour.com/protocol v0.0.0 => ../../connect/protocol/build/bringyour.com/protocol

module bringyour.com/service/connect

go 1.21.0

require (
	bringyour.com/bringyour v0.0.0
	bringyour.com/connect v0.0.0
	bringyour.com/protocol v0.0.0
	github.com/docopt/docopt-go v0.0.0-20180111231733-ee0de3bc6815
	github.com/gorilla/websocket v1.5.0
	golang.org/x/exp v0.0.0-20230713183714-613f0c0eb8a1
	google.golang.org/protobuf v1.31.0
)

require (
	github.com/aws/aws-sdk-go v1.44.331 // indirect
	github.com/cespare/xxhash/v2 v2.2.0 // indirect
	github.com/coreos/go-semver v0.3.1 // indirect
	github.com/dgryski/go-rendezvous v0.0.0-20200823014737-9f7001d12a5f // indirect
	github.com/go-jose/go-jose/v3 v3.0.0 // indirect
	github.com/golang-jwt/jwt/v5 v5.0.0 // indirect
	github.com/golang/protobuf v1.5.0 // indirect
	github.com/google/gopacket v1.1.19 // indirect
	github.com/jackc/pgerrcode v0.0.0-20220416144525-469b46aa5efa // indirect
	github.com/jackc/pgpassfile v1.0.0 // indirect
	github.com/jackc/pgservicefile v0.0.0-20221227161230-091c0ba34f0a // indirect
	github.com/jackc/pgx/v5 v5.3.1 // indirect
	github.com/jackc/puddle/v2 v2.2.0 // indirect
	github.com/jmespath/go-jmespath v0.4.0 // indirect
	github.com/mozillazg/go-unidecode v0.2.0 // indirect
	github.com/nyaruka/phonenumbers v1.1.6 // indirect
	github.com/oklog/ulid/v2 v2.1.0 // indirect
	github.com/redis/go-redis/v9 v9.0.3 // indirect
	golang.org/x/crypto v0.8.0 // indirect
	golang.org/x/sync v0.1.0 // indirect
	golang.org/x/sys v0.7.0 // indirect
	golang.org/x/text v0.9.0 // indirect
	gopkg.in/yaml.v3 v3.0.1 // indirect
)

replace bringyour.com/bringyour v0.0.0 => ../bringyour

replace bringyour.com/connect v0.0.0 => ../../connect/connect

replace bringyour.com/protocol v0.0.0 => ../../connect/protocol/build/bringyour.com/protocol
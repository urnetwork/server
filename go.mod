module github.com/urnetwork/server

go 1.24.0

require (
	github.com/aws/aws-sdk-go v1.55.6
	github.com/coreos/go-semver v0.3.1
	github.com/docopt/docopt-go v0.0.0-20180111231733-ee0de3bc6815
	github.com/go-jose/go-jose/v3 v3.0.4
	github.com/go-playground/assert/v2 v2.2.0
	github.com/golang-jwt/jwt/v5 v5.2.2
	github.com/golang/glog v1.2.4
	github.com/gorilla/websocket v1.5.3
	github.com/ipinfo/go/v2 v2.10.0
	github.com/jackc/pgerrcode v0.0.0-20240316143900-6e2875d9b438
	github.com/jackc/pgx/v5 v5.7.4
	github.com/mozillazg/go-unidecode v0.2.0
	github.com/nyaruka/phonenumbers v1.6.0
	github.com/oklog/ulid/v2 v2.1.0
	github.com/pquerna/cachecontrol v0.2.0
	github.com/prometheus/client_golang v1.22.0
	github.com/redis/go-redis/v9 v9.7.3
	github.com/samber/lo v1.49.1
	github.com/skip2/go-qrcode v0.0.0-20200617195104-da1b6568686e
	github.com/stripe/stripe-go/v76 v76.25.0
	github.com/twmb/murmur3 v1.1.8
	github.com/tyler-smith/go-bip39 v1.1.0
	github.com/urnetwork/connect v2025.4.3-58948734+incompatible
	golang.org/x/crypto v0.37.0
	golang.org/x/exp v0.0.0-20250408133849-7e4ce0ab07d0
	golang.org/x/net v0.39.0
	google.golang.org/protobuf v1.36.6
	gopkg.in/yaml.v3 v3.0.1
)

require (
	github.com/beorn7/perks v1.0.1 // indirect
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/dgryski/go-rendezvous v0.0.0-20200823014737-9f7001d12a5f // indirect
	github.com/google/gopacket v1.1.19 // indirect
	github.com/jackc/pgpassfile v1.0.0 // indirect
	github.com/jackc/pgservicefile v0.0.0-20240606120523-5a60cdf6a761 // indirect
	github.com/jackc/puddle/v2 v2.2.2 // indirect
	github.com/jmespath/go-jmespath v0.4.0 // indirect
	github.com/munnerz/goautoneg v0.0.0-20191010083416-a7dc8b61c822 // indirect
	github.com/patrickmn/go-cache v2.1.0+incompatible // indirect
	github.com/prometheus/client_model v0.6.1 // indirect
	github.com/prometheus/common v0.63.0 // indirect
	github.com/prometheus/procfs v0.16.0 // indirect
	golang.org/x/sync v0.13.0 // indirect
	golang.org/x/sys v0.32.0 // indirect
	golang.org/x/text v0.24.0 // indirect
	src.agwa.name/tlshacks v0.0.0-20231008131857-90d701ba3225 // indirect
)

replace github.com/urnetwork/connect => ../connect

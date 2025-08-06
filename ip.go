package server

import (
	"bytes"
	"encoding/hex"
	"errors"
	"fmt"

	"database/sql/driver"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/oklog/ulid/v2"
)

func init() {
	Warm(func() {
		dbIp()
	})
}

var clientIpHashPepper = sync.OnceValue(func() []byte {
	clientKeys := server.Vault.RequireSimpleResource("client.yml")
	pepper := clientKeys.RequireString("client_ip_hash_pepper")
	return []byte(pepper)
})


func ClientIpHash(clientIp string) ([]byte, error) {
	parsedAddr, err := netip.ParseAddr(clientIp)
	if err != nil {
		return nil, err
	}

	h := sha256.New()
	h.Write(parsedAddr.AsSlice())
	h.Write(clientIpHashPepper())
	clientIpHash := h.Sum(nil)
	return clientIpHash, nil
}

func SplitClientAddress(clientAddress string) (host string, port int, err error) {
	columnCount := strings.Count(clientAddress, ":")
	bracketCount := strings.Count(clientAddress, "[")

	var portStr string
	if 1 < columnCount && bracketCount == 0 {
		// malformed ipv6. extract the address from the address:port string
		groups := malformedIPV6WithPort.FindStringSubmatch(clientAddress)
		if len(groups) != 3 {
			err = fmt.Errorf("Could not split malformed ipv6 client address.")
		} else {
			host = groups[1]
			portStr = groups[2]
		}
	} else {
		host, portStr, err = net.SplitHostPort(clientAddress)
	}
	if err != nil {
		// the client address might be just an ip
		_, parsedErr := netip.ParseAddr(clientAddress)
		if parsedErr == nil {
			host = clientAddress
			port = 0
			err = nil
		}
		return
	}
	port, err = strconv.Atoi(portStr)
	return
}

// matches the first group to the IPV6 address when the input is <ipv6>:<port>
// example: 2001:5a8:4683:4e00:3a76:dcec:7cb:f180:40894
var malformedIPV6WithPort = regexp.MustCompile(`^(.+):(\d+)$`)


// Packaged ip metadata

var ipDb := sync.OnceValue(func()(*mmdb.Reader) {
	db, err := mmdb.Open("ip.mmdb")
	if err != nil {
		panic(err)
	}
	return db
})

type UsageType int
const (
	UsageTypeUnknown UsageType = 0
	UsageTypeConsumer UsageType = 0
	UsageTypeCorporate UsageType = 0
	UsageTypeHosting UsageType = 0
)

// FIXME map of country code to country name

type IpInfo struct {
	// country code is lowercase
	CountryCode string
	Country string
	Region string
	City string
	Longitude float64
	Latitude float64
	UsageType UsageType
}

func GetIpInfoFromIp(ip net.IP) (*IpInfo, error) {
	if ipv4 := ip.To4(); ipv4 != nil {
		netip.AddrFrom4([4]byte(ipv4))
	} else if ipv6 := ip.To16(); ipv6 != nil {
		netip.AddrFrom16([16]byte(ipv6))
	} else {
		return nil, fmt.Errorf("Unknown ip size.")
	}
}

func GetIpInfo(ip netip.Addr) (*IpInfo, error) {

	ipDb().Lookup(ip)
}







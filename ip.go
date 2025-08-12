package server

import (
	// "bytes"
	// "encoding/hex"
	// "errors"
	"crypto/sha256"
	"fmt"
	"net"
	"net/netip"
	"regexp"
	"strconv"
	"strings"
	"sync"
	// "sync"

	// "database/sql/driver"

	// "github.com/jackc/pgx/v5/pgtype"
	// "github.com/oklog/ulid/v2"

	"github.com/golang/glog"

	mmdb "github.com/oschwald/maxminddb-golang/v2"
	"github.com/oschwald/maxminddb-golang/v2/mmdbdata"
)

func init() {
	Warm(func() {
		ipDb()
	})

	glog.Infof("[ip]ip info database type: %s\n", ipDb().Metadata.DatabaseType)
}

var clientIpHashPepper = sync.OnceValue(func() []byte {
	clientKeys := Vault.RequireSimpleResource("client.yml")
	pepper := clientKeys.RequireString("client_ip_hash_pepper")
	return []byte(pepper)
})

func ClientIpHash(clientIp string) ([]byte, error) {
	addr, err := netip.ParseAddr(clientIp)
	if err != nil {
		return nil, err
	}
	return ClientIpHashForAddr(addr), nil
}

func ClientIpHashForAddr(addr netip.Addr) []byte {
	h := sha256.New()
	h.Write(addr.AsSlice())
	h.Write(clientIpHashPepper())
	clientIpHash := h.Sum(nil)
	return clientIpHash
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

// Scrubbing

var errorIpv4 = regexp.MustCompile(`[0-9]+(?:\.[0-9]+){3,3}`)
var errorIpv4Port = regexp.MustCompile(`[0-9]+(?:\.[0-9]+){3,3}:[0-9]+`)
var errorIpv6 = regexp.MustCompile(`[0-9]+(?::[0-9]+){,15}(?:::[0-9]+)?`)
var errorIpv6Port = regexp.MustCompile(`[0-9]+(?::[0-9]+){,15}(?:::[0-9]+)?:[0-9]+`)

func ScrubIpPort(s string) string {
	s = errorIpv4Port.ReplaceAllString(s, `ipv4:port`)
	s = errorIpv4.ReplaceAllString(s, `ipv4`)
	s = errorIpv6Port.ReplaceAllString(s, `ipv6:port`)
	s = errorIpv6.ReplaceAllString(s, `ipv6`)
	return s
}

// Packaged ip metadata

var ipDb = sync.OnceValue(func() *mmdb.Reader {
	path, err := Config.ResourcePath("mmdb/ip.mmdb")
	if err != nil {
		panic(err)
	}
	db, err := mmdb.Open(path)
	if err != nil {
		panic(err)
	}
	return db
})

type UserType int

const (
	UserTypeUnknown  UserType = 0
	UserTypeConsumer UserType = 1
	UserTypeBusiness UserType = 2
	UserTypeHosting  UserType = 3
)

type IpInfo struct {
	// country code is lowercase
	CountryCode    string
	Country        string
	Region         string
	Regions        []string
	City           string
	Longitude      float64
	Latitude       float64
	UserType       UserType
	Organization   string
	ASN            uint32
	ASOrganization string
}

func (self *IpInfo) UnmarshalMaxMindDB(d *mmdbdata.Decoder) error {
	// the following schema are supported:
	// - `DBIP-Location-ISP (compat=Enterprise)`
	//   https://db-ip.com/db/format/ip-to-location-isp/mmdb.html
	mapIter, _, err := d.ReadMap()
	if err != nil {
		return err
	}
	for key, err := range mapIter {
		if err != nil {
			return err
		}
		// kind, err := d.PeekKind()
		// if err != nil {
		// 	return err
		// }
		// glog.Infof("[ip]decode key \"%s\" = %s\n", key, kind)
		switch string(key) {
		case "country":
			// readMap
			// iso_code
			// names [en]
			countryIter, _, err := d.ReadMap()
			if err != nil {
				return err
			}
			for countryKey, err := range countryIter {
				if err != nil {
					return err
				}
				switch string(countryKey) {
				case "iso_code":
					countryCode, err := d.ReadString()
					if err != nil {
						return err
					}
					self.CountryCode = strings.ToLower(countryCode)
				case "names":
					namesIter, _, err := d.ReadMap()
					if err != nil {
						return err
					}
					for namesKey, err := range namesIter {
						if err != nil {
							return err
						}
						switch string(namesKey) {
						case "en":
							self.Country, err = d.ReadString()
							if err != nil {
								return err
							}
						default:
							if err := d.SkipValue(); err != nil {
								return err
							}
						}
					}
				default:
					if err := d.SkipValue(); err != nil {
						return err
					}
				}
			}

		case "subdivisions":
			// map
			// names [en]

			subdivisionsIter, _, err := d.ReadSlice()
			if err != nil {
				return err
			}
			for err := range subdivisionsIter {
				if err != nil {
					return err
				}

				subdivisionIter, _, err := d.ReadMap()
				if err != nil {
					return err
				}
				for subdivisionKey, err := range subdivisionIter {
					if err != nil {
						return err
					}
					switch string(subdivisionKey) {
					case "names":
						namesIter, _, err := d.ReadMap()
						if err != nil {
							return err
						}
						for namesKey, err := range namesIter {
							if err != nil {
								return err
							}
							switch string(namesKey) {
							case "en":
								region, err := d.ReadString()
								if err != nil {
									return err
								}
								self.Regions = append(self.Regions, region)
							default:
								if err := d.SkipValue(); err != nil {
									return err
								}
							}
						}
					default:
						if err := d.SkipValue(); err != nil {
							return err
						}
					}
				}
			}

			if 0 < len(self.Regions) {
				self.Region = self.Regions[0]
			}

		case "city":
			// map
			// names [en]

			cityIter, _, err := d.ReadMap()
			if err != nil {
				return err
			}
			for cityKey, err := range cityIter {
				if err != nil {
					return err
				}
				switch string(cityKey) {
				case "names":
					namesIter, _, err := d.ReadMap()
					if err != nil {
						return err
					}
					for namesKey, err := range namesIter {
						if err != nil {
							return err
						}
						switch string(namesKey) {
						case "en":
							self.City, err = d.ReadString()
							if err != nil {
								return err
							}
						default:
							if err := d.SkipValue(); err != nil {
								return err
							}
						}
					}
				default:
					if err := d.SkipValue(); err != nil {
						return err
					}
				}
			}

		case "location":
			// readMap
			// latitude
			// longitude
			locationIter, _, err := d.ReadMap()
			if err != nil {
				return err
			}
			for locationKey, err := range locationIter {
				if err != nil {
					return err
				}
				switch string(locationKey) {
				case "latitude":
					self.Latitude, err = d.ReadFloat64()
					if err != nil {
						return err
					}
				case "longitude":
					self.Longitude, err = d.ReadFloat64()
					if err != nil {
						return err
					}
				default:
					if err := d.SkipValue(); err != nil {
						return err
					}
				}
			}

		case "traits":
			// map
			// user_type [business,residential,cellular,hosting]

			traitsIter, _, err := d.ReadMap()
			if err != nil {
				return err
			}
			for traitsKey, err := range traitsIter {
				if err != nil {
					return err
				}
				switch string(traitsKey) {
				case "user_type":
					userType, err := d.ReadString()
					if err != nil {
						return err
					}
					switch userType {
					case "residential":
					case "cellular":
						self.UserType = UserTypeConsumer
					case "business":
						self.UserType = UserTypeBusiness
					case "hosting":
						self.UserType = UserTypeHosting
					default:
						self.UserType = UserTypeUnknown
					}
				case "organization":
					self.Organization, err = d.ReadString()
					if err != nil {
						return err
					}
				case "autonomous_system_number":
					self.ASN, err = d.ReadUint32()
					if err != nil {
						return err
					}
				case "autonomous_system_organization":
					self.ASOrganization, err = d.ReadString()
					if err != nil {
						return err
					}
				default:
					if err := d.SkipValue(); err != nil {
						return err
					}
				}
			}

		default:
			// glog.Infof("[ip]decode skip key \"%s\"\n", key)
			if err := d.SkipValue(); err != nil {
				return err
			}
		}
	}
	return nil
}

func GetIpInfoFromIp(ip net.IP) (*IpInfo, error) {
	if ipv4 := ip.To4(); ipv4 != nil {
		addr := netip.AddrFrom4([4]byte(ipv4))
		return GetIpInfo(addr)
	} else if ipv6 := ip.To16(); ipv6 != nil {
		addr := netip.AddrFrom16([16]byte(ipv6))
		return GetIpInfo(addr)
	} else {
		return nil, fmt.Errorf("Unknown ip size.")
	}
}

func GetIpInfo(addr netip.Addr) (*IpInfo, error) {
	r := ipDb().Lookup(addr)
	var ipInfo IpInfo
	err := r.Decode(&ipInfo)
	if err != nil {
		return nil, err
	}
	return &ipInfo, nil
}

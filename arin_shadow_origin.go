package server

import (
	"slices"
	"strconv"
	"strings"
)

const arinShadowOriginLimit = 4096
const arinShadowOriginASNLimit = 8

// Public routing origins identify networks that need operator/use research.
// An ASN or an unknown origin-use state never establishes subscriber access.
// Multiple origins remain a set; assigning their connections to one arbitrary
// operator would hide the very conflicts that the classifier must preserve.
type ArinShadowOrigin struct {
	ASNs     []uint32 `json:"asns,omitempty"`
	UseState string   `json:"use_state,omitempty"`
}

type ArinShadowOriginCount struct {
	ArinShadowOrigin
	Connections int64 `json:"connections"`
	Subscriber  int64 `json:"subscriber"`
	Excluded    int64 `json:"excluded"`
	Unknown     int64 `json:"unknown"`
	Ambiguous   int64 `json:"ambiguous"`
	Risk        int64 `json:"risk"`
	ProxyRisk   int64 `json:"proxy_risk"`
}

func validArinShadowOrigin(origin ArinShadowOrigin) bool {
	if len(origin.ASNs) == 0 {
		return origin.UseState == ""
	}
	if len(origin.ASNs) > arinShadowOriginASNLimit || !slices.Contains([]string{"subscriber", "excluded", "unknown", "ambiguous"}, origin.UseState) {
		return false
	}
	var previous uint32
	for _, asn := range origin.ASNs {
		if asn <= previous {
			return false
		}
		previous = asn
	}
	return true
}

func newArinShadowOrigin(asns []uint32, useState string) ArinShadowOrigin {
	origin := ArinShadowOrigin{ASNs: asns, UseState: useState}
	if !validArinShadowOrigin(origin) {
		return ArinShadowOrigin{}
	}
	origin.ASNs = slices.Clone(asns)
	return origin
}

func arinShadowOriginKey(origin ArinShadowOrigin) string {
	var key strings.Builder
	key.WriteString(origin.UseState)
	for _, asn := range origin.ASNs {
		key.WriteByte(',')
		key.WriteString(strconv.FormatUint(uint64(asn), 10))
	}
	return key.String()
}

func (s *ArinShadowCaptureStream) addOrigin(f arinShadowFacts) {
	if len(f.origin.ASNs) == 0 {
		s.report.OriginUnattributedConnections++
		return
	}
	key := arinShadowOriginKey(f.origin)
	row := s.origins[key]
	if row == nil {
		if len(s.origins) >= arinShadowOriginLimit {
			s.report.OriginOverflowConnections++
			return
		}
		row = &ArinShadowOriginCount{ArinShadowOrigin: newArinShadowOrigin(f.origin.ASNs, f.origin.UseState)}
		s.origins[key] = row
	}
	row.Connections++
	switch f.state {
	case "subscriber":
		row.Subscriber++
	case "excluded":
		row.Excluded++
	case "ambiguous":
		row.Ambiguous++
	default:
		row.Unknown++
	}
	if f.risk {
		row.Risk++
	}
	if f.proxyRisk {
		row.ProxyRisk++
	}
}

func (s *ArinShadowCaptureStream) finishOrigins() {
	s.report.OriginCounts = make([]ArinShadowOriginCount, 0, len(s.origins))
	for _, row := range s.origins {
		s.report.OriginCounts = append(s.report.OriginCounts, *row)
	}
	slices.SortFunc(s.report.OriginCounts, func(a, b ArinShadowOriginCount) int {
		if a.Connections > b.Connections {
			return -1
		}
		if a.Connections < b.Connections {
			return 1
		}
		if order := slices.Compare(a.ASNs, b.ASNs); order != 0 {
			return order
		}
		return strings.Compare(a.UseState, b.UseState)
	})
}

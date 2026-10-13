package server

import (
	"crypto/sha256"
	"encoding/hex"
	"slices"
	"strings"
)

const arinShadowRegistrationLimit = 4096

// Public registration identifiers describe the candidate's source record,
// not a provider address or an ASN-wide subscriber allow. Rule names are
// hashed because a rule name can itself contain a literal prefix/address.
type ArinShadowRegistration struct {
	OrgHandle               string `json:"org_handle,omitempty"`
	NetHandle               string `json:"net_handle,omitempty"`
	ClassificationOrgHandle string `json:"classification_org_handle,omitempty"`
	RuleSHA256              string `json:"rule_sha256,omitempty"`
}

type ArinShadowRegistrationCount struct {
	ArinShadowRegistration
	Connections int64 `json:"connections"`
	Subscriber  int64 `json:"subscriber"`
	Excluded    int64 `json:"excluded"`
	Unknown     int64 `json:"unknown"`
	Ambiguous   int64 `json:"ambiguous"`
	Risk        int64 `json:"risk"`
	ProxyRisk   int64 `json:"proxy_risk"`
}

func validArinShadowHandle(handle string) bool {
	if len(handle) < 1 || len(handle) > 64 {
		return false
	}
	for _, c := range handle {
		if !(c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-') {
			return false
		}
	}
	return true
}

func newArinShadowRegistration(org, network, classificationOrg, rule string) ArinShadowRegistration {
	if !validArinShadowHandle(org) {
		return ArinShadowRegistration{}
	}
	r := ArinShadowRegistration{OrgHandle: org}
	if validArinShadowHandle(network) {
		r.NetHandle = network
	}
	if validArinShadowHandle(classificationOrg) {
		r.ClassificationOrgHandle = classificationOrg
	}
	if rule != "" {
		hash := sha256.Sum256([]byte(rule))
		r.RuleSHA256 = hex.EncodeToString(hash[:])
	}
	return r
}

func validArinShadowRegistration(r ArinShadowRegistration) bool {
	if r == (ArinShadowRegistration{}) {
		return true
	}
	return validArinShadowHandle(r.OrgHandle) &&
		(r.NetHandle == "" || validArinShadowHandle(r.NetHandle)) &&
		(r.ClassificationOrgHandle == "" || validArinShadowHandle(r.ClassificationOrgHandle)) &&
		(r.RuleSHA256 == "" || len(r.RuleSHA256) == 64 && arinShadowHex(r.RuleSHA256, 32))
}

func (s *ArinShadowCaptureStream) addRegistration(f arinShadowFacts) {
	if f.registration.OrgHandle == "" {
		s.report.RegistrationUnattributedConnections++
		return
	}
	row := s.registrations[f.registration]
	if row == nil {
		if len(s.registrations) >= arinShadowRegistrationLimit {
			s.report.RegistrationOverflowConnections++
			return
		}
		row = &ArinShadowRegistrationCount{ArinShadowRegistration: f.registration}
		s.registrations[f.registration] = row
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

func (s *ArinShadowCaptureStream) finishRegistrations() {
	s.report.RegistrationCounts = make([]ArinShadowRegistrationCount, 0, len(s.registrations))
	for _, row := range s.registrations {
		s.report.RegistrationCounts = append(s.report.RegistrationCounts, *row)
	}
	slices.SortFunc(s.report.RegistrationCounts, func(a, b ArinShadowRegistrationCount) int {
		if a.Connections > b.Connections {
			return -1
		}
		if a.Connections < b.Connections {
			return 1
		}
		return strings.Compare(a.OrgHandle+"\x00"+a.NetHandle+"\x00"+a.ClassificationOrgHandle+"\x00"+a.RuleSHA256, b.OrgHandle+"\x00"+b.NetHandle+"\x00"+b.ClassificationOrgHandle+"\x00"+b.RuleSHA256)
	})
}

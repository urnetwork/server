package model

import (
	"testing"
	"time"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
)

// Tests of the drift model's calibration (lag.dev/DRIFTTEST.md V3, V4, K2,
// Q12): its defaults, its relay thresholds and the network types they are
// kept by.

// The defaults are the design's: κ 0.25, σ₀ 1 ms and a relay threshold of
// 80 ms for every continent and network type. Pure.
func TestLagDriftCalibrationDefaults(t *testing.T) {
	calibration := DefaultLagDriftCalibration()
	connect.AssertEqual(t, calibration.Kappa, 0.25)
	connect.AssertEqual(t, calibration.Sigma0Ms, 1.0)
	connect.AssertEqual(t, calibration.RelayThresholdMs, 80.0)
	connect.AssertEqual(t, calibration.Calibrated(), false)
	connect.AssertEqual(t, calibration.CalibrationId, (*server.Id)(nil))
	connect.AssertEqual(t, calibration.CalibratedAt, (*time.Time)(nil))
	for _, continentCode := range append([]string{""}, ContinentCodes...) {
		for _, networkType := range append([]string{""}, LagDriftNetworkTypes...) {
			connect.AssertEqual(t, calibration.RelayThresholdMsFor(continentCode, networkType), 80.0)
		}
	}
	connect.AssertEqual(t, calibration.RelaySuspected(80, "EU", LagDriftNetworkTypeFixed), false)
	connect.AssertEqual(t, calibration.RelaySuspected(80.5, "EU", LagDriftNetworkTypeFixed), true)
}

// A visitor's threshold is the most specific one kept: the continent's for
// the network type, every continent's for the type, the continent's for
// every type, and else the threshold over everything. Pure.
func TestLagDriftCalibrationRelayThresholdLookup(t *testing.T) {
	calibration := DefaultLagDriftCalibration()
	calibration.RelayThresholdMs = 60
	calibration.RelayThresholdsMs = map[string]map[string]float64{
		"": {
			"":                           60,
			LagDriftNetworkTypeFixed:     45,
			LagDriftNetworkTypeMobile:    110,
			LagDriftNetworkTypeSatellite: 0,
		},
		"EU": {
			"":                       50,
			LagDriftNetworkTypeFixed: 35,
		},
		"AF": {
			"": 95,
		},
	}
	// the continent's own for the type
	connect.AssertEqual(t, calibration.RelayThresholdMsFor("EU", LagDriftNetworkTypeFixed), 35.0)
	// every continent's for the type before the continent's for every type
	connect.AssertEqual(t, calibration.RelayThresholdMsFor("EU", LagDriftNetworkTypeMobile), 110.0)
	connect.AssertEqual(t, calibration.RelayThresholdMsFor("AF", LagDriftNetworkTypeFixed), 45.0)
	// the continent's for every type when its type has none anywhere; a
	// threshold of 0 is none
	connect.AssertEqual(t, calibration.RelayThresholdMsFor("AF", LagDriftNetworkTypeSatellite), 95.0)
	connect.AssertEqual(t, calibration.RelayThresholdMsFor("EU", LagDriftNetworkTypeUnknown), 50.0)
	// and else the threshold over everything
	connect.AssertEqual(t, calibration.RelayThresholdMsFor("OC", LagDriftNetworkTypeUnknown), 60.0)
	connect.AssertEqual(t, calibration.RelayThresholdMsFor("", ""), 60.0)
	connect.AssertEqual(t, calibration.RelaySuspected(36, "EU", LagDriftNetworkTypeFixed), true)
	connect.AssertEqual(t, calibration.RelaySuspected(36, "EU", LagDriftNetworkTypeMobile), false)
}

// A network is typed by its number where a curated list knows it, else by
// its name, satellite before mobile, else as fixed; without a number it is
// unknown. Pure.
func TestLagDriftNetworkType(t *testing.T) {
	asn := func(number int64) *int64 {
		return &number
	}
	connect.AssertEqual(t, LagDriftNetworkType(nil, ""), LagDriftNetworkTypeUnknown)
	connect.AssertEqual(t, LagDriftNetworkType(nil, "T-Mobile USA, Inc."), LagDriftNetworkTypeUnknown)
	connect.AssertEqual(t, LagDriftNetworkType(asn(14593), "SPACEX-STARLINK"), LagDriftNetworkTypeSatellite)
	connect.AssertEqual(t, LagDriftNetworkType(asn(64500), "Example Satellite Broadband"), LagDriftNetworkTypeSatellite)
	connect.AssertEqual(t, LagDriftNetworkType(asn(21928), "T-Mobile USA, Inc."), LagDriftNetworkTypeMobile)
	connect.AssertEqual(t, LagDriftNetworkType(asn(22394), "Cellco Partnership DBA Verizon Wireless"), LagDriftNetworkTypeMobile)
	connect.AssertEqual(t, LagDriftNetworkType(asn(55836), "Reliance Jio Infocomm Limited"), LagDriftNetworkTypeMobile)
	connect.AssertEqual(t, LagDriftNetworkType(asn(16135), "Turkcell Iletisim Hizmetleri A.S."), LagDriftNetworkTypeMobile)
	connect.AssertEqual(t, LagDriftNetworkType(asn(7922), "Comcast Cable Communications, LLC"), LagDriftNetworkTypeFixed)
	connect.AssertEqual(t, LagDriftNetworkType(asn(3320), "Deutsche Telekom AG"), LagDriftNetworkTypeFixed)
	connect.AssertEqual(t, LagDriftNetworkType(asn(64501), ""), LagDriftNetworkTypeFixed)
}

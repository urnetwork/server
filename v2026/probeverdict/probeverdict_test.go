package probeverdict

import (
	"reflect"
	"testing"
	"time"
)

func TestEvaluateNoConsensusIsUnverified(t *testing.T) {
	v := Evaluate(Input{CountryConfident: false})
	if v.State != "unverified" || v.Reason != "no_consensus" {
		t.Errorf("got %+v, want unverified/no_consensus", v)
	}
}

func TestEvaluateCountryFlipFlopIsSuspect(t *testing.T) {
	now := time.Now()
	v := Evaluate(Input{
		CountryConfident:    true,
		CountryCode:         "de",
		PreviousCountryCode: "es",
		PreviousObservedAt:  now.Add(-2 * time.Hour),
		Now:                 now,
	})
	if v.State != "suspect" || v.Reason != "unstable" {
		t.Errorf("got %+v, want suspect/unstable", v)
	}
}

func TestEvaluateCountryChangeOutsideWindowIsVerified(t *testing.T) {
	// a country change is only "unstable" within the 24h window -- after it,
	// a changed country is a legitimate correction, not a flip-flop
	now := time.Now()
	v := Evaluate(Input{
		CountryConfident:    true,
		CountryCode:         "de",
		PreviousCountryCode: "es",
		PreviousObservedAt:  now.Add(-25 * time.Hour),
		Now:                 now,
	})
	if v.State != "verified" {
		t.Errorf("got %+v, want verified (change is outside the 24h window)", v)
	}
}

func TestEvaluateMmdbDivergenceAloneIsNotSuspect(t *testing.T) {
	// this test asserts the single most important safety property in the
	// package: a country that differs from what mmdb would have said is NOT
	// an input to Evaluate at all -- there is no field for it on Input, so a
	// clean first-time probe always verifies regardless of what mmdb would
	// have said about the same connection.
	v := Evaluate(Input{
		CountryConfident: true,
		CountryCode:      "es",
	})
	if v.State != "verified" {
		t.Errorf("a clean probe with no prior history must verify regardless of what mmdb would have said, got %+v", v)
	}
}

// TestInputHasNoMmdbRttOrCoordinateFields locks the two structural omissions
// this package depends on for correctness. Neither can be asserted
// behaviourally -- they are the absence of inputs, so the only way to test
// them is to pin the field set itself.
//
//  1. No mmdb-derived country. A probed country diverging from what the free
//     mmdb would have said is the entire point of this project, and with no
//     field for it a caller cannot get the rule wrong.
//  2. No RTT or coordinate fields. An RTT-distance floor was designed and
//     deliberately dropped (see the package doc comment and the spec's "The
//     RTT floor was designed, then dropped"): it needs a fixed reference
//     point this system has no single answer for, and a wrong reference point
//     does not fail safe -- it can flag an honest provider as suspect.
//
// If this test fails because a field was added, that is the point: resolve
// the reference-point problem in the spec first.
func TestInputHasNoMmdbRttOrCoordinateFields(t *testing.T) {
	allowed := map[string]bool{
		"CountryConfident":    true,
		"CountryCode":         true,
		"PreviousCountryCode": true,
		"PreviousObservedAt":  true,
		"Now":                 true,
	}

	inputType := reflect.TypeOf(Input{})
	for i := range inputType.NumField() {
		name := inputType.Field(i).Name
		if !allowed[name] {
			t.Errorf("Input has an unexpected field %q: mmdb-country, RTT and coordinate "+
				"fields are deliberately absent from Input; see the package doc comment", name)
		}
	}
	if inputType.NumField() != len(allowed) {
		t.Errorf("Input has %d fields, want exactly %d", inputType.NumField(), len(allowed))
	}
}

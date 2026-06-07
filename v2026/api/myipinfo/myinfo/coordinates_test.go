package myinfo_test

import (
	"math"
	"testing"

	"github.com/urnetwork/server/v2026/api/myipinfo/myinfo"

	"github.com/go-playground/assert/v2"
)

func TestParseCoordinates(t *testing.T) {
	c, err := myinfo.ParseCoordinates("45.8399,-119.7006")
	assert.Equal(t, nil, err)

	if d := math.Abs(45.8399 - c.Latitude); 1e-8 < d {
		t.Fatalf("%f<>%f", 45.8399, c.Latitude)
	}
	if d := math.Abs(-119.7006 - c.Longitude); 1e-8 < d {
		t.Fatalf("%f<>%f", -119.7006, c.Longitude)
	}
}

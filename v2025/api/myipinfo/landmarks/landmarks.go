package landmarks

import (
	_ "embed"
	"encoding/json"
	"fmt"

	"github.com/urnetwork/server/api/myipinfo/myinfo"
)

type Landmarks struct {
	Landmarks []Landmark `json:"landmarks"`
	RTTScale  float64    `json:"rtt_scale"`
	RTTAlpha  float64    `json:"rtt_alpha"`
}

type Landmark struct {
	Name        string             `json:"name"`
	Addr        string             `json:"addr"`
	WSURL       string             `json:"ws_url"`
	Coordinates myinfo.Coordinates `json:"coordinates"`
}

type AllIPVersions struct {
	V4 Landmarks `json:"v4"`
	V6 Landmarks `json:"v6"`
}

var CurrentLandmarks AllIPVersions

//go:embed landmarks.json
var landmarksJSON []byte

func init() {
	CurrentLandmarks = AllIPVersions{}
	err := json.Unmarshal(landmarksJSON, &CurrentLandmarks)
	if err != nil {
		panic(fmt.Errorf("failed to unmarshal landmarks: %w", err))
	}
}

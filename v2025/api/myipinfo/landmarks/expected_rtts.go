package landmarks

import (
	"slices"

	"github.com/samber/lo"

	"github.com/urnetwork/server/v2025/api/myipinfo/landmarks/kmeanspp"
	"github.com/urnetwork/server/v2025/api/myipinfo/myinfo"
)

type LandmarkAndRTT struct {
	Name        string    `json:"name"`
	IP          string    `json:"ip"`
	URL         string    `json:"url"`
	RTT         float64   `json:"rtt"`
	WSURL       string    `json:"ws_url"`
	Coordinates []float64 `json:"coordinates"`
}

func nearestPoint(points []myinfo.Coordinates, point myinfo.Coordinates) myinfo.Coordinates {
	minDistance := point.CalculateDistanceInLightSecondsOnEarthSurface(points[0])
	minIndex := 0
	for i, p := range points {
		distance := point.CalculateDistanceInLightSecondsOnEarthSurface(p)
		if distance < minDistance {
			minDistance = distance
			minIndex = i
		}
	}
	return points[minIndex]
}

func (l Landmarks) ExpectedRTTSVerbose(c myinfo.Coordinates, cnt int) []LandmarkAndRTT {

	type chosenLandmark struct {
		index    int
		distance float64
	}

	chosen := lo.Map(l.Landmarks, func(lm Landmark, i int) chosenLandmark {
		return chosenLandmark{i, c.CalculateDistanceInLightSecondsOnEarthSurface(lm.Coordinates)}
	})

	// Sort by distance
	slices.SortFunc(chosen, func(i, j chosenLandmark) int {
		if i.distance < j.distance {
			return -1
		}
		if i.distance > j.distance {
			return 1
		}
		return 0
	})

	candidates := lo.Filter(chosen, func(c chosenLandmark, _ int) bool {
		return c.distance > 0 && (c.distance/l.RTTScale+l.RTTAlpha) < 0.05
	})

	if len(candidates) < 3 {
		chosen = chosen[:3]
	} else {

		coordinates := lo.Map(candidates, func(c chosenLandmark, _ int) myinfo.Coordinates {
			return l.Landmarks[c.index].Coordinates
		})

		candidatesByCoordinates := lo.SliceToMap(candidates, func(c chosenLandmark) (myinfo.Coordinates, chosenLandmark) {
			return l.Landmarks[c.index].Coordinates, c
		})

		km := kmeanspp.KMeans(coordinates, 3, 100)

		chosen = []chosenLandmark{}

		for _, cluster := range km {
			first := nearestPoint(cluster.Points, c)
			chosen = append(chosen, candidatesByCoordinates[first])
		}
	}

	rtts := []LandmarkAndRTT{}
	for _, c := range chosen {

		rtts = append(
			rtts,
			LandmarkAndRTT{
				Name:        l.Landmarks[c.index].Name,
				IP:          l.Landmarks[c.index].Addr,
				WSURL:       l.Landmarks[c.index].WSURL,
				RTT:         c.distance/l.RTTScale + l.RTTAlpha,
				Coordinates: []float64{l.Landmarks[c.index].Coordinates.Longitude, l.Landmarks[c.index].Coordinates.Latitude},
			},
		)
	}

	return rtts
}

package kmeanspp

import (
	"math"
	"math/rand"

	"github.com/urnetwork/server/api/myipinfo/myinfo"
)

// Point represents a geographical point with longitude and latitude in degrees.
type Point = myinfo.Coordinates

// toCartesian converts a Point from spherical (lon, lat) to Cartesian (x, y, z) coordinates.
func toCartesian(p Point) (x, y, z float64) {
	latRad := degreesToRadians(p.Latitude)
	lonRad := degreesToRadians(p.Longitude)

	x = math.Cos(latRad) * math.Cos(lonRad)
	y = math.Cos(latRad) * math.Sin(lonRad)
	z = math.Sin(latRad)
	return
}

// cartesianToPoint converts Cartesian coordinates (x, y, z) back to a Point (lon, lat).
func cartesianToPoint(x, y, z float64) Point {
	hyp := math.Sqrt(x*x + y*y)
	latRad := math.Atan2(z, hyp)
	lonRad := math.Atan2(y, x)

	lat := radiansToDegrees(latRad)
	lon := radiansToDegrees(lonRad)
	return Point{Longitude: lon, Latitude: lat}
}

// degreesToRadians converts degrees to radians.
func degreesToRadians(deg float64) float64 {
	return deg * math.Pi / 180.0
}

// radiansToDegrees converts radians to degrees.
func radiansToDegrees(rad float64) float64 {
	return rad * 180.0 / math.Pi
}

// angularDistance computes the angular distance between two points on a sphere.
func angularDistance(p1, p2 Point) float64 {
	x1, y1, z1 := toCartesian(p1)
	x2, y2, z2 := toCartesian(p2)
	dot := x1*x2 + y1*y2 + z1*z2

	// Clamp dot product to [-1,1] to avoid numerical errors
	if dot > 1.0 {
		dot = 1.0
	} else if dot < -1.0 {
		dot = -1.0
	}
	angle := math.Acos(dot)
	return angle
}

// Cluster represents a cluster with a centroid and associated points.
type Cluster struct {
	Centroid Point
	Points   []Point
}

// kMeans performs k-means clustering on spherical coordinates.
func KMeans(points []Point, k int, maxIterations int) []Cluster {
	// Initialize centroids using k-means++ algorithm
	centroids := initializeCentroids(points, k)

	var clusters []Cluster
	for i := 0; i < maxIterations; i++ {
		// Assignment step
		clusters = assignPointsToClusters(points, centroids)

		// Update step
		newCentroids := updateCentroids(clusters)

		// Check for convergence
		converged := true
		for j := 0; j < k; j++ {
			if angularDistance(centroids[j], newCentroids[j]) > 1e-6 {
				converged = false
				break
			}
		}
		centroids = newCentroids

		if converged {
			break
		}
	}
	return clusters
}

// initializeCentroids selects initial centroids using the k-means++ algorithm.
func initializeCentroids(points []Point, k int) []Point {
	n := len(points)
	centroids := make([]Point, 0, k)

	// Step 1: Randomly select the first centroid from the data points
	index := rand.Intn(n)
	centroids = append(centroids, points[index])

	// Create a slice to store the distances to the nearest centroid
	distances := make([]float64, n)

	// Loop to select the rest of the centroids
	for c := 1; c < k; c++ {
		// For each point, compute the squared distance to the nearest centroid
		totalDistance := 0.0
		for i, p := range points {
			minDist := math.MaxFloat64
			for _, centroid := range centroids {
				dist := angularDistance(p, centroid)
				distSquared := dist * dist
				if distSquared < minDist {
					minDist = distSquared
				}
			}
			distances[i] = minDist
			totalDistance += minDist
		}

		// Choose a new centroid, weighted by the squared distances
		threshold := rand.Float64() * totalDistance
		cumulativeDistance := 0.0
		for i, d := range distances {
			cumulativeDistance += d
			if cumulativeDistance >= threshold {
				centroids = append(centroids, points[i])
				break
			}
		}
	}
	return centroids
}

// assignPointsToClusters assigns each point to the nearest centroid.
func assignPointsToClusters(points []Point, centroids []Point) []Cluster {
	k := len(centroids)
	clusters := make([]Cluster, k)
	for i := 0; i < k; i++ {
		clusters[i].Centroid = centroids[i]
		clusters[i].Points = []Point{}
	}

	for _, p := range points {
		minDist := math.MaxFloat64
		minIndex := 0
		for i, c := range centroids {
			dist := angularDistance(p, c)
			if dist < minDist {
				minDist = dist
				minIndex = i
			}
		}
		clusters[minIndex].Points = append(clusters[minIndex].Points, p)
	}
	return clusters
}

// updateCentroids recalculates centroids by computing the mean position of cluster points.
func updateCentroids(clusters []Cluster) []Point {
	newCentroids := make([]Point, len(clusters))
	for i, cluster := range clusters {
		if len(cluster.Points) == 0 {
			// If a cluster has no points, keep the old centroid
			newCentroids[i] = cluster.Centroid
			continue
		}
		sumX, sumY, sumZ := 0.0, 0.0, 0.0
		for _, p := range cluster.Points {
			x, y, z := toCartesian(p)
			sumX += x
			sumY += y
			sumZ += z
		}
		numPoints := float64(len(cluster.Points))
		avgX := sumX / numPoints
		avgY := sumY / numPoints
		avgZ := sumZ / numPoints
		// Normalize the average vector to unit length
		norm := math.Sqrt(avgX*avgX + avgY*avgY + avgZ*avgZ)
		avgX /= norm
		avgY /= norm
		avgZ /= norm
		newCentroids[i] = cartesianToPoint(avgX, avgY, avgZ)
	}
	return newCentroids
}

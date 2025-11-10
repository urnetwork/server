package router

import (
	"context"
	"slices"
	"strings"
	"sync"
	"time"

	"golang.org/x/exp/maps"

	"github.com/golang/glog"

	"github.com/urnetwork/server"
)

type routeStat struct {
	netSuccessDuration time.Duration
	successDurations   []time.Duration
	successCount       int64
	errorCount         int64
	minBucket          int
	maxBucket          int
}

func (self *routeStat) computeStats() {
	slices.Sort(self.successDurations)
}

func (self *routeStat) meanSuccessDuration() time.Duration {
	if self.successCount == 0 {
		return 0
	}
	return self.netSuccessDuration / time.Duration(self.successCount)
}

func (self *routeStat) pSuccessDuration(p int) time.Duration {
	if len(self.successDurations) == 0 {
		return 0
	}
	p = min(100, max(0, p))
	return self.successDurations[(p*len(self.successDurations))/100]
}

type RouterStats struct {
	ctx context.Context

	startTime      time.Time
	bucketDuration time.Duration

	stateLock        sync.Mutex
	bucketRouteStats map[int]map[string]*routeStat
}

func NewRouterStats(ctx context.Context, bucketDuration time.Duration) *RouterStats {
	stats := &RouterStats{
		ctx:              ctx,
		startTime:        time.Now(),
		bucketDuration:   bucketDuration,
		bucketRouteStats: map[int]map[string]*routeStat{},
	}
	go server.HandleError(stats.run)
	return stats
}

func (self *RouterStats) run() {
	for {
		select {
		case <-self.ctx.Done():
			return
		case <-time.After(self.bucketDuration):
		}

		routeStats := self.currentRouteStats()
		self.cleanupRouteStats()

		routes := maps.Keys(routeStats)
		slices.SortFunc(routes, func(a string, b string) int {
			statsA := routeStats[a]
			statsB := routeStats[b]

			// descending
			if statsA.errorCount < statsB.errorCount {
				return 1
			} else if statsB.errorCount < statsA.errorCount {
				return -1
			}

			if statsA.successCount < statsB.successCount {
				return 1
			} else if statsB.successCount < statsA.successCount {
				return -1
			}

			if statsA.pSuccessDuration(50) < statsB.pSuccessDuration(50) {
				return 1
			} else if statsB.pSuccessDuration(50) < statsA.pSuccessDuration(50) {
				return -1
			}

			return strings.Compare(a, b)
		})

		host := server.RequireHost()
		service := server.RequireService()
		block := server.RequireBlock()
		netSuccessDurationPerBucketDuration := float64(0)
		for i, route := range routes {
			stat := routeStats[route]
			glog.Infof(
				"[%s][%s][%s][%02d]%-40s %.fms %d/%d\n",
				host,
				service,
				block,
				i,
				route,
				float64(stat.pSuccessDuration(50)/time.Microsecond)/1000.0,
				stat.errorCount,
				stat.errorCount+stat.successCount,
			)
			netSuccessDurationPerBucketDuration += float64(stat.netSuccessDuration/time.Millisecond) / (float64(self.bucketDuration/time.Millisecond) * float64(stat.maxBucket-stat.minBucket))
		}
		glog.Infof(
			"[%s][%s][%s] ++ %-40s %.2f\n",
			host,
			service,
			block,
			"net concurrent db connections",
			netSuccessDurationPerBucketDuration,
		)
	}
}

func (self *RouterStats) currentBucket() int {
	return int(time.Now().Sub(self.startTime) / self.bucketDuration)
}

func (self *RouterStats) currentRouteStats() map[string]*routeStat {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	n := 2

	currentBucket := self.currentBucket()

	netRouteStats := map[string]*routeStat{}
	for i := 0; i < n; i += 1 {
		bucket := currentBucket - i

		routeStats, ok := self.bucketRouteStats[bucket]
		if ok {
			for route, stat := range routeStats {
				netStat, ok := netRouteStats[route]
				if !ok {
					netStat = &routeStat{
						minBucket: bucket,
						maxBucket: bucket + 1,
					}
					netRouteStats[route] = netStat
				}
				netStat.minBucket = min(netStat.minBucket, stat.minBucket)
				netStat.maxBucket = min(netStat.maxBucket, stat.maxBucket)
				netStat.netSuccessDuration += stat.netSuccessDuration
				netStat.successDurations = append(netStat.successDurations, stat.successDurations...)
				netStat.successCount += stat.successCount
				netStat.errorCount += stat.errorCount
			}
		}
	}

	for _, netStat := range netRouteStats {
		netStat.computeStats()
	}

	return netRouteStats
}

func (self *RouterStats) cleanupRouteStats() {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	n := 2

	currentBucket := self.currentBucket()

	for bucket, _ := range self.bucketRouteStats {
		if bucket <= currentBucket-n {
			delete(self.bucketRouteStats, bucket)
		}
	}
}

func (self *RouterStats) Success(route string, duration time.Duration) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	bucket := self.currentBucket()

	routeStats, ok := self.bucketRouteStats[bucket]
	if !ok {
		routeStats = map[string]*routeStat{}
		self.bucketRouteStats[bucket] = routeStats
	}
	stat, ok := routeStats[route]
	if !ok {
		stat = &routeStat{
			minBucket: bucket,
			maxBucket: bucket + 1,
		}
		routeStats[route] = stat
	}
	stat.netSuccessDuration += duration
	stat.successDurations = append(stat.successDurations, duration)
	stat.successCount += 1
}

func (self *RouterStats) Error(route string) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	bucket := self.currentBucket()

	routeStats, ok := self.bucketRouteStats[bucket]
	if !ok {
		routeStats = map[string]*routeStat{}
		self.bucketRouteStats[bucket] = routeStats
	}
	stat, ok := routeStats[route]
	if !ok {
		stat = &routeStat{
			minBucket: bucket,
			maxBucket: bucket + 1,
		}
		routeStats[route] = stat
	}
	stat.errorCount += 1
}

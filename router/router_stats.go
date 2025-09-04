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
	successCount       int64
	errorCount         int64
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

			if statsA.netSuccessDuration < statsB.netSuccessDuration {
				return 1
			} else if statsB.netSuccessDuration < statsA.netSuccessDuration {
				return -1
			}

			return strings.Compare(a, b)
		})

		host := server.RequireHost()
		service := server.RequireService()
		block := server.RequireBlock()
		for i, route := range routes {
			stats := routeStats[route]
			glog.Infof(
				"[%s][%s][%s][%02d]%-40s %.fms %d/%d\n",
				host,
				service,
				block,
				i,
				route,
				float64(stats.netSuccessDuration/time.Nanosecond)/(1000.0*1000.0),
				stats.errorCount,
				stats.errorCount+stats.successCount,
			)
		}
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
					netStat = &routeStat{}
					netRouteStats[route] = netStat
				}
				netStat.netSuccessDuration += stat.netSuccessDuration
				netStat.successCount += stat.successCount
				netStat.errorCount += stat.errorCount
			}
		}
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
		stat = &routeStat{}
		routeStats[route] = stat
	}
	stat.netSuccessDuration += duration
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
		stat = &routeStat{}
		routeStats[route] = stat
	}
	stat.errorCount += 1
}

package controller

import (
	"github.com/prometheus/client_golang/prometheus"

	"github.com/urnetwork/connect/v2026"
)

// Message pool health used to be observable only through the connect
// library's `pool[...] r=/t=/c=` line, which printed once per pool per tag
// every 60s and was the single largest log source in the connect and api
// services. That per-tag detail now sits behind the library logger's V(1),
// so these export the aggregate the leak signal actually depends on: a
// return count that falls behind taken is a pooled-buffer leak, and a
// created count that tracks taken means the pool is not being reused.
//
// The connect library deliberately has no prometheus dependency (it ships
// inside the SDK on user devices), so the collectors live here, on the
// server side, reading the library's exported counters at scrape time.
func init() {
	prometheus.MustRegister(
		prometheus.NewCounterFunc(
			prometheus.CounterOpts{
				Namespace: "urnetwork",
				Subsystem: "message_pool",
				Name:      "taken_total",
				Help:      "Pooled message buffers taken",
			},
			func() float64 {
				taken, _, _ := connect.MessagePoolCounts()
				return float64(taken)
			},
		),
		prometheus.NewCounterFunc(
			prometheus.CounterOpts{
				Namespace: "urnetwork",
				Subsystem: "message_pool",
				Name:      "returned_total",
				Help:      "Pooled message buffers returned; falling behind taken is a leak",
			},
			func() float64 {
				_, returned, _ := connect.MessagePoolCounts()
				return float64(returned)
			},
		),
		prometheus.NewCounterFunc(
			prometheus.CounterOpts{
				Namespace: "urnetwork",
				Subsystem: "message_pool",
				Name:      "created_total",
				Help:      "Pooled message buffers allocated; tracking taken means no reuse",
			},
			func() float64 {
				_, _, created := connect.MessagePoolCounts()
				return float64(created)
			},
		),
		prometheus.NewCounterFunc(
			prometheus.CounterOpts{
				Namespace: "urnetwork",
				Subsystem: "message_pool",
				Name:      "unpooled_taken_total",
				Help:      "Messages served outside the pools (oversize classes)",
			},
			func() float64 {
				taken, _ := connect.MessagePoolUnpooledCounts()
				return float64(taken)
			},
		),
		prometheus.NewCounterFunc(
			prometheus.CounterOpts{
				Namespace: "urnetwork",
				Subsystem: "message_pool",
				Name:      "unpooled_bytes_total",
				Help:      "Bytes served outside the pools",
			},
			func() float64 {
				_, byteCount := connect.MessagePoolUnpooledCounts()
				return float64(byteCount)
			},
		),
	)
}

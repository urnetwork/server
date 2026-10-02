package main

import "time"

type hostContainer struct {
	ID          string    `json:"id"`
	PID         int       `json:"pid"`
	Image       string    `json:"image"`
	StartedAt   time.Time `json:"started_at"`
	Environment string    `json:"environment"`
	Service     string    `json:"service"`
	Block       string    `json:"block"`
	Version     string    `json:"version"`
}
type hostCaptureProcess struct {
	Container        hostContainer    `json:"container"`
	ExecutableSHA256 string           `json:"executable_sha256"`
	Endpoint         *captureEndpoint `json:"endpoint,omitempty"`
	Reason           string           `json:"reason"`
}
type hostCaptureInventory struct {
	Hostname    string               `json:"hostname"`
	Environment string               `json:"environment"`
	StartedAt   time.Time            `json:"started_at"`
	FinishedAt  time.Time            `json:"finished_at"`
	Complete    bool                 `json:"complete"`
	Processes   []hostCaptureProcess `json:"processes"`
}

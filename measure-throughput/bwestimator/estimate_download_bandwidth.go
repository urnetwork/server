package bwestimator

import (
	"context"
	"fmt"
	"io"
	"net"
	"time"
)

func EstimateDownloadBandwidth(ctx context.Context, conn net.Conn, timeout time.Duration) (float64, error) {

	err := conn.SetDeadline(time.Now().Add(timeout))
	if err != nil {
		return 0, fmt.Errorf("failed to set deadline: %w", err)
	}

	start := time.Now()

	cnt, err := io.Copy(io.Discard, conn)
	if err != nil {
		return 0, fmt.Errorf("failed to copy: %w", err)
	}

	elapsed := time.Since(start)

	return float64(cnt) / elapsed.Seconds(), nil

}

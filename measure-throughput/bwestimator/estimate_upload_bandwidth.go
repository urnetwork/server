package bwestimator

import (
	"context"
	"fmt"
	"net"
	"time"
)

func EstimateUploadBandwidth(ctx context.Context, conn net.Conn, duration time.Duration) (float64, error) {

	data := make([]byte, 1024)
	// fill data
	for i := range data {
		data[i] = byte(i)
	}

	start := time.Now()
	err := conn.SetDeadline(time.Now().Add(duration))
	if err != nil {
		return 0, fmt.Errorf("failed to set deadline: %w", err)
	}
	uploaded := 0

	for ctx.Err() == nil {
		n, err := conn.Write(data)
		uploaded += n
		if err != nil {
			elapsed := time.Since(start)
			return float64(uploaded) / elapsed.Seconds(), err
		}

	}

	elapsed := time.Since(start)

	return float64(uploaded) / elapsed.Seconds(), err

}

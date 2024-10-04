package datasource

import (
	"context"
	"fmt"
	"io"
	"net"
	"time"

	"github.com/jedib0t/go-pretty/v6/progress"
)

func RunSink(ctx context.Context, addr string, pw progress.Writer) (err error) {

	tracker := &progress.Tracker{
		Message: "Data Sink is running",
		Total:   0,
	}

	pw.AppendTracker(tracker)
	tracker.Start()

	tracker.Increment(1)

	defer func() {
		if ctx.Err() != nil {
			err = nil
		}

		if err != nil {
			tracker.UpdateMessage(fmt.Sprintf("Data Sink failed: %v", err))
			tracker.MarkAsErrored()
			return
		}
		tracker.UpdateMessage("Data Sink is done")
		tracker.MarkAsDone()
	}()

	l, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("failed to listen: %w", err)
	}

	context.AfterFunc(ctx, func() {
		l.Close()
	})

	defer l.Close()

	for ctx.Err() == nil {
		conn, err := l.Accept()
		if err != nil {
			return fmt.Errorf("failed to accept: %w", err)
		}

		startTime := time.Now()

		go func() {
			defer conn.Close()
			n, _ := io.Copy(io.Discard, conn)

			perSecond := float64(n) / time.Since(startTime).Seconds()

			conn.Write([]byte(fmt.Sprintf("%d\n%0.3f\n", n, perSecond)))

		}()
	}

	return nil

}

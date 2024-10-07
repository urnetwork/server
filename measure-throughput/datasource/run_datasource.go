package datasource

import (
	"context"
	"fmt"
	"net"
	"time"

	"github.com/jedib0t/go-pretty/v6/progress"
)

var datablock = make([]byte, 1024)

func init() {
	//
	for i := range datablock {
		datablock[i] = byte(i % 256)
	}
}

func Run(ctx context.Context, addr string, pw progress.Writer, kilobytesToWrite int, timeout time.Duration) (err error) {

	tracker := &progress.Tracker{
		Message: "Datasource is running",
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
			tracker.UpdateMessage(fmt.Sprintf("Datasource failed: %v", err))
			tracker.MarkAsErrored()
			return
		}
		tracker.UpdateMessage("Datasource is done")
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

		go func() {
			defer conn.Close()

			conn.SetDeadline(time.Now().Add(timeout))

			for range kilobytesToWrite {
				_, err = conn.Write(datablock)
				if err != nil {
					return
				}
			}
		}()
	}

	return nil

}

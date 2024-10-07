package healthcheck

import (
	"context"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/cenkalti/backoff/v4"
)

func WaitForEndpoint(
	ctx context.Context,
	url string,
	isHealthy func(statusCode int, body []byte) bool,
	maxWait time.Duration,
) error {

	bo := backoff.NewExponentialBackOff()
	bo.MaxElapsedTime = maxWait
	bo.MaxInterval = 1 * time.Second

	withContext := backoff.WithContext(bo, ctx)

	client := http.Client{
		Timeout: 1 * time.Second,
	}

	return backoff.Retry(
		func() error {
			res, err := client.Get(url)
			if err != nil {
				return err
			}

			defer res.Body.Close()

			body, err := io.ReadAll(res.Body)
			if err != nil {
				return err
			}

			if isHealthy(res.StatusCode, body) {
				return nil

			}

			return errors.New("not healthy")
		},
		withContext,
	)

}

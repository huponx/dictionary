package lookup

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/huponx/gocommon/retry"
)

// maxBody bounds what is read from a provider.
const maxBody = 2 << 20

// getter makes the GET requests of the providers: a User-Agent, a bounded body and a retry with
// backoff on network errors, 429 and 5xx.
type getter struct {
	client    *http.Client
	userAgent string
	retry     retry.Config
}

func newGetter(client *http.Client, userAgent string) *getter {
	return &getter{
		client:    client,
		userAgent: userAgent,
		retry:     retry.Config{MaxAttempts: 3, InitialWait: 300 * time.Millisecond, MaxWait: 2 * time.Second, Multiplier: 2, Jitter: 0.2},
	}
}

// get returns the body and the status of the final response. A status below 500 other than 429 is
// returned to the caller (404 means "no entry"); the other statuses are retried and then fail with
// ErrUnavailable.
func (g *getter) get(ctx context.Context, url string) ([]byte, int, error) {
	var (
		body   []byte
		status int
	)
	err := retry.Do(ctx, g.retry, func() error {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return err
		}
		req.Header.Set("User-Agent", g.userAgent)
		req.Header.Set("Accept", "application/json")
		resp, err := g.client.Do(req)
		if err != nil {
			return fmt.Errorf("%w: %v", ErrUnavailable, err)
		}
		defer resp.Body.Close()
		b, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
		if err != nil {
			return fmt.Errorf("%w: %v", ErrUnavailable, err)
		}
		if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500 {
			return fmt.Errorf("%w: status %d", ErrUnavailable, resp.StatusCode)
		}
		body, status = b, resp.StatusCode
		return nil
	})
	if err != nil {
		return nil, 0, err
	}
	return body, status, nil
}

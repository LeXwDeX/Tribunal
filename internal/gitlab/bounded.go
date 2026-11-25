package gitlab

import (
	"context"
	"errors"
	"io"
	"net/http"
)

// MaxRawFileBytes bounds one raw repository file body. Diff recovery already
// rejects larger bodies; the client enforces the same bound while reading so
// an arbitrarily large blob never reaches memory before that check.
const MaxRawFileBytes = 2 << 20

// MaxRecoveryFileBytes bounds one raw repository file body read for diff
// recovery. Recovery rebuilds a diff GitLab withheld as too_large, so it must
// accept bodies the ordinary MaxRawFileBytes bound rejects; it is still capped
// so an arbitrarily large blob never reaches memory.
const MaxRecoveryFileBytes = 8 << 20

// ErrFileTooLarge reports a raw repository file above MaxRawFileBytes. It is
// a permanent outcome: callers must not treat it as a transient upstream
// failure and retry it.
var ErrFileTooLarge = errors.New("gitlab: repository file exceeds the size limit")

// getBounded issues a raw authenticated GET and reads at most limit+1 bytes.
// The response body is closed as soon as the bound is crossed, so the process
// never holds more than limit+1 bytes of it regardless of Content-Length or
// chunking. The bool reports that the body was over the bound.
func (c *Client) getBounded(reqCtx context.Context, token, op, rawURL string, limit int64) ([]byte, bool, error) {
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, false, &Error{Op: op}
	}
	req.Header.Set("PRIVATE-TOKEN", token)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, false, mapError(reqCtx, op, nil, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, false, &Error{Op: op, Status: resp.StatusCode}
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, false, mapError(reqCtx, op, nil, err)
	}
	if int64(len(body)) > limit {
		// Keep the first limit+1 bytes for callers that truncate (job
		// traces); raw-file callers reject the result instead.
		return body, true, nil
	}
	return body, false, nil
}

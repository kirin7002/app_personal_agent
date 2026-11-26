// Package runs implements the Hermes Runs API used for vehicle turns.
package runs

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/kirin7002/app_personal_agent/contract"
)

type StartRequest struct {
	SessionID    string `json:"session_id"`
	Input        string `json:"input"`
	Instructions string `json:"instructions"`
}

type Run struct {
	ID     string `json:"run_id"`
	Status string `json:"status"`
	Output string `json:"output"`
}

type Client struct {
	base string
	key  string
	http *http.Client

	// Enable only after confirming durable idempotency on the deployed Hermes.
	RetryAdmission bool
}

func New(base, key string, client *http.Client) (*Client, error) {
	u, err := url.Parse(base)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" ||
		u.User != nil || u.RawQuery != "" || u.Fragment != "" || key == "" {
		return nil, errors.New("Hermes requires an HTTP(S) base URL without user info or query and a service key")
	}
	if client == nil {
		client = http.DefaultClient
	}
	copyClient := *client
	copyClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &Client{base: strings.TrimRight(u.String(), "/"), key: key, http: &copyClient}, nil
}

type HTTPError struct {
	Operation string
	Status    int
}

func (e *HTTPError) Error() string {
	return fmt.Sprintf("Hermes %s returned HTTP %d", e.Operation, e.Status)
}

func (c *Client) open(ctx context.Context, method, path, accept string, body []byte, headers http.Header) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	for key, values := range headers {
		req.Header[key] = values
	}
	if method == http.MethodPost && !c.RetryAdmission {
		req.GetBody = nil
	}
	req.Header.Set("Authorization", "Bearer "+c.key)
	req.Header.Set("Accept", accept)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return c.http.Do(req)
}

var runID = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,128}$`)
var ErrAdmissionUnknown = errors.New("Hermes admission outcome unknown")
var ErrRunUnknown = errors.New("Hermes execution outcome unknown")

func (c *Client) Start(ctx context.Context, payload StartRequest, metadata contract.TurnMetadata, idempotencyKey string) (Run, error) {
	if metadata.HermesSessionID != payload.SessionID || metadata.TurnHandle == "" || idempotencyKey == "" {
		return Run{}, errors.New("invalid trusted Run admission metadata")
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return Run{}, err
	}
	headers := make(http.Header)
	headers.Set("Idempotency-Key", idempotencyKey)
	headers.Set(contract.TurnHandleHeader, metadata.TurnHandle)
	headers.Set(contract.HermesSessionHeader, metadata.HermesSessionID)
	for attempt := 0; ; attempt++ {
		call, cancel := context.WithTimeout(ctx, 5*time.Second)
		resp, err := c.open(call, http.MethodPost, "/v1/runs", "application/json", data, headers)
		var run Run
		var ambiguous bool
		switch {
		case err != nil:
			ambiguous = true
		case resp.StatusCode == http.StatusAccepted:
			err = json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&run)
			ambiguous = err != nil || !runID.MatchString(run.ID)
			if !ambiguous {
				resp.Body.Close()
				cancel()
				return run, nil
			}
		default:
			err = &HTTPError{Operation: "start", Status: resp.StatusCode}
			ambiguous = resp.StatusCode >= 500 || resp.StatusCode == http.StatusRequestTimeout
		}
		if resp != nil {
			resp.Body.Close()
		}
		cancel()
		if !ambiguous {
			return Run{}, err
		}
		if !c.RetryAdmission || attempt == 2 {
			return Run{}, ErrAdmissionUnknown
		}
		if err := pause(ctx, time.Duration(attempt+1)*150*time.Millisecond); err != nil {
			return Run{}, errors.Join(ErrAdmissionUnknown, err)
		}
	}
}

func (c *Client) Stop(ctx context.Context, id string) error {
	if !runID.MatchString(id) {
		return errors.New("invalid Hermes Run ID")
	}
	resp, err := c.open(ctx, http.MethodPost, "/v1/runs/"+id+"/stop", "application/json", nil, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &HTTPError{Operation: "stop", Status: resp.StatusCode}
	}
	return nil
}

func (c *Client) status(ctx context.Context, id string) (Run, error) {
	resp, err := c.open(ctx, http.MethodGet, "/v1/runs/"+id, "application/json", nil, nil)
	if err != nil {
		return Run{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return Run{}, &HTTPError{Operation: "status", Status: resp.StatusCode}
	}
	var run Run
	err = json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&run)
	if err == nil && run.ID != id {
		err = errors.New("Hermes status returned a different Run ID")
	}
	return run, err
}

func (r Run) Terminal() bool {
	switch r.Status {
	case "completed", "failed", "cancelled", "canceled", "interrupted", "incomplete", "expired":
		return true
	}
	return false
}

// Wait uses only the Run's terminal event or polled terminal state as the final reply.
func (c *Client) Wait(ctx context.Context, id string) (Run, error) {
	if !runID.MatchString(id) {
		return Run{}, errors.New("invalid Hermes Run ID")
	}
	var seq int64
	for {
		pollCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		run, err := c.status(pollCtx, id)
		cancel()
		if err == nil {
			if run.Terminal() {
				return run, nil
			}
			if run.Status == "unknown" {
				return Run{}, ErrRunUnknown
			}
		} else {
			var httpErr *HTTPError
			if errors.As(err, &httpErr) && httpErr.Status >= 400 && httpErr.Status < 500 && httpErr.Status != 429 {
				return Run{}, err
			}
		}
		streamCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		run, err = c.stream(streamCtx, id, &seq)
		cancel()
		if err == nil {
			return run, nil
		}
		if err := pause(ctx, 150*time.Millisecond); err != nil {
			return Run{}, err
		}
	}
}

func (c *Client) stream(ctx context.Context, id string, seq *int64) (Run, error) {
	headers := make(http.Header)
	if *seq > 0 {
		headers.Set("Last-Event-ID", strconv.FormatInt(*seq, 10))
	}
	resp, err := c.open(ctx, http.MethodGet, "/v1/runs/"+id+"/events", "text/event-stream", nil, headers)
	if err != nil {
		return Run{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return Run{}, &HTTPError{Operation: "events", Status: resp.StatusCode}
	}
	lines := bufio.NewScanner(resp.Body)
	lines.Buffer(make([]byte, 4096), 1<<20)
	var data strings.Builder
	for lines.Scan() {
		line := lines.Text()
		if line != "" {
			if field, value, ok := strings.Cut(line, ":"); ok && field == "data" {
				if data.Len()+len(value) > 1<<20 {
					return Run{}, errors.New("Hermes SSE event too large")
				}
				data.WriteString(strings.TrimPrefix(value, " "))
				data.WriteByte('\n')
			}
			continue
		}
		if data.Len() == 0 {
			continue
		}
		var ev struct {
			Run
			Event string `json:"event"`
			Seq   int64  `json:"seq"`
		}
		err := json.Unmarshal([]byte(data.String()), &ev)
		data.Reset()
		if err != nil || ev.ID != id || (ev.Seq > 0 && ev.Seq <= *seq) {
			continue
		}
		if ev.Seq > *seq {
			*seq = ev.Seq
		}
		ev.Run.Status = strings.TrimPrefix(ev.Event, "run.")
		if strings.HasPrefix(ev.Event, "run.") && ev.Run.Terminal() {
			return ev.Run, nil
		}
	}
	if lines.Err() != nil {
		return Run{}, lines.Err()
	}
	return Run{}, io.ErrUnexpectedEOF
}

func pause(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

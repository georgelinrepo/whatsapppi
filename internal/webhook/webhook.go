// Package webhook posts the feed to the Apps Script web app. Apps Script
// cannot set HTTP status codes, so success is a 200 whose body is exactly "ok".
// This package logs nothing and never puts the secret, the URL or a response
// body in an error.
package webhook

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"
)

type Payload struct {
	Secret string `json:"secret"`
	ID     string `json:"id"`
	Date   string `json:"date"`
	Text   string `json:"text"`
}

type Sender struct {
	URL    string
	Client *http.Client                                     // default: 60s timeout, follows redirects
	Waits  []time.Duration                                  // default {5m, 5m, 5m}
	Sleep  func(ctx context.Context, d time.Duration) error // default: timer or ctx.Done
}

var ErrNotOK = errors.New("webhook: response was not ok")

// errTransport is a URL-free stand-in for *url.Error from http.Client.Do.
var errTransport = errors.New("webhook: request failed")

func defaultSleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Send posts p, retrying after each wait; every attempt sends the same payload.
func (s *Sender) Send(ctx context.Context, p Payload) error {
	client := s.Client
	if client == nil {
		client = &http.Client{Timeout: 60 * time.Second}
	}
	waits := s.Waits
	if waits == nil {
		waits = []time.Duration{5 * time.Minute, 5 * time.Minute, 5 * time.Minute}
	}
	sleep := s.Sleep
	if sleep == nil {
		sleep = defaultSleep
	}
	body, err := json.Marshal(p)
	if err != nil {
		return fmt.Errorf("webhook: encode payload: %w", err)
	}

	attempts := 1 + len(waits)
	var last error
	for i := 0; i < attempts; i++ {
		if i > 0 {
			if err := sleep(ctx, waits[i-1]); err != nil {
				return err
			}
		}
		last = s.attempt(ctx, client, body)
		if last == nil {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
	}
	return fmt.Errorf("after %d attempts: %w", attempts, last)
}

func (s *Sender) attempt(ctx context.Context, client *http.Client, body []byte) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.URL, bytes.NewReader(body))
	if err != nil {
		return errTransport // NewRequest errors can embed the URL
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return errTransport // *url.Error would carry the URL
	}
	defer resp.Body.Close()
	got, err := io.ReadAll(io.LimitReader(resp.Body, 1024))
	if err != nil {
		return errTransport
	}
	if resp.StatusCode != http.StatusOK || string(got) != "ok" {
		return fmt.Errorf("%w (status %d)", ErrNotOK, resp.StatusCode)
	}
	return nil
}

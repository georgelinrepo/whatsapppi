// Package webhook posts the feed to the Apps Script web app. Apps Script
// cannot set HTTP status codes, so success is a 200 whose body is exactly "ok".
// This package logs nothing and never puts the secret, the URL or a response
// body in an error.
package webhook

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"syscall"
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

// transportError is a URL-free stand-in for *url.Error from http.Client.Do:
// it carries only a category, never the raw error text, host or URL.
type transportError struct{ cat string }

func (e transportError) Error() string { return "webhook: request failed (" + e.cat + ")" }

func transportErr(err error) error { return transportError{cat: category(err)} }

// category classifies a transport error as timeout, dns, tls, refused or other.
func category(err error) string {
	var ne net.Error
	if errors.Is(err, context.DeadlineExceeded) || errors.As(err, &ne) && ne.Timeout() {
		return "timeout"
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return "dns"
	}
	var (
		recErr   tls.RecordHeaderError
		alertErr tls.AlertError
		verErr   *tls.CertificateVerificationError
		uaErr    x509.UnknownAuthorityError
		hostErr  x509.HostnameError
		invErr   x509.CertificateInvalidError
		rootsErr x509.SystemRootsError
	)
	if errors.As(err, &recErr) || errors.As(err, &alertErr) || errors.As(err, &verErr) ||
		errors.As(err, &uaErr) || errors.As(err, &hostErr) || errors.As(err, &invErr) || errors.As(err, &rootsErr) {
		return "tls"
	}
	if errors.Is(err, syscall.ECONNREFUSED) {
		return "refused"
	}
	return "other"
}

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
	return fmt.Errorf("%w after %d attempts", last, attempts)
}

func (s *Sender) attempt(ctx context.Context, client *http.Client, body []byte) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.URL, bytes.NewReader(body))
	if err != nil {
		return transportError{cat: "other"} // NewRequest errors can embed the URL
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return transportErr(err) // *url.Error would carry the URL
	}
	defer resp.Body.Close()
	got, err := io.ReadAll(io.LimitReader(resp.Body, 1024))
	if err != nil {
		return transportErr(err)
	}
	if resp.StatusCode != http.StatusOK || string(got) != "ok" {
		return fmt.Errorf("%w (status %d)", ErrNotOK, resp.StatusCode)
	}
	return nil
}

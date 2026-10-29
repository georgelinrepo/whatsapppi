package webhook

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

var testPayload = Payload{Secret: "s3cret-value", ID: "2026-10-26T06:35:00Z", Date: "26 Oct 2026", Text: "hello £ 😀"}

type sleeps struct {
	mu sync.Mutex
	n  int
}

func (s *sleeps) sleep(ctx context.Context, d time.Duration) error {
	s.mu.Lock()
	s.n++
	s.mu.Unlock()
	return ctx.Err()
}

func newSender(url string, sl *sleeps) *Sender {
	return &Sender{URL: url, Waits: []time.Duration{0, 0, 0}, Sleep: sl.sleep}
}

func TestSendOK(t *testing.T) {
	var reqs int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reqs++
		if r.Method != http.MethodPost {
			t.Errorf("method = %s", r.Method)
		}
		if ct := r.Header.Get("Content-Type"); ct != "application/json" {
			t.Errorf("content type = %q", ct)
		}
		var got Payload
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Errorf("decode: %v", err)
		}
		if got != testPayload {
			t.Errorf("payload = %+v", got)
		}
		w.Write([]byte("ok"))
	}))
	defer srv.Close()
	sl := &sleeps{}
	if err := newSender(srv.URL, sl).Send(context.Background(), testPayload); err != nil {
		t.Fatal(err)
	}
	if reqs != 1 || sl.n != 0 {
		t.Errorf("reqs=%d sleeps=%d", reqs, sl.n)
	}
}

func TestFollowsAppsScriptRedirect(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/exec", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/echo", http.StatusFound)
	})
	mux.HandleFunc("/echo", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("redirect method = %s", r.Method)
		}
		w.Write([]byte("ok"))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	if err := newSender(srv.URL+"/exec", &sleeps{}).Send(context.Background(), testPayload); err != nil {
		t.Fatal(err)
	}
}

func TestRetriesThenFails(t *testing.T) {
	var mu sync.Mutex
	var ids []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var got Payload
		json.NewDecoder(r.Body).Decode(&got)
		mu.Lock()
		ids = append(ids, got.ID)
		mu.Unlock()
		w.Write([]byte("forbidden"))
	}))
	defer srv.Close()
	sl := &sleeps{}
	err := newSender(srv.URL, sl).Send(context.Background(), testPayload)
	if !errors.Is(err, ErrNotOK) {
		t.Fatalf("err = %v", err)
	}
	if len(ids) != 4 || sl.n != 3 {
		t.Fatalf("reqs=%d sleeps=%d", len(ids), sl.n)
	}
	for _, id := range ids {
		if id != testPayload.ID {
			t.Errorf("id = %q", id)
		}
	}
	for _, leak := range []string{testPayload.Secret, srv.URL, "forbidden"} {
		if strings.Contains(err.Error(), leak) {
			t.Errorf("error leaks %q: %v", leak, err)
		}
	}
}

func TestNotOKVariants(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
	}{
		{"trailing newline", 200, "ok\n"},
		{"leading space", 200, " ok"},
		{"sign-in page", 200, "<html>Sign in</html>"},
		{"ok but 500", 500, "ok"},
		{"empty", 200, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(c.status)
				w.Write([]byte(c.body))
			}))
			defer srv.Close()
			err := newSender(srv.URL, &sleeps{}).Send(context.Background(), testPayload)
			if !errors.Is(err, ErrNotOK) {
				t.Errorf("err = %v", err)
			}
		})
	}
}

func TestRecoversOnRetry(t *testing.T) {
	var reqs int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reqs++
		if reqs < 3 {
			w.Write([]byte("error"))
			return
		}
		w.Write([]byte("ok"))
	}))
	defer srv.Close()
	if err := newSender(srv.URL, &sleeps{}).Send(context.Background(), testPayload); err != nil {
		t.Fatal(err)
	}
	if reqs != 3 {
		t.Errorf("reqs = %d", reqs)
	}
}

func TestContextCancel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("error"))
	}))
	defer srv.Close()
	ctx, cancel := context.WithCancel(context.Background())
	s := &Sender{URL: srv.URL, Waits: []time.Duration{time.Hour}, Sleep: func(ctx context.Context, d time.Duration) error {
		cancel()
		<-ctx.Done()
		return ctx.Err()
	}}
	err := s.Send(ctx, testPayload)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
}

func TestTransportErrorDoesNotLeakURL(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := srv.URL + "/macros/s/DEPLOYMENT-ID/exec"
	srv.Close()
	err := newSender(url, &sleeps{}).Send(context.Background(), testPayload)
	if err == nil {
		t.Fatal("want error")
	}
	host := strings.TrimPrefix(srv.URL, "http://")
	for _, leak := range []string{"DEPLOYMENT-ID", srv.URL, host, "127.0.0.1", "connection refused"} {
		if strings.Contains(err.Error(), leak) {
			t.Errorf("error leaks %q: %v", leak, err)
		}
	}
	if want := "webhook: request failed (refused) after 4 attempts"; err.Error() != want {
		t.Errorf("err = %q, want %q", err, want)
	}
}

func TestTransportErrorCategories(t *testing.T) {
	// Self-signed certificate: the default client rejects it.
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()
	err := (&Sender{URL: srv.URL + "/exec", Waits: []time.Duration{}}).Send(context.Background(), testPayload)
	if err == nil || !strings.Contains(err.Error(), "(tls)") || strings.Contains(err.Error(), "127.0.0.1") {
		t.Errorf("tls: %v", err)
	}

	// Client timeout.
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(300 * time.Millisecond)
	}))
	defer slow.Close()
	s := &Sender{URL: slow.URL, Client: &http.Client{Timeout: 20 * time.Millisecond}, Waits: []time.Duration{}}
	if err := s.Send(context.Background(), testPayload); err == nil || !strings.Contains(err.Error(), "(timeout)") {
		t.Errorf("timeout: %v", err)
	}

	wrap := func(e error) error { return &url.Error{Op: "Post", URL: "https://h.example/x", Err: e} }
	for _, tc := range []struct {
		err  error
		want string
	}{
		{wrap(&net.DNSError{Err: "no such host", Name: "h.example", IsNotFound: true}), "dns"},
		{wrap(context.DeadlineExceeded), "timeout"},
		{wrap(&net.OpError{Op: "dial", Err: os.NewSyscallError("connect", syscall.ECONNREFUSED)}), "refused"},
		{wrap(x509.UnknownAuthorityError{}), "tls"},
		{wrap(x509.HostnameError{Certificate: &x509.Certificate{}, Host: "h.example"}), "tls"},
		{wrap(&tls.CertificateVerificationError{Err: errors.New("x")}), "tls"},
		{wrap(tls.RecordHeaderError{Msg: "bad"}), "tls"},
		{wrap(tls.AlertError(40)), "tls"},
		{wrap(errors.New("something h.example")), "other"},
	} {
		got := category(tc.err)
		if got != tc.want {
			t.Errorf("category(%v) = %q, want %q", tc.err, got, tc.want)
		}
		if msg := transportErr(tc.err).Error(); msg != fmt.Sprintf("webhook: request failed (%s)", tc.want) {
			t.Errorf("message %q", msg)
		}
	}
}

func TestDefaultSleepHonoursContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := defaultSleep(ctx, time.Hour); !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v", err)
	}
}

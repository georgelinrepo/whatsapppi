package deploy

import (
	"os"
	"strings"
	"testing"
)

func lines(t *testing.T, path string) map[string]bool {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	m := map[string]bool{}
	for _, l := range strings.Split(string(b), "\n") {
		m[strings.TrimSpace(l)] = true
	}
	return m
}

func TestServiceUnit(t *testing.T) {
	got := lines(t, "feedbridge.service")
	for _, want := range []string{
		"Type=oneshot",
		"TimeoutStartSec=30min",
		"User=feedbridge",
		"ExecStart=/usr/local/bin/feedbridge --run",
		"NoNewPrivileges=yes",
		"ProtectSystem=strict",
		"ProtectHome=yes",
		"PrivateTmp=yes",
		"ReadWritePaths=/var/lib/feedbridge",
		"RestrictAddressFamilies=AF_INET AF_INET6",
		"CapabilityBoundingSet=",
		"MemoryMax=256M",
		"Wants=network-online.target time-sync.target",
		"After=network-online.target time-sync.target",
	} {
		if !got[want] {
			t.Errorf("feedbridge.service: missing line %q", want)
		}
	}
}

func TestTimerUnit(t *testing.T) {
	got := lines(t, "feedbridge.timer")
	for _, want := range []string{
		"OnCalendar=*-*-* 06:30 Europe/London",
		"Persistent=true",
		"WantedBy=timers.target",
	} {
		if !got[want] {
			t.Errorf("feedbridge.timer: missing line %q", want)
		}
	}
}

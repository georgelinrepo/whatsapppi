package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"feedbridge/internal/lock"
)

func writeConfig(t *testing.T, groups string) (cfgPath, dir string) {
	t.Helper()
	dir = t.TempDir()
	url := filepath.Join(dir, "webhook.url")
	sec := filepath.Join(dir, "webhook.secret")
	os.WriteFile(url, []byte("https://script.google.com/macros/s/x/exec\n"), 0o600)
	os.WriteFile(sec, []byte(strings.Repeat("ab", 32)+"\n"), 0o600)
	cfgPath = filepath.Join(dir, "config.toml")
	toml := "[whatsapp]\ngroups = [" + groups + "]\nsession_db = \"" + filepath.Join(dir, "session.db") + "\"\n" +
		"[store]\npath = \"" + filepath.Join(dir, "messages.db") + "\"\n" +
		"[feed]\nwebhook_url_file = \"" + url + "\"\nsecret_file = \"" + sec + "\"\n"
	if err := os.WriteFile(cfgPath, []byte(toml), 0o600); err != nil {
		t.Fatal(err)
	}
	return cfgPath, dir
}

func TestVersionNeedsNoConfig(t *testing.T) {
	var out, errb bytes.Buffer
	if code := realMain([]string{"--version", "--config", "/nonexistent"}, &out, &errb); code != 0 {
		t.Fatalf("exit %d", code)
	}
	if out.String() != version+"\n" {
		t.Fatalf("got %q", out.String())
	}
}

func TestUsageErrors(t *testing.T) {
	for _, args := range [][]string{{}, {"--run", "--link"}, {"--bogus"}, {"--run", "extra"}} {
		var out, errb bytes.Buffer
		if code := realMain(args, &out, &errb); code != 2 {
			t.Errorf("%v: exit %d, want 2", args, code)
		}
	}
}

func TestConfigErrorExit2(t *testing.T) {
	var out, errb bytes.Buffer
	if code := realMain([]string{"--run", "--config", "/nonexistent.toml"}, &out, &errb); code != 2 {
		t.Fatalf("exit %d, want 2", code)
	}
}

func TestRunRefusesEmptyGroups(t *testing.T) {
	cfg, _ := writeConfig(t, "")
	var out, errb bytes.Buffer
	if code := realMain([]string{"--run", "--config", cfg}, &out, &errb); code != 2 {
		t.Fatalf("exit %d, want 2", code)
	}
	if !strings.Contains(errb.String(), "whatsapp.groups") {
		t.Fatalf("message: %q", errb.String())
	}
}

func TestBusyLockExit75(t *testing.T) {
	cfg, dir := writeConfig(t, `"120363000000000001@g.us"`)
	release, err := lock.Acquire(filepath.Join(dir, "lock"))
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	for _, mode := range []string{"--run", "--link", "--list-groups"} {
		var out, errb bytes.Buffer
		if code := realMain([]string{mode, "--config", cfg}, &out, &errb); code != 75 {
			t.Errorf("%s: exit %d, want 75", mode, code)
		}
		if !strings.Contains(errb.String(), "run in progress") {
			t.Errorf("%s: message %q", mode, errb.String())
		}
	}
}

func TestListGroupsUnlinked(t *testing.T) {
	cfg, _ := writeConfig(t, "")
	var out, errb bytes.Buffer
	if code := realMain([]string{"--list-groups", "--config", cfg}, &out, &errb); code != 1 {
		t.Fatalf("exit %d, want 1", code)
	}
	if !strings.Contains(errb.String(), "not linked") {
		t.Fatalf("message %q", errb.String())
	}
}

func TestDatabasesCreatedPrivate(t *testing.T) {
	old := syscall.Umask(0o022) // a typical login umask
	defer syscall.Umask(old)
	cfg, dir := writeConfig(t, "")
	var out, errb bytes.Buffer
	realMain([]string{"--list-groups", "--config", cfg}, &out, &errb)
	for _, name := range []string{"messages.db", "session.db"} {
		fi, err := os.Stat(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		if perm := fi.Mode().Perm(); perm != 0o600 {
			t.Errorf("%s: mode %o, want 600", name, perm)
		}
	}
}

package store

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spec-studio-app/plugin-json/internal/files"
	"github.com/spec-studio-app/plugin-sdk/go/plugin"
	"github.com/spec-studio-app/plugin-sdk/go/protocol"
)

// R5.S1, R5.S2
func TestDefaultDataDir(t *testing.T) {
	home := func() (string, error) { return "/Users/me", nil }
	noEnv := func(string) string { return "" }
	withSupport := func(k string) string {
		if k == "SPEC_STUDIO_SUPPORT" {
			return "/tmp/test-support"
		}
		return ""
	}
	cases := []struct {
		kind   string
		getenv func(string) string
		want   string
	}{
		{"relay", noEnv, "/data"},
		{"relay", withSupport, "/data"},
		{"app", noEnv, "/Users/me/Library/Application Support/Spec Studio/backend/json"},
		{"app", withSupport, "/tmp/test-support/backend/json"},
		{"test", noEnv, ""},
	}
	for _, c := range cases {
		got, err := DefaultDataDir(c.kind, c.getenv, home)
		if err != nil || got != c.want {
			t.Errorf("%s: %q, %v; want %q", c.kind, got, err, c.want)
		}
	}
	if _, err := DefaultDataDir("app", noEnv, func() (string, error) { return "", errors.New("no home") }); err == nil {
		t.Error("app with no home: no error")
	}
}

// R5.S2: the app's default is used when the setting has no value.
func TestOpenUsesHostDefault(t *testing.T) {
	support := t.TempDir()
	s := New()
	s.getenv = func(k string) string {
		if k == "SPEC_STUDIO_SUPPORT" {
			return support
		}
		return ""
	}
	if err := s.Open(ctx(t), plugin.OpenOptions{Settings: map[string]string{}, Host: protocol.Host{Kind: "app"}, Log: slog.New(slog.DiscardHandler)}); err != nil {
		t.Fatal(err)
	}
	defer s.Close(context.Background())
	if want := filepath.Join(support, "backend", "json"); s.Dir() != want {
		t.Fatalf("dir %s, want %s", s.Dir(), want)
	}
	if _, err := os.Stat(filepath.Join(s.Dir(), "layout.json")); err != nil {
		t.Fatalf("no layout.json: %v", err)
	}
}

// A test host with no data_dir gets a temporary folder, removed at Close.
func TestTestHostTemporaryFolder(t *testing.T) {
	s := New()
	if err := s.Open(ctx(t), plugin.OpenOptions{Host: protocol.Host{Kind: "test"}, Log: slog.New(slog.DiscardHandler)}); err != nil {
		t.Fatal(err)
	}
	dir := s.Dir()
	if !strings.Contains(filepath.Base(dir), "plugin-json-test-") {
		t.Fatalf("dir %s", dir)
	}
	s.Close(context.Background())
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("the temporary folder stays: %v", err)
	}
}

// R5.S3
func TestUnwritableDataDir(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can write a read-only folder")
	}
	parent := t.TempDir()
	dir := filepath.Join(parent, "ro")
	os.Mkdir(dir, 0o500)
	t.Cleanup(func() { os.Chmod(dir, 0o700) })
	err := New().Open(ctx(t), plugin.OpenOptions{Settings: map[string]string{SettingDataDir: dir}, Host: protocol.Host{Kind: "relay"}, Log: slog.New(slog.DiscardHandler)})
	if !protocol.Is(err, protocol.BadSettings) {
		t.Fatalf("error %v, want bad_settings", err)
	}
	if msg := err.Error(); !strings.Contains(msg, "data_dir") || !strings.Contains(msg, "write access") {
		t.Fatalf("message %q", msg)
	}
}

// R2.S3
func TestCrashBeforeRename(t *testing.T) {
	dir := t.TempDir()
	s, _ := open(t, dir)
	put(t, s, "specs", "x", "old", nil, nil)
	files.AfterTempWrite = func(string) error { return errors.New("crash") }
	_, err := s.Put(ctx(t), protocol.StatePutParams{Collection: "specs", Key: "x", Value: raw("new"), ExpectedVersion: nil})
	files.AfterTempWrite = nil
	if err == nil {
		t.Fatal("the put with no expected version on an existing document succeeded")
	}
	cur, _ := s.Get(ctx(t), protocol.StateGetParams{Collection: "specs", Key: "x"})
	files.AfterTempWrite = func(string) error { return errors.New("crash") }
	_, err = s.Put(ctx(t), protocol.StatePutParams{Collection: "specs", Key: "x", Value: raw("new"), ExpectedVersion: &cur.Version})
	files.AfterTempWrite = nil
	if err == nil {
		t.Fatal("the crash hook did not stop the write")
	}
	temps, _ := filepath.Glob(filepath.Join(dir, "state", "specs", "*.tmp"))
	if len(temps) != 1 {
		t.Fatalf("temporary files %v, want 1", temps)
	}
	s.Close(ctx(t))

	s2, _ := open(t, dir)
	got, _ := s2.Get(ctx(t), protocol.StateGetParams{Collection: "specs", Key: "x"})
	if string(got.Value) != `"old"` || got.Version != cur.Version {
		t.Fatalf("after the crash: %s %s", got.Value, got.Version)
	}
	temps, _ = filepath.Glob(filepath.Join(dir, "state", "specs", "*.tmp"))
	if len(temps) != 0 {
		t.Fatalf("the start left %v", temps)
	}
}

// R1.S4
func TestBadNameRefused(t *testing.T) {
	parent := t.TempDir()
	dir := filepath.Join(parent, "data")
	c, _ := serve(t, dir)
	for _, p := range []protocol.StatePutParams{
		{Collection: "relay", Key: "../config", Value: raw(1)},
		{Collection: ".", Key: "x", Value: raw(1)},
		{Collection: "..", Key: "x", Value: raw(1)},
		{Collection: "a/b", Key: "x", Value: raw(1)},
	} {
		if _, err := c.Put(ctx(t), p); !protocol.Is(err, protocol.Refused) {
			t.Errorf("put %s / %s: %v, want refused", p.Collection, p.Key, err)
		}
	}
	entries, _ := os.ReadDir(parent)
	if len(entries) != 1 || entries[0].Name() != "data" {
		t.Fatalf("the parent folder has %v", entries)
	}
	if _, err := os.Stat(filepath.Join(dir, "config.json")); err == nil {
		t.Fatal("a file outside the state folder")
	}
}

// Names that differ only in case are two documents, also on a disk that
// ignores case; a name with ".." inside is one file.
func TestNamesKeepCase(t *testing.T) {
	s, _ := open(t, t.TempDir())
	put(t, s, "specs", "Ab", "upper", nil, nil)
	put(t, s, "specs", "ab", "lower", nil, nil)
	put(t, s, "specs", "fix..login", "dots", nil, nil)
	put(t, s, "specs", "k:x@y", "colon", nil, nil)
	long := strings.Repeat("A", 200)
	put(t, s, "specs", long, "long", nil, nil)
	for key, want := range map[string]string{"Ab": `"upper"`, "ab": `"lower"`, "fix..login": `"dots"`, "k:x@y": `"colon"`, long: `"long"`} {
		got, err := s.Get(ctx(t), protocol.StateGetParams{Collection: "specs", Key: key})
		if err != nil || string(got.Value) != want {
			t.Errorf("%s: %s %v, want %s", key[:min(len(key), 12)], got.Value, err, want)
		}
	}
	l, _ := s.List(ctx(t), protocol.StateListParams{Collection: "specs"})
	if len(l.Items) != 5 {
		t.Fatalf("list %d items, want 5", len(l.Items))
	}
}

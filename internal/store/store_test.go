package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
		{"cli", noEnv, "/Users/me/Library/Application Support/Spec Studio/backend/json"},
		{"cli", withSupport, "/tmp/test-support/backend/json"},
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

// The studio CLI (host kind cli) uses the app's data folder, not a
// temporary one: its writes stay after Close.
func TestOpenCLIUsesAppFolder(t *testing.T) {
	support := t.TempDir()
	s := New()
	s.getenv = func(k string) string {
		if k == "SPEC_STUDIO_SUPPORT" {
			return support
		}
		return ""
	}
	if err := s.Open(ctx(t), plugin.OpenOptions{Settings: map[string]string{}, Host: protocol.Host{Kind: "cli"}, Log: slog.New(slog.DiscardHandler)}); err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(support, "backend", "json")
	if s.Dir() != want {
		t.Fatalf("dir %s, want %s", s.Dir(), want)
	}
	put(t, s, "specs", "x", "kept", nil, nil)
	s.Close(context.Background())
	if _, err := os.Stat(files.DocPath(filepath.Join(want, "state"), "specs", "x")); err != nil {
		t.Fatalf("the write of the cli is gone after Close: %v", err)
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

// A long key and a key that is the long key's hashed file name are two
// documents (review round 1).
func TestHashedNameNoCollision(t *testing.T) {
	s, _ := open(t, t.TempDir())
	long := strings.Repeat("A", 200)
	hashed := files.FileName(long)
	if !strings.HasPrefix(hashed, "%h") {
		t.Fatalf("hashed name %q", hashed)
	}
	// The other key that looks most like the hashed form: "h" + the same hex.
	other := strings.TrimPrefix(hashed, "%")
	put(t, s, "specs", long, "long", nil, nil)
	put(t, s, "specs", other, "other", nil, nil)
	for key, want := range map[string]string{long: `"long"`, other: `"other"`} {
		got, err := s.Get(ctx(t), protocol.StateGetParams{Collection: "specs", Key: key})
		if err != nil || string(got.Value) != want {
			t.Fatalf("%s: %s %v", key[:8], got.Value, err)
		}
	}
}

// A failed Open for a test host does not leave a temporary folder to remove
// at a later Close of a real folder (review round 1).
func TestFailedTestOpenThenRealOpen(t *testing.T) {
	s := New()
	// The state that a test-host Open leaves when it fails after it made its
	// temporary folder.
	s.temp, s.dir = true, t.TempDir()
	real := t.TempDir()
	os.WriteFile(filepath.Join(real, "keep"), []byte("x"), 0o600)
	if err := s.Open(ctx(t), plugin.OpenOptions{Settings: map[string]string{SettingDataDir: real}, Host: protocol.Host{Kind: "relay"}, Log: slog.New(slog.DiscardHandler)}); err != nil {
		t.Fatal(err)
	}
	s.Close(context.Background())
	if _, err := os.Stat(filepath.Join(real, "keep")); err != nil {
		t.Fatalf("Close removed the real data folder: %v", err)
	}
}

// readLayout reads layout.json of a data folder.
func readLayout(t *testing.T, dir string) Layout {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, "layout.json"))
	if err != nil {
		t.Fatal(err)
	}
	var l Layout
	if err := json.Unmarshal(data, &l); err != nil {
		t.Fatal(err)
	}
	return l
}

// A new folder gets the layout version of this plugin.
func TestNewFolderLayoutVersion(t *testing.T) {
	dir := t.TempDir()
	open(t, dir)
	if l := readLayout(t, dir); l.Layout != LayoutName || l.Version != 2 {
		t.Fatalf("layout.json %+v, want plugin-json version 2", l)
	}
}

// A folder at layout version 1 (the data of an older plugin-json: documents
// with no "commit" field, no events) opens, keeps its documents, and gets
// version 2. A commit on an old document then works, after a restart too.
func TestLayoutUpgradeFromV1(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "layout.json"), []byte(`{"layout":"plugin-json","version":1}`), 0o600)
	old, err := files.WriteDoc(filepath.Join(dir, "state"), "app-specs", "r1:old", raw("old"), map[string]any{"open": true}, nil)
	if err != nil {
		t.Fatal(err)
	}
	s, log := open(t, dir)
	if l := readLayout(t, dir); l.Version != LayoutVersion {
		t.Fatalf("layout.json %+v after the upgrade, want version %d", l, LayoutVersion)
	}
	if !strings.Contains(log.String(), "layout_upgraded") {
		t.Fatalf("no log line for the upgrade: %s", log.String())
	}
	g := get(t, s, "r1:old")
	if !g.Exists || g.Version != old.Version || string(g.Value) != `"old"` || g.Attributes["open"] != true {
		t.Fatalf("the old document after the upgrade: %+v", g)
	}
	c := commit(t, s, "r1:old", "new", &old.Version, "spec:r1:old", "A", "B")
	s.Close(ctx(t))

	s, log = open(t, dir)
	if strings.Contains(log.String(), "layout_upgraded") {
		t.Fatalf("a second upgrade: %s", log.String())
	}
	if g := get(t, s, "r1:old"); g.Version != c.Version || string(g.Value) != `"new"` {
		t.Fatalf("the document after a restart: %+v", g)
	}
	if got := types(readAll(t, s, "spec:r1:old")); got != "A,B" {
		t.Fatalf("the stream after a restart: %s", got)
	}
}

// A folder with a newer layout version than this plugin's is refused, and
// the plugin changes nothing in it. A plugin-json that knows only version 1
// refuses a version 2 folder by the same rule.
func TestNewerLayoutRefused(t *testing.T) {
	dir := t.TempDir()
	newer := fmt.Sprintf(`{"layout":"plugin-json","version":%d}`, LayoutVersion+1)
	os.WriteFile(filepath.Join(dir, "layout.json"), []byte(newer), 0o600)
	err := New().Open(ctx(t), plugin.OpenOptions{Settings: map[string]string{SettingDataDir: dir}, Host: protocol.Host{Kind: "relay"}, Log: slog.New(slog.DiscardHandler)})
	if !protocol.Is(err, protocol.Unavailable) || !strings.Contains(err.Error(), "Update plugin-json") {
		t.Fatalf("error %v, want unavailable with Update plugin-json", err)
	}
	if data, _ := os.ReadFile(filepath.Join(dir, "layout.json")); string(data) != newer {
		t.Fatalf("layout.json changed: %s", data)
	}
}

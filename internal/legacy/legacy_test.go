package legacy_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/spec-studio-app/plugin-json/internal/legacy"
	"github.com/spec-studio-app/plugin-json/internal/store"
	"github.com/spec-studio-app/plugin-sdk/go/plugin"
	"github.com/spec-studio-app/plugin-sdk/go/protocol"
)

type logBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *logBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *logBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// TestMain gives the tests an environment with no DEPLOY_ID, so a value on
// the test computer does not change them.
func TestMain(m *testing.M) {
	legacy.Getenv = func(string) string { return "" }
	os.Exit(m.Run())
}

func ctx(t *testing.T) context.Context {
	c, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	return c
}

func openStore(dir string) (*store.Store, *logBuffer, error) {
	s := store.New()
	log := &logBuffer{}
	err := s.Open(context.Background(), plugin.OpenOptions{
		Settings: map[string]string{store.SettingDataDir: dir},
		Host:     protocol.Host{Kind: "relay", Version: "0.6.0"},
		Log:      slog.New(slog.NewTextHandler(log, nil)),
	})
	return s, log, err
}

func mustOpen(t *testing.T, dir string) (*store.Store, *logBuffer) {
	t.Helper()
	s, log, err := openStore(dir)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { s.Close(context.Background()) })
	return s, log
}

// copyFixture copies testdata/relay-v1 to a new folder.
func copyFixture(t *testing.T) string {
	t.Helper()
	src := filepath.Join("..", "..", "testdata", "relay-v1")
	dst := t.TempDir()
	err := filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		if d.IsDir() {
			return os.MkdirAll(filepath.Join(dst, rel), 0o700)
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(dst, rel), data, 0o600)
	})
	if err != nil {
		t.Fatal(err)
	}
	return dst
}

type expected struct {
	Documents []struct {
		Collection string          `json:"collection"`
		Key        string          `json:"key"`
		Value      json.RawMessage `json:"value"`
		Attributes map[string]any  `json:"attributes"`
	} `json:"documents"`
}

func loadExpected(t *testing.T) expected {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "testdata", "relay-v1.expected.json"))
	if err != nil {
		t.Fatal(err)
	}
	var e expected
	if err := json.Unmarshal(data, &e); err != nil {
		t.Fatal(err)
	}
	return e
}

func jsonEqual(a, b []byte) bool {
	var x, y any
	if json.Unmarshal(a, &x) != nil || json.Unmarshal(b, &y) != nil {
		return false
	}
	return reflect.DeepEqual(x, y)
}

// R6.S1
func TestRelayFixture(t *testing.T) {
	dir := copyFixture(t)
	s, _ := mustOpen(t, dir)
	e := loadExpected(t)
	if len(e.Documents) < 7 {
		t.Fatalf("the fixture has %d documents", len(e.Documents))
	}
	for _, d := range e.Documents {
		got, err := s.Get(ctx(t), protocol.StateGetParams{Collection: d.Collection, Key: d.Key})
		if err != nil || !got.Exists {
			t.Errorf("%s / %s: %+v %v", d.Collection, d.Key, got, err)
			continue
		}
		if !jsonEqual(got.Value, d.Value) {
			t.Errorf("%s / %s: the value is not the old file's value\n got %s\nwant %s", d.Collection, d.Key, got.Value, d.Value)
		}
	}
	// No other document.
	for _, c := range []string{"relay", "specs", "desks", "closed-specs"} {
		l, err := s.List(ctx(t), protocol.StateListParams{Collection: c})
		if err != nil {
			t.Fatal(err)
		}
		n := 0
		for _, d := range e.Documents {
			if d.Collection == c {
				n++
			}
		}
		if len(l.Items) != n {
			t.Errorf("%s: %d documents, want %d", c, len(l.Items), n)
		}
	}
}

// R6.S2
func TestRelayFixtureAttributes(t *testing.T) {
	dir := copyFixture(t)
	s, _ := mustOpen(t, dir)
	e := loadExpected(t)
	var wantPending []string
	for _, d := range e.Documents {
		got, _ := s.Get(ctx(t), protocol.StateGetParams{Collection: d.Collection, Key: d.Key})
		if !reflect.DeepEqual(map[string]any(got.Attributes), d.Attributes) {
			t.Errorf("%s / %s: attributes %v, want %v", d.Collection, d.Key, got.Attributes, d.Attributes)
		}
		if d.Collection == "specs" && d.Attributes["pendingForMac"] == true && d.Attributes["open"] == true {
			wantPending = append(wantPending, d.Key)
		}
	}
	if len(wantPending) == 0 {
		t.Fatal("the fixture has no open spec with a message for the Mac")
	}
	l, err := s.List(ctx(t), protocol.StateListParams{Collection: "specs", Filter: map[string]any{"pendingForMac": true, "open": true}})
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, it := range l.Items {
		got = append(got, it.Key)
	}
	sort.Strings(wantPending)
	if !reflect.DeepEqual(got, wantPending) {
		t.Fatalf("list %v, want %v", got, wantPending)
	}
}

func treeHash(t *testing.T, dir string, skip func(rel string) bool) map[string]string {
	t.Helper()
	out := map[string]string{}
	filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		rel, _ := filepath.Rel(dir, p)
		if skip != nil && skip(rel) {
			return nil
		}
		f, err := os.Open(p)
		if err != nil {
			return nil
		}
		defer f.Close()
		h := sha256.New()
		io.Copy(h, f)
		out[rel] = hex.EncodeToString(h.Sum(nil))
		return nil
	})
	return out
}

// R6.S3
func TestBackupAndLog(t *testing.T) {
	dir := copyFixture(t)
	before := treeHash(t, dir, nil)
	_, log := mustOpen(t, dir)
	backups, _ := filepath.Glob(filepath.Join(dir, legacy.BackupPrefix+"*"))
	if len(backups) != 1 || strings.HasSuffix(backups[0], ".partial") {
		t.Fatalf("backups %v", backups)
	}
	if got := treeHash(t, backups[0], nil); !reflect.DeepEqual(got, before) {
		t.Fatalf("the backup is not the old files:\n got %v\nwant %v", got, before)
	}
	for _, old := range []string{"config.json", "pair-code.json", "closed.json", "specs", "desks"} {
		if _, err := os.Stat(filepath.Join(dir, old)); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("the old %s stays", old)
		}
	}
	lines := strings.Count(log.String(), "msg=legacy_import")
	if lines != 1 {
		t.Fatalf("%d legacy_import lines:\n%s", lines, log.String())
	}
	for _, want := range []string{"specs=3", "desks=2", "closedSpecs=1", "configs=1", "backup=" + filepath.Base(backups[0])} {
		if !strings.Contains(log.String(), want) {
			t.Errorf("the log has no %s:\n%s", want, log.String())
		}
	}
}

// R6.S4
func TestNoSecondImport(t *testing.T) {
	dir := copyFixture(t)
	s, _ := mustOpen(t, dir)
	s.Close(context.Background())
	skipLocks := func(rel string) bool { return strings.HasSuffix(rel, ".lock") }
	before := treeHash(t, dir, skipLocks)
	_, log := mustOpen(t, dir)
	if strings.Contains(log.String(), "legacy_import") {
		t.Fatalf("a second import:\n%s", log.String())
	}
	if after := treeHash(t, dir, skipLocks); !reflect.DeepEqual(after, before) {
		t.Fatal("the second start changed a file")
	}
}

// R6.S5
func TestCrashDuringImport(t *testing.T) {
	dir := copyFixture(t)
	legacy.AfterState = func() error { return errors.New("crash") }
	_, _, err := openStore(dir)
	legacy.AfterState = nil
	if err == nil {
		t.Fatal("the crash hook did not stop the import")
	}
	if _, err := os.Stat(filepath.Join(dir, "layout.json")); err == nil {
		t.Fatal("layout.json after a crash")
	}
	s, log := mustOpen(t, dir)
	if !strings.Contains(log.String(), "legacy_import") {
		t.Fatal("no import after the crash")
	}
	e := loadExpected(t)
	for _, c := range []string{"specs", "desks", "closed-specs", "relay"} {
		l, _ := s.List(ctx(t), protocol.StateListParams{Collection: c})
		seen := map[string]bool{}
		for _, it := range l.Items {
			if seen[it.Key] {
				t.Errorf("%s / %s twice", c, it.Key)
			}
			seen[it.Key] = true
		}
		n := 0
		for _, d := range e.Documents {
			if d.Collection == c {
				n++
			}
		}
		if len(seen) != n {
			t.Errorf("%s: %d documents, want %d", c, len(seen), n)
		}
	}
	if p, _ := filepath.Glob(filepath.Join(dir, "*.partial")); len(p) != 0 {
		t.Fatalf("partial backups stay: %v", p)
	}
}

// R6.S6
func TestBadOldFile(t *testing.T) {
	dir := copyFixture(t)
	specs, _ := filepath.Glob(filepath.Join(dir, "specs", "*.json"))
	bad := specs[0]
	os.WriteFile(bad, []byte("{not json"), 0o600)
	before := treeHash(t, dir, nil)
	_, _, err := openStore(dir)
	if !protocol.Is(err, protocol.Unavailable) {
		t.Fatalf("error %v, want unavailable", err)
	}
	if !strings.Contains(err.Error(), "specs/"+filepath.Base(bad)) || !strings.Contains(err.Error(), "Correct the file, or remove it") {
		t.Fatalf("message %q", err.Error())
	}
	after := treeHash(t, dir, func(rel string) bool { return rel == ".lock" || strings.HasPrefix(rel, ".probe") })
	if !reflect.DeepEqual(after, before) {
		t.Fatalf("the failed import changed files:\nbefore %v\nafter %v", before, after)
	}
}

// R7.S1
func TestImportLogHasNoSecret(t *testing.T) {
	dir := copyFixture(t)
	var secrets []string
	var collect func(v any)
	collect = func(v any) {
		switch x := v.(type) {
		case map[string]any:
			for k, val := range x {
				if s, ok := val.(string); ok && (k == "hash" || strings.HasSuffix(k, "Hash") || k == "client_id") {
					secrets = append(secrets, s)
				}
				collect(val)
			}
		case []any:
			for _, val := range x {
				collect(val)
			}
		}
	}
	for _, f := range []string{"config.json", "pair-code.json"} {
		data, _ := os.ReadFile(filepath.Join(dir, f))
		var v any
		json.Unmarshal(data, &v)
		collect(v)
	}
	// The token hashes are keys of the token maps.
	data, _ := os.ReadFile(filepath.Join(dir, "config.json"))
	for _, h := range []string{strings.Repeat("a", 64), strings.Repeat("b", 64), strings.Repeat("c", 64)} {
		if strings.Contains(string(data), h[:16]) {
			secrets = append(secrets, h[:16])
		}
	}
	if len(secrets) < 3 {
		t.Fatalf("the fixture has too few secrets: %d", len(secrets))
	}
	_, log := mustOpen(t, dir)
	for _, s := range secrets {
		if s != "" && strings.Contains(log.String(), s) {
			t.Fatalf("the log has a secret value:\n%s", log.String())
		}
	}
}

// A new folder gets layout.json and no import.
func TestNewFolder(t *testing.T) {
	dir := t.TempDir()
	_, log := mustOpen(t, dir)
	data, err := os.ReadFile(filepath.Join(dir, "layout.json"))
	if err != nil || !strings.Contains(string(data), `"plugin-json"`) {
		t.Fatalf("layout.json: %s %v", data, err)
	}
	if strings.Contains(log.String(), "legacy") {
		t.Fatalf("log:\n%s", log.String())
	}
}

// Old files next to the plugin layout are ignored, with one log line.
func TestOldFilesIgnored(t *testing.T) {
	dir := t.TempDir()
	s, _ := mustOpen(t, dir)
	s.Close(context.Background())
	os.WriteFile(filepath.Join(dir, "config.json"), []byte(`{"version":1}`), 0o600)
	_, log := mustOpen(t, dir)
	if strings.Count(log.String(), "legacy_files_ignored") != 1 || strings.Contains(log.String(), "legacy_import") {
		t.Fatalf("log:\n%s", log.String())
	}
}

// The import keeps only the spec and desk files of the config's deploy ID,
// as the relay loads them; the others stay in the backup (review round 1).
func TestOtherDeployIDSkipped(t *testing.T) {
	dir := copyFixture(t)
	specs, _ := filepath.Glob(filepath.Join(dir, "specs", "*.json"))
	data, _ := os.ReadFile(specs[0])
	var f map[string]any
	json.Unmarshal(data, &f)
	state := f["state"].(map[string]any)
	spec := state["spec"].(map[string]any)
	key := spec["repoKey"].(string) + ":" + spec["slug"].(string)
	state["deployId"] = "old-deploy"
	state["line"] = 999.0
	f["lastActivityAt"] = 9e12
	other, _ := json.Marshal(f)
	os.WriteFile(filepath.Join(dir, "specs", "0000other.json"), other, 0o600)
	s, log := mustOpen(t, dir)
	got, err := s.Get(ctx(t), protocol.StateGetParams{Collection: "specs", Key: key})
	if err != nil || !got.Exists || got.Attributes["deployId"] != "default" || strings.Contains(string(got.Value), "old-deploy") {
		t.Fatalf("%s: %v %v", key, got.Attributes, err)
	}
	if !strings.Contains(log.String(), "skipped=1") {
		t.Fatalf("log:\n%s", log.String())
	}
	backups, _ := filepath.Glob(filepath.Join(dir, legacy.BackupPrefix+"*", "specs", "0000other.json"))
	if len(backups) != 1 {
		t.Fatal("the skipped file is not in the backup")
	}
}

// A pair-code.json that does not parse counts as none, as in the relay.
func TestBadPairCodeIgnored(t *testing.T) {
	dir := copyFixture(t)
	os.WriteFile(filepath.Join(dir, "pair-code.json"), []byte("{bad"), 0o600)
	s, log := mustOpen(t, dir)
	if !strings.Contains(log.String(), "legacy_import") {
		t.Fatal("no import")
	}
	got, _ := s.Get(ctx(t), protocol.StateGetParams{Collection: "relay", Key: "config"})
	var v struct {
		State struct {
			PairCode any `json:"pairCode"`
		} `json:"state"`
	}
	json.Unmarshal(got.Value, &v)
	if v.State.PairCode != nil {
		t.Fatalf("the bad pair code was applied: %v", v.State.PairCode)
	}
}

// The relay's DEPLOY_ID wins over the deploy ID of config.json: after a
// change of DEPLOY_ID, the relay's live files are the ones of the new ID
// (review round 2).
func TestDeployIDFromEnvironment(t *testing.T) {
	dir := copyFixture(t)
	specs, _ := filepath.Glob(filepath.Join(dir, "specs", "*.json"))
	for _, p := range specs {
		data, _ := os.ReadFile(p)
		os.WriteFile(p, []byte(strings.ReplaceAll(string(data), `"deployId":"default"`, `"deployId":"new-id"`)), 0o600)
	}
	legacy.Getenv = func(k string) string {
		if k == "DEPLOY_ID" {
			return "new-id"
		}
		return ""
	}
	t.Cleanup(func() { legacy.Getenv = func(string) string { return "" } })
	s, _ := mustOpen(t, dir)
	l, err := s.List(ctx(t), protocol.StateListParams{Collection: "specs"})
	if err != nil || len(l.Items) != len(specs) {
		t.Fatalf("specs %d (%v), want %d", len(l.Items), err, len(specs))
	}
	// The desks keep the old ID, so the relay with the new ID has none.
	d, _ := s.List(ctx(t), protocol.StateListParams{Collection: "desks"})
	if len(d.Items) != 0 {
		t.Fatalf("desks %d, want 0", len(d.Items))
	}
}

// A pair-code.json that the plugin cannot read stops the import and names
// the file, as the relay stops (review round 2).
func TestUnreadablePairCode(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads a file with mode 0")
	}
	dir := copyFixture(t)
	p := filepath.Join(dir, "pair-code.json")
	os.Chmod(p, 0)
	t.Cleanup(func() { os.Chmod(p, 0o600) })
	_, _, err := openStore(dir)
	if !protocol.Is(err, protocol.Unavailable) || !strings.Contains(err.Error(), "pair-code.json") {
		t.Fatalf("error %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "layout.json")); err == nil {
		t.Fatal("the import went on")
	}
}

// The deploy ID follows the relay's rule: DEPLOY_ID trimmed, or "default"
// (review round 3).
func TestRelayDeployIDRule(t *testing.T) {
	t.Cleanup(func() { legacy.Getenv = func(string) string { return "" } })
	for env, want := range map[string]string{"": "default", "   ": "default", " prod ": "prod", "new-id": "new-id"} {
		legacy.Getenv = func(k string) string {
			if k == "DEPLOY_ID" {
				return env
			}
			return ""
		}
		if got := legacy.RelayDeployID(); got != want {
			t.Errorf("DEPLOY_ID %q: %q, want %q", env, got, want)
		}
	}
}

// An old file that the plugin cannot read stops the import and names the
// file, before any change (review round 3).
func TestUnreadableClosed(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads a file with mode 0")
	}
	dir := copyFixture(t)
	p := filepath.Join(dir, "closed.json")
	os.Chmod(p, 0)
	t.Cleanup(func() { os.Chmod(p, 0o600) })
	_, _, err := openStore(dir)
	if !protocol.Is(err, protocol.Unavailable) || !strings.Contains(err.Error(), "closed.json") {
		t.Fatalf("error %v", err)
	}
	if b, _ := filepath.Glob(filepath.Join(dir, legacy.BackupPrefix+"*")); len(b) != 0 {
		t.Fatalf("the import made a backup: %v", b)
	}
}

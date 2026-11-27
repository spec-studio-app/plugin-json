package store

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/spec-studio-app/plugin-sdk/go/protocol"
)

// R1.S1
func TestPutRestartGet(t *testing.T) {
	dir := t.TempDir()
	s, _ := open(t, dir)
	r := put(t, s, "specs", "1a2b3c4d5e6f:drop-spec", map[string]any{"a": 1}, map[string]any{"open": true}, nil)
	s.Close(ctx(t))

	s2, _ := open(t, dir)
	got, err := s2.Get(ctx(t), protocol.StateGetParams{Collection: "specs", Key: "1a2b3c4d5e6f:drop-spec"})
	if err != nil {
		t.Fatal(err)
	}
	if !got.Exists || string(got.Value) != `{"a":1}` || got.Version != r.Version || got.Attributes["open"] != true {
		t.Fatalf("after a restart: %+v", got)
	}
	if !got.UpdatedAt.Equal(r.UpdatedAt) {
		t.Fatalf("updatedAt %v, want %v", got.UpdatedAt, r.UpdatedAt)
	}
}

// R1.S2
func TestGetMissing(t *testing.T) {
	s, _ := open(t, t.TempDir())
	got, err := s.Get(ctx(t), protocol.StateGetParams{Collection: "desks", Key: "none"})
	if err != nil || got.Exists {
		t.Fatalf("got %+v, %v; want exists false", got, err)
	}
	b, _ := json.Marshal(got)
	if string(b) != `{"exists":false}` {
		t.Fatalf("result %s", b)
	}
}

// R1.S3
func TestListFilterPages(t *testing.T) {
	s, _ := open(t, t.TempDir())
	put(t, s, "specs", "b", 1, map[string]any{"open": true}, nil)
	put(t, s, "specs", "a", 2, map[string]any{"open": true}, nil)
	put(t, s, "specs", "c", 3, map[string]any{"open": false}, nil)
	put(t, s, "specs", "d", 4, nil, nil)

	p1, err := s.List(ctx(t), protocol.StateListParams{Collection: "specs", Filter: map[string]any{"open": true}, PageSize: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(p1.Items) != 1 || p1.Items[0].Key != "a" || p1.Cursor == nil {
		t.Fatalf("page 1: %+v", p1)
	}
	p2, err := s.List(ctx(t), protocol.StateListParams{Collection: "specs", Filter: map[string]any{"open": true}, PageSize: 1, Cursor: p1.Cursor})
	if err != nil {
		t.Fatal(err)
	}
	if len(p2.Items) != 1 || p2.Items[0].Key != "b" || p2.Cursor != nil {
		t.Fatalf("page 2: %+v", p2)
	}
	// A missing attribute is null.
	nul, _ := s.List(ctx(t), protocol.StateListParams{Collection: "specs", Filter: map[string]any{"open": nil}, WithValues: true})
	if len(nul.Items) != 1 || nul.Items[0].Key != "d" || string(nul.Items[0].Value) != "4" {
		t.Fatalf("null filter: %+v", nul)
	}
	// Key order is byte order: "B" is before "a".
	put(t, s, "order", "a", 1, nil, nil)
	put(t, s, "order", "B", 1, nil, nil)
	o, _ := s.List(ctx(t), protocol.StateListParams{Collection: "order"})
	if len(o.Items) != 2 || o.Items[0].Key != "B" {
		t.Fatalf("order: %+v", o.Items)
	}
}

// R2.S1
func TestConcurrentPut(t *testing.T) {
	s, _ := open(t, t.TempDir())
	v := put(t, s, "relay", "config", 0, nil, nil).Version
	var wg sync.WaitGroup
	var mu sync.Mutex
	won, conflicts := 0, 0
	var newVersion string
	var stored []string
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			r, err := s.Put(ctx(t), protocol.StatePutParams{Collection: "relay", Key: "config", Value: raw(i), ExpectedVersion: str(v)})
			mu.Lock()
			defer mu.Unlock()
			if err == nil {
				won++
				newVersion = r.Version
				return
			}
			var e *protocol.Error
			if pe, ok := err.(*protocol.Error); ok {
				e = pe
			}
			if e == nil || e.Code() != protocol.Conflict || e.Data.Version == nil {
				t.Errorf("error %v, want conflict with a version", err)
				return
			}
			conflicts++
			stored = append(stored, *e.Data.Version)
		}(i)
	}
	wg.Wait()
	if won != 1 || conflicts != 19 {
		t.Fatalf("won %d, conflicts %d; want 1 and 19", won, conflicts)
	}
	for _, sv := range stored {
		if sv != newVersion {
			t.Fatalf("a conflict gave the version %s, want the new version %s", sv, newVersion)
		}
	}
}

// R2.S2: the test binary runs itself twice as a helper process.
func TestTwoProcesses(t *testing.T) {
	if os.Getenv("PLUGIN_JSON_HELPER") == "put" {
		return
	}
	dir := t.TempDir()
	s, _ := open(t, dir)
	v := put(t, s, "relay", "config", 0, nil, nil).Version
	s.Close(ctx(t))

	start := filepath.Join(dir, "go")
	var cmds []*exec.Cmd
	var outs []*strings.Builder
	for i := 0; i < 2; i++ {
		cmd := exec.Command(os.Args[0], "-test.run=^TestHelperPut$")
		cmd.Env = append(os.Environ(), "PLUGIN_JSON_HELPER=put", "HELPER_DIR="+dir, "HELPER_VERSION="+v, "HELPER_START="+start, fmt.Sprintf("HELPER_VALUE=%d", i))
		out := &strings.Builder{}
		cmd.Stdout, cmd.Stderr = out, out
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		cmds = append(cmds, cmd)
		outs = append(outs, out)
	}
	time.Sleep(300 * time.Millisecond)
	os.WriteFile(start, []byte("go"), 0o600)
	won, conflicts := 0, 0
	for i, cmd := range cmds {
		cmd.Wait()
		switch {
		case strings.Contains(outs[i].String(), "RESULT ok"):
			won++
		case strings.Contains(outs[i].String(), "RESULT conflict"):
			conflicts++
		default:
			t.Fatalf("helper %d: %s", i, outs[i].String())
		}
	}
	if won != 1 || conflicts != 1 {
		t.Fatalf("won %d, conflicts %d; want 1 and 1", won, conflicts)
	}
}

func TestHelperPut(t *testing.T) {
	if os.Getenv("PLUGIN_JSON_HELPER") != "put" {
		t.Skip("a helper of TestTwoProcesses")
	}
	s, _ := open(t, os.Getenv("HELPER_DIR"))
	for {
		if _, err := os.Stat(os.Getenv("HELPER_START")); err == nil {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	_, err := s.Put(ctx(t), protocol.StatePutParams{Collection: "relay", Key: "config", Value: raw(os.Getenv("HELPER_VALUE")), ExpectedVersion: str(os.Getenv("HELPER_VERSION"))})
	switch {
	case err == nil:
		fmt.Println("RESULT ok")
	case protocol.Is(err, protocol.Conflict):
		fmt.Println("RESULT conflict")
	default:
		fmt.Println("RESULT error", err)
	}
}

// R2.S4
func TestVersionNotReused(t *testing.T) {
	s, _ := open(t, t.TempDir())
	seen := map[string]bool{}
	for i := 0; i < 100; i++ {
		r := put(t, s, "specs", "x", i, nil, nil)
		if seen[r.Version] {
			t.Fatalf("the version %s came again", r.Version)
		}
		seen[r.Version] = true
		if _, err := s.Delete(ctx(t), protocol.StateDeleteParams{Collection: "specs", Key: "x", ExpectedVersion: r.Version}); err != nil {
			t.Fatal(err)
		}
	}
}

func TestDelete(t *testing.T) {
	s, _ := open(t, t.TempDir())
	_, err := s.Delete(ctx(t), protocol.StateDeleteParams{Collection: "specs", Key: "x", ExpectedVersion: "v"})
	if !protocol.Is(err, protocol.NotFound) {
		t.Fatalf("delete of a missing document: %v, want not_found", err)
	}
	r := put(t, s, "specs", "x", 1, nil, nil)
	if _, err := s.Delete(ctx(t), protocol.StateDeleteParams{Collection: "specs", Key: "x", ExpectedVersion: "old"}); !protocol.Is(err, protocol.Conflict) {
		t.Fatalf("delete with a wrong version: %v, want conflict", err)
	}
	if _, err := s.Delete(ctx(t), protocol.StateDeleteParams{Collection: "specs", Key: "x", ExpectedVersion: r.Version}); err != nil {
		t.Fatal(err)
	}
	got, _ := s.Get(ctx(t), protocol.StateGetParams{Collection: "specs", Key: "x"})
	if got.Exists {
		t.Fatal("the document is still there")
	}
}

// R7.S2
func TestErrorHasNoValue(t *testing.T) {
	const canary = "canary-value-7f3a"
	c, stderr := serve(t, t.TempDir())
	r, err := c.Put(ctx(t), protocol.StatePutParams{Collection: "relay", Key: "config", Value: raw(map[string]string{"tokenHash": canary})})
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.Put(ctx(t), protocol.StatePutParams{Collection: "relay", Key: "config", Value: raw(map[string]string{"tokenHash": canary}), ExpectedVersion: str("stale")})
	if !protocol.Is(err, protocol.Conflict) || strings.Contains(err.Error(), canary) {
		t.Fatalf("conflict: %v", err)
	}
	_, err = c.Put(ctx(t), protocol.StatePutParams{Collection: "relay", Key: "../config", Value: raw(canary), ExpectedVersion: &r.Version})
	if !protocol.Is(err, protocol.Refused) || strings.Contains(err.Error(), canary) {
		t.Fatalf("refused: %v", err)
	}
	if strings.Contains(stderr.String(), canary) {
		t.Fatalf("the log has the value:\n%s", stderr.String())
	}
}

package store

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/spec-studio-app/plugin-sdk/go/protocol"
)

func commit(t *testing.T, s *Store, key string, value any, expected *string, stream string, types ...string) protocol.StateCommitResult {
	t.Helper()
	r, err := s.Commit(ctx(t), commitParams(key, value, expected, stream, types...))
	if err != nil {
		t.Fatalf("commit %s: %v", key, err)
	}
	return r
}

func commitParams(key string, value any, expected *string, stream string, types ...string) protocol.StateCommitParams {
	events := []protocol.NewEvent{}
	for i, ty := range types {
		events = append(events, protocol.NewEvent{Type: ty, Data: raw(i)})
	}
	return protocol.StateCommitParams{Collection: "app-specs", Key: key, Value: raw(value), Attributes: map[string]any{"stage": "build"}, ExpectedVersion: expected, Stream: stream, Events: events}
}

func get(t *testing.T, s *Store, key string) protocol.StateGetResult {
	t.Helper()
	g, err := s.Get(ctx(t), protocol.StateGetParams{Collection: "app-specs", Key: key})
	if err != nil {
		t.Fatal(err)
	}
	return g
}

// lines counts the lines of the stream file on the disk.
func lines(t *testing.T, s *Store, stream string) int {
	t.Helper()
	data, err := os.ReadFile(s.streamPath(stream))
	if errors.Is(err, os.ErrNotExist) {
		return 0
	}
	if err != nil {
		t.Fatal(err)
	}
	return bytes.Count(data, []byte("\n"))
}

// R2.S1: a commit writes the document and the events, and the document
// file records the stream's length.
func TestCommitWritesBoth(t *testing.T) {
	s, _ := open(t, t.TempDir())
	const key, stream = "r1:drop-spec", "spec:r1:drop-spec"
	appendN(t, s, stream, "before")
	v1 := commit(t, s, key, "one", nil, stream, "A")
	r := commit(t, s, key, "two", &v1.Version, stream, "B", "C")
	if strings.Join(r.Cursors, ",") != "3,4" || r.Version == v1.Version || r.UpdatedAt.IsZero() {
		t.Fatalf("commit result %+v", r)
	}
	g := get(t, s, key)
	if g.Version != r.Version || string(g.Value) != `"two"` || g.Attributes["stage"] != "build" {
		t.Fatalf("document %+v", g)
	}
	if got := types(readAll(t, s, stream)); got != "before,A,B,C" {
		t.Fatalf("stream %s", got)
	}
	d, _ := s.readDoc("app-specs", key)
	if d.Commit == nil || d.Commit.Stream != stream || d.Commit.Length != 4 {
		t.Fatalf("the document records %+v, want the stream with length 4", d.Commit)
	}
	// No events: a new version and no cursor.
	r2 := commit(t, s, key, "three", &r.Version, stream)
	if len(r2.Cursors) != 0 || r2.Version == r.Version {
		t.Fatalf("a commit with no events: %+v", r2)
	}
}

// R2.S2: a stale version writes neither the document nor the events.
func TestCommitStaleWritesNeither(t *testing.T) {
	s, _ := open(t, t.TempDir())
	const key, stream = "r1:drop-spec", "spec:r1:drop-spec"
	v1 := commit(t, s, key, "one", nil, stream, "A")
	v2 := commit(t, s, key, "two", &v1.Version, stream, "B")

	_, err := s.Commit(ctx(t), commitParams(key, "three", &v1.Version, stream, "C", "D"))
	var pe *protocol.Error
	if !errors.As(err, &pe) || pe.Code() != protocol.Conflict || pe.Data.Version == nil || *pe.Data.Version != v2.Version {
		t.Fatalf("stale commit: %v, want conflict with %s", err, v2.Version)
	}
	if strings.Contains(err.Error(), "three") {
		t.Fatalf("the message has the value: %v", err)
	}
	_, err = s.Commit(ctx(t), commitParams(key, "four", nil, stream, "E"))
	if !protocol.Is(err, protocol.Conflict) {
		t.Fatalf("commit with null on a document that exists: %v", err)
	}
	_, err = s.Commit(ctx(t), commitParams("r1:none", 1, &v1.Version, "spec:r1:none", "F"))
	if !errors.As(err, &pe) || pe.Code() != protocol.Conflict || pe.Data.Version != nil {
		t.Fatalf("commit with a version on a missing document: %v", err)
	}
	if g := get(t, s, key); g.Version != v2.Version || string(g.Value) != `"two"` {
		t.Fatalf("the document changed: %+v", g)
	}
	if got := types(readAll(t, s, stream)); got != "A,B" || lines(t, s, stream) != 2 {
		t.Fatalf("the stream changed: %s", got)
	}
	if g := get(t, s, "r1:none"); g.Exists {
		t.Fatal("the failed commit made the document")
	}
	if n := lines(t, s, "spec:r1:none"); n != 0 {
		t.Fatalf("the failed commit appended %d events", n)
	}
}

func crashNextCommit(t *testing.T) {
	afterCommitAppend = func() error { return errors.New("crash") }
	t.Cleanup(func() { afterCommitAppend = nil })
}

// R2.S2: a crash between the append and the rename leaves the old document
// and no new event: the next start cuts the stream back.
func TestCommitCrashBetweenAppendAndRename(t *testing.T) {
	dir := t.TempDir()
	s, _ := open(t, dir)
	const key, stream = "r1:drop-spec", "spec:r1:drop-spec"
	v1 := commit(t, s, key, "one", nil, stream, "A")

	crashNextCommit(t)
	if _, err := s.Commit(ctx(t), commitParams(key, "two", &v1.Version, stream, "B", "C")); err == nil {
		t.Fatal("the crash hook did not stop the commit")
	}
	afterCommitAppend = nil
	if n := lines(t, s, stream); n != 3 {
		t.Fatalf("the stream file has %d lines after the crash, want 3 (the append happened)", n)
	}
	s.Close(ctx(t))

	s2, log := open(t, dir)
	if n := lines(t, s2, stream); n != 1 {
		t.Fatalf("after the start the stream file has %d lines, want 1", n)
	}
	if !strings.Contains(log.String(), "stream_cut_back") {
		t.Fatalf("no log line for the cut back: %s", log.String())
	}
	if g := get(t, s2, key); g.Version != v1.Version || string(g.Value) != `"one"` {
		t.Fatalf("after the crash the document is %+v, want the old one", g)
	}
	if got := types(readAll(t, s2, stream)); got != "A" {
		t.Fatalf("after the crash the stream has %s, want A", got)
	}
	// The next commit continues from the old state.
	r := commit(t, s2, key, "two", &v1.Version, stream, "B")
	if r.Cursors[0] != "2" || types(readAll(t, s2, stream)) != "A,B" {
		t.Fatalf("the commit after the crash: %+v, %s", r, types(readAll(t, s2, stream)))
	}
}

// A crash in the commit that makes the document: no document, and no event
// of the commit. An event that was there before stays.
func TestCommitCrashOnCreate(t *testing.T) {
	dir := t.TempDir()
	s, _ := open(t, dir)
	const key, stream = "r1:new", "spec:r1:new"
	appendN(t, s, stream, "before")
	crashNextCommit(t)
	if _, err := s.Commit(ctx(t), commitParams(key, "one", nil, stream, "A")); err == nil {
		t.Fatal("the crash hook did not stop the commit")
	}
	afterCommitAppend = nil
	// A read in the same process, under the lock, cuts back too.
	if got := types(readAll(t, s, stream)); got != "before" {
		t.Fatalf("after the crash the stream has %s, want before", got)
	}
	if lines(t, s, stream) != 1 || get(t, s, key).Exists {
		t.Fatal("the crash left the event or the document")
	}
}

// A put keeps the stream record, a plain append on a committed stream stays,
// and a delete of the document keeps the stream's events.
func TestCommitWithPutAppendDelete(t *testing.T) {
	dir := t.TempDir()
	s, _ := open(t, dir)
	const key, stream = "r1:drop-spec", "spec:r1:drop-spec"
	v1 := commit(t, s, key, "one", nil, stream, "A")
	pv := put(t, s, "app-specs", key, "put", nil, &v1.Version)
	appendN(t, s, stream, "plain")
	s.Close(ctx(t))
	s, _ = open(t, dir)
	if got := types(readAll(t, s, stream)); got != "A,plain" {
		t.Fatalf("after a put and a plain append: %s", got)
	}
	v3 := commit(t, s, key, "three", &pv.Version, stream, "B")
	if got := types(readAll(t, s, stream)); got != "A,plain,B" {
		t.Fatalf("after the next commit: %s", got)
	}
	if _, err := s.Delete(ctx(t), protocol.StateDeleteParams{Collection: "app-specs", Key: key, ExpectedVersion: v3.Version}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(s.markerPath(stream)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the marker stays after the delete of the document: %v", err)
	}
	s.Close(ctx(t))
	s, _ = open(t, dir)
	if got := types(readAll(t, s, stream)); got != "A,plain,B" {
		t.Fatalf("after the delete of the document: %s", got)
	}
}

// markersOf gives the streams whose marker names the document.
func markersOf(t *testing.T, s *Store, key string) []string {
	t.Helper()
	entries, err := os.ReadDir(s.eventsRoot())
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), markerExt) {
			continue
		}
		m, err := readMarker(filepath.Join(s.eventsRoot(), e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if m != nil && m.Collection == "app-specs" && m.Key == key {
			out = append(out, m.Stream)
		}
	}
	return out
}

// A document that commits to a second stream keeps the events that it
// committed to the first one, after a restart and after its delete.
func TestCommitMovesToAnotherStream(t *testing.T) {
	dir := t.TempDir()
	s, _ := open(t, dir)
	const key, s1, s2 = "r1:drop-spec", "spec:r1:one", "spec:r1:two"
	v1 := commit(t, s, key, "one", nil, s1, "A")
	v2 := commit(t, s, key, "two", &v1.Version, s1, "B", "C")
	v3 := commit(t, s, key, "three", &v2.Version, s2, "D")
	if got := types(readAll(t, s, s1)); got != "A,B,C" {
		t.Fatalf("after the move the first stream has %s, want A,B,C", got)
	}
	if got := strings.Join(markersOf(t, s, key), ","); got != s2 {
		t.Fatalf("the markers that name the document: %q, want %s", got, s2)
	}
	s.Close(ctx(t))

	s, _ = open(t, dir)
	if got := types(readAll(t, s, s1)); got != "A,B,C" {
		t.Fatalf("after a restart the first stream has %s, want A,B,C", got)
	}
	if got := types(readAll(t, s, s2)); got != "D" {
		t.Fatalf("after a restart the second stream has %s, want D", got)
	}
	// A move back to the first stream keeps the events of both.
	v4 := commit(t, s, key, "four", &v3.Version, s1, "E")
	if r := v4.Cursors; len(r) != 1 || r[0] != "4" {
		t.Fatalf("the cursors of the move back: %v", r)
	}
	if _, err := s.Delete(ctx(t), protocol.StateDeleteParams{Collection: "app-specs", Key: key, ExpectedVersion: v4.Version}); err != nil {
		t.Fatal(err)
	}
	if got := markersOf(t, s, key); len(got) != 0 {
		t.Fatalf("markers stay after the delete: %v", got)
	}
	s.Close(ctx(t))

	s, _ = open(t, dir)
	if got := types(readAll(t, s, s1)); got != "A,B,C,E" {
		t.Fatalf("after the delete the first stream has %s, want A,B,C,E", got)
	}
	if got := types(readAll(t, s, s2)); got != "D" {
		t.Fatalf("after the delete the second stream has %s, want D", got)
	}
}

// A crash in the commit that moves a document to another stream: the first
// stream keeps all its events, the second one loses only the events of the
// stopped commit, and the next commit continues.
func TestCommitCrashOnMove(t *testing.T) {
	dir := t.TempDir()
	s, _ := open(t, dir)
	const key, s1, s2 = "r1:drop-spec", "spec:r1:one", "spec:r1:two"
	appendN(t, s, s2, "before")
	v1 := commit(t, s, key, "one", nil, s1, "A")
	v2 := commit(t, s, key, "two", &v1.Version, s1, "B")

	crashNextCommit(t)
	if _, err := s.Commit(ctx(t), commitParams(key, "three", &v2.Version, s2, "C", "D")); err == nil {
		t.Fatal("the crash hook did not stop the commit")
	}
	afterCommitAppend = nil
	if n := lines(t, s, s2); n != 3 {
		t.Fatalf("the second stream file has %d lines after the crash, want 3 (the append happened)", n)
	}
	s.Close(ctx(t))

	s, _ = open(t, dir)
	if got := types(readAll(t, s, s1)); got != "A,B" {
		t.Fatalf("after the crash the first stream has %s, want A,B", got)
	}
	if got := types(readAll(t, s, s2)); got != "before" {
		t.Fatalf("after the crash the second stream has %s, want before", got)
	}
	if g := get(t, s, key); g.Version != v2.Version {
		t.Fatalf("after the crash the document is %+v, want the old one", g)
	}
	v3 := commit(t, s, key, "three", &v2.Version, s2, "C")
	if got := types(readAll(t, s, s2)); got != "before,C" || v3.Cursors[0] != "2" {
		t.Fatalf("the commit after the crash: %v, %s", v3.Cursors, got)
	}
	if got := types(readAll(t, s, s1)); got != "A,B" {
		t.Fatalf("the first stream has %s, want A,B", got)
	}
}

// A crash in a move leaves a marker on the new stream that names the
// document, while the document still records the old stream. A delete of the
// document settles that marker too: the events of the stopped commit go, and
// the other events stay.
func TestDeleteSettlesEveryMarker(t *testing.T) {
	dir := t.TempDir()
	s, _ := open(t, dir)
	const key, s1, s2 = "r1:drop-spec", "spec:r1:one", "spec:r1:two"
	appendN(t, s, s2, "before")
	v1 := commit(t, s, key, "one", nil, s1, "A")
	crashNextCommit(t)
	if _, err := s.Commit(ctx(t), commitParams(key, "two", &v1.Version, s2, "B")); err == nil {
		t.Fatal("the crash hook did not stop the commit")
	}
	afterCommitAppend = nil
	if got := strings.Join(markersOf(t, s, key), ","); got != s2 {
		t.Fatalf("the markers after the crash: %q, want %s", got, s2)
	}
	if _, err := s.Delete(ctx(t), protocol.StateDeleteParams{Collection: "app-specs", Key: key, ExpectedVersion: v1.Version}); err != nil {
		t.Fatal(err)
	}
	if got := markersOf(t, s, key); len(got) != 0 {
		t.Fatalf("markers stay after the delete: %v", got)
	}
	s.Close(ctx(t))
	s, _ = open(t, dir)
	if got := types(readAll(t, s, s1)); got != "A" {
		t.Fatalf("the first stream has %s, want A", got)
	}
	if got := types(readAll(t, s, s2)); got != "before" {
		t.Fatalf("the second stream has %s, want before", got)
	}
}

// A commit wakes a wait on the document and a wait on the stream.
func TestCommitWakesWaits(t *testing.T) {
	s, _ := open(t, t.TempDir())
	const key, stream = "r1:drop-spec", "spec:r1:drop-spec"
	v1 := commit(t, s, key, "one", nil, stream, "A")
	doc := waitAsync(s, protocol.WatchWaitParams{Collection: "app-specs", Key: key, Version: &v1.Version, TimeoutMs: 5000})
	strm := waitAsync(s, protocol.WatchWaitParams{Stream: stream, After: &v1.Cursors[0], TimeoutMs: 5000})
	time.Sleep(50 * time.Millisecond)
	v2 := commit(t, s, key, "two", &v1.Version, stream, "B")
	d, w := <-doc, <-strm
	if !d.r.Changed || *d.r.Version != v2.Version || d.at > time.Second {
		t.Fatalf("document wait: %+v after %v", d.r, d.at)
	}
	if !w.r.Changed || *w.r.Cursor != v2.Cursors[0] || w.at > time.Second {
		t.Fatalf("stream wait: %+v after %v", w.r, w.at)
	}
}

// The storeId is json:<absolute data folder>, in a 1.1 handshake only.
func TestStoreID(t *testing.T) {
	dir := t.TempDir()
	abs, _ := filepath.Abs(dir)
	_, _, h := serveVersions(t, dir, "1.0", "1.1")
	if h.ProtocolVersion != "1.1" || h.StoreID != "json:"+abs {
		t.Fatalf("handshake %s with storeId %q, want 1.1 with json:%s", h.ProtocolVersion, h.StoreID, abs)
	}
	_, _, h = serveVersions(t, dir, "1.0")
	if h.ProtocolVersion != "1.0" || h.StoreID != "" {
		t.Fatalf("a 1.0 handshake: %s with storeId %q", h.ProtocolVersion, h.StoreID)
	}
}

func bg() context.Context { return context.Background() }

// commitRetry reads the document and commits one event on its version, and
// tries again on a conflict. It gives the number of conflicts.
func commitRetry(s *Store, key, stream, event string) (int, error) {
	conflicts := 0
	for {
		g, err := s.Get(bg(), protocol.StateGetParams{Collection: "app-specs", Key: key})
		if err != nil {
			return conflicts, err
		}
		var expected *string
		n := 0
		if g.Exists {
			expected = &g.Version
			n, _ = strconv.Atoi(string(g.Value))
		}
		_, err = s.Commit(bg(), protocol.StateCommitParams{Collection: "app-specs", Key: key, Value: raw(n + 1), ExpectedVersion: expected, Stream: stream,
			Events: []protocol.NewEvent{{Type: "activity", Data: raw(event)}}})
		if err == nil {
			return conflicts, nil
		}
		if !protocol.Is(err, protocol.Conflict) {
			return conflicts, err
		}
		conflicts++
	}
}

// checkHundred checks that the document counts 100 commits and the stream
// has the 100 events, each one time.
func checkHundred(t *testing.T, s *Store, key, stream string) {
	t.Helper()
	if g := get(t, s, key); string(g.Value) != "100" {
		t.Fatalf("the document counts %s commits, want 100", g.Value)
	}
	events := readAll(t, s, stream)
	if len(events) != 100 {
		t.Fatalf("the stream has %d events, want 100", len(events))
	}
	seen := map[string]bool{}
	for _, e := range events {
		if seen[string(e.Data)] {
			t.Fatalf("the event %s is two times in the stream", e.Data)
		}
		seen[string(e.Data)] = true
	}
}

// R6.S2: two stores on one data folder commit the same spec 50 times each,
// in parallel. Each commit is kept or gets a conflict and is tried again,
// and the stream has exactly 100 events.
func TestCommitTwoStores(t *testing.T) {
	dir := t.TempDir()
	a, _ := open(t, dir)
	b, _ := open(t, dir)
	const key, stream = "r1:race", "spec:r1:race"
	var wg sync.WaitGroup
	var mu sync.Mutex
	conflicts := 0
	for i, s := range []*Store{a, b} {
		wg.Add(1)
		go func(i int, s *Store) {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				c, err := commitRetry(s, key, stream, fmt.Sprintf("s%d-%d", i, j))
				if err != nil {
					t.Errorf("store %d commit %d: %v", i, j, err)
					return
				}
				mu.Lock()
				conflicts += c
				mu.Unlock()
			}
		}(i, s)
	}
	wg.Wait()
	checkHundred(t, a, key, stream)
	checkHundred(t, b, key, stream)
	t.Logf("%d conflicts", conflicts)
}

// R6.S2 with two processes: the test binary runs itself twice as a helper.
func TestCommitTwoProcesses(t *testing.T) {
	if os.Getenv("PLUGIN_JSON_HELPER") == "commit" {
		return
	}
	dir := t.TempDir()
	start := filepath.Join(dir, "go")
	const key, stream = "r1:race", "spec:r1:race"
	var cmds []*exec.Cmd
	var outs []*strings.Builder
	for i := 0; i < 2; i++ {
		cmd := exec.Command(os.Args[0], "-test.run=^TestHelperCommit$")
		cmd.Env = append(os.Environ(), "PLUGIN_JSON_HELPER=commit", "HELPER_DIR="+dir, "HELPER_START="+start, fmt.Sprintf("HELPER_NAME=p%d", i))
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
	for i, cmd := range cmds {
		cmd.Wait()
		if !strings.Contains(outs[i].String(), "RESULT ok") {
			t.Fatalf("helper %d: %s", i, outs[i].String())
		}
		t.Logf("helper %d: %s", i, strings.TrimSpace(outs[i].String()))
	}
	s, _ := open(t, dir)
	checkHundred(t, s, key, stream)
}

func TestHelperCommit(t *testing.T) {
	if os.Getenv("PLUGIN_JSON_HELPER") != "commit" {
		t.Skip("a helper of TestCommitTwoProcesses")
	}
	s, _ := open(t, os.Getenv("HELPER_DIR"))
	for {
		if _, err := os.Stat(os.Getenv("HELPER_START")); err == nil {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	conflicts := 0
	for j := 0; j < 50; j++ {
		c, err := commitRetry(s, "r1:race", "spec:r1:race", fmt.Sprintf("%s-%d", os.Getenv("HELPER_NAME"), j))
		if err != nil {
			fmt.Println("RESULT error", err)
			return
		}
		conflicts += c
	}
	fmt.Println("RESULT ok conflicts", conflicts)
}

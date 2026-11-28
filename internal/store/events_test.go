package store

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spec-studio-app/plugin-sdk/go/protocol"
)

func appendN(t *testing.T, s *Store, stream string, types ...string) []string {
	t.Helper()
	var events []protocol.NewEvent
	for i, ty := range types {
		events = append(events, protocol.NewEvent{Type: ty, Data: raw(i)})
	}
	r, err := s.Append(ctx(t), protocol.EventsAppendParams{Stream: stream, Events: events})
	if err != nil {
		t.Fatalf("append to %s: %v", stream, err)
	}
	return r.Cursors
}

func readAll(t *testing.T, s *Store, stream string) []protocol.Event {
	t.Helper()
	var out []protocol.Event
	var after *string
	for {
		r, err := s.Read(ctx(t), protocol.EventsReadParams{Stream: stream, After: after, Limit: 1000})
		if err != nil {
			t.Fatalf("read %s: %v", stream, err)
		}
		out = append(out, r.Events...)
		if !r.More {
			return out
		}
		after = r.Cursor
	}
}

func types(events []protocol.Event) string {
	var out []string
	for _, e := range events {
		out = append(out, e.Type)
	}
	return strings.Join(out, ",")
}

// The events of a stream: an order, pages with more, a cursor, a time range,
// a delete, and a restart.
func TestEventsReadAppendDelete(t *testing.T) {
	dir := t.TempDir()
	s, _ := open(t, dir)
	stream := "desk:1a2b3c4d5e6f"
	c1 := appendN(t, s, stream, "A", "B", "C")
	c2 := appendN(t, s, stream, "D", "E")
	if strings.Join(c1, ",") != "1,2,3" || strings.Join(c2, ",") != "4,5" {
		t.Fatalf("cursors %v %v", c1, c2)
	}

	p1, err := s.Read(ctx(t), protocol.EventsReadParams{Stream: stream, Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if types(p1.Events) != "A,B" || !p1.More || p1.Cursor == nil || *p1.Cursor != "2" {
		t.Fatalf("page 1: %+v", p1)
	}
	p2, _ := s.Read(ctx(t), protocol.EventsReadParams{Stream: stream, After: p1.Cursor, Limit: 2})
	if types(p2.Events) != "C,D" || !p2.More {
		t.Fatalf("page 2: %+v", p2)
	}
	p3, _ := s.Read(ctx(t), protocol.EventsReadParams{Stream: stream, After: p2.Cursor, Limit: 2})
	if types(p3.Events) != "E" || p3.More || *p3.Cursor != "5" {
		t.Fatalf("page 3: %+v", p3)
	}
	end, _ := s.Read(ctx(t), protocol.EventsReadParams{Stream: stream, After: p3.Cursor, Limit: 2})
	if len(end.Events) != 0 || end.More || end.Cursor == nil || *end.Cursor != "5" {
		t.Fatalf("after the end: %+v", end)
	}
	if string(p1.Events[1].Data) != "1" || p1.Events[0].At.IsZero() {
		t.Fatalf("event B: %+v", p1.Events[1])
	}

	// A cursor of another plugin is refused.
	for _, bad := range []string{"x", "-1", "01", "1.5"} {
		_, err := s.Read(ctx(t), protocol.EventsReadParams{Stream: stream, After: str(bad)})
		if !protocol.Is(err, protocol.Refused) {
			t.Fatalf("cursor %q: %v, want refused", bad, err)
		}
	}

	// The time range: from <= at < to.
	base := time.Date(2026, 1, 2, 10, 0, 0, 0, time.UTC)
	var timed []protocol.NewEvent
	for i, m := range []int{0, 5, 10} {
		at := base.Add(time.Duration(m) * time.Minute)
		timed = append(timed, protocol.NewEvent{Type: string(rune('X' + i)), Data: raw(i), At: &at})
	}
	if _, err := s.Append(ctx(t), protocol.EventsAppendParams{Stream: "timed", Events: timed}); err != nil {
		t.Fatal(err)
	}
	to := base.Add(10 * time.Minute)
	r, _ := s.Read(ctx(t), protocol.EventsReadParams{Stream: "timed", From: &base, To: &to})
	if types(r.Events) != "X,Y" || !r.Events[1].At.Equal(base.Add(5*time.Minute)) {
		t.Fatalf("range: %+v", r.Events)
	}

	// A restart keeps the events.
	s.Close(ctx(t))
	s, _ = open(t, dir)
	if got := types(readAll(t, s, stream)); got != "A,B,C,D,E" {
		t.Fatalf("after a restart: %s", got)
	}

	// A delete removes the events, and keeps the count: a new event never
	// gets the cursor of a deleted event.
	d, err := s.DeleteStream(ctx(t), protocol.EventsDeleteParams{Stream: stream})
	if err != nil || !d.Deleted {
		t.Fatalf("delete: %+v %v", d, err)
	}
	if got := readAll(t, s, stream); len(got) != 0 {
		t.Fatalf("after the delete: %s", types(got))
	}
	d, _ = s.DeleteStream(ctx(t), protocol.EventsDeleteParams{Stream: stream})
	if d.Deleted {
		t.Fatal("a second delete says that the stream had events")
	}
	d, _ = s.DeleteStream(ctx(t), protocol.EventsDeleteParams{Stream: "never"})
	if d.Deleted {
		t.Fatal("the delete of a stream with no events says that it had events")
	}
	if c := appendN(t, s, stream, "F"); c[0] != "6" {
		t.Fatalf("the first event after the delete has the cursor %s, want 6", c[0])
	}
	if got := readAll(t, s, stream); types(got) != "F" || got[0].Cursor != "6" {
		t.Fatalf("after the delete and an append: %+v", got)
	}
}

// A part of a line that a crash left is not an event, and the next append
// goes after the last complete line.
func TestEventsTornLine(t *testing.T) {
	dir := t.TempDir()
	s, _ := open(t, dir)
	appendN(t, s, "torn", "A")
	f, err := os.OpenFile(s.streamPath("torn"), os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString(`{"type":"B","at":"2026-`)
	f.Close()
	if got := types(readAll(t, s, "torn")); got != "A" {
		t.Fatalf("with a part of a line: %s", got)
	}
	if c := appendN(t, s, "torn", "C"); c[0] != "2" {
		t.Fatalf("cursor %s, want 2", c[0])
	}
	if got := types(readAll(t, s, "torn")); got != "A,C" {
		t.Fatalf("after an append: %s", got)
	}
}

// The file name of a stream keeps the case and has no ":".
func TestEventsFileNames(t *testing.T) {
	s, _ := open(t, t.TempDir())
	appendN(t, s, "Spec:AB", "upper")
	appendN(t, s, "spec:ab", "lower")
	if types(readAll(t, s, "Spec:AB")) != "upper" || types(readAll(t, s, "spec:ab")) != "lower" {
		t.Fatal("two stream names that differ in case share a file")
	}
	names, _ := filepath.Glob(filepath.Join(s.eventsRoot(), "*.jsonl"))
	for _, n := range names {
		if strings.Contains(filepath.Base(n), ":") {
			t.Fatalf("the file name %s has a colon", n)
		}
	}
}

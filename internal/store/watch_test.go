package store

import (
	"context"
	"testing"
	"time"

	"github.com/spec-studio-app/plugin-sdk/go/protocol"
)

type waitResult struct {
	r   protocol.WatchWaitResult
	err error
	at  time.Duration
}

func waitAsync(s *Store, p protocol.WatchWaitParams) <-chan waitResult {
	out := make(chan waitResult, 1)
	start := time.Now()
	go func() {
		c, cancel := context.WithTimeout(context.Background(), time.Duration(p.TimeoutMs)*time.Millisecond)
		defer cancel()
		r, err := s.Wait(c, p)
		out <- waitResult{r, err, time.Since(start)}
	}()
	return out
}

// R4.S1
func TestWaitEndsOnPut(t *testing.T) {
	s, _ := open(t, t.TempDir())
	v := put(t, s, "specs", "x", 1, nil, nil).Version
	done := waitAsync(s, protocol.WatchWaitParams{Collection: "specs", Key: "x", Version: &v, TimeoutMs: 30000})
	time.Sleep(100 * time.Millisecond)
	nv := put(t, s, "specs", "x", 2, nil, &v).Version
	select {
	case w := <-done:
		if w.err != nil || !w.r.Changed || w.r.Version == nil || *w.r.Version != nv {
			t.Fatalf("wait %+v %v", w.r, w.err)
		}
	case <-time.After(time.Second):
		t.Fatal("the wait did not end within 1 s of the put")
	}
}

// R4.S2: a second store on the same folder shares no memory with the first,
// as a second process.
func TestWaitSeesOtherStore(t *testing.T) {
	dir := t.TempDir()
	a, _ := open(t, dir)
	b, _ := open(t, dir)
	v := put(t, a, "relay", "config", 1, nil, nil).Version
	done := waitAsync(a, protocol.WatchWaitParams{Collection: "relay", Key: "config", Version: &v, TimeoutMs: 30000})
	time.Sleep(100 * time.Millisecond)
	put(t, b, "relay", "config", 2, nil, &v)
	select {
	case w := <-done:
		if w.err != nil || !w.r.Changed {
			t.Fatalf("wait %+v %v", w.r, w.err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the wait did not see the other store's put within 2 s")
	}
}

// R4.S3
func TestWaitEndsOnDelete(t *testing.T) {
	s, _ := open(t, t.TempDir())
	v := put(t, s, "specs", "x", 1, nil, nil).Version
	done := waitAsync(s, protocol.WatchWaitParams{Collection: "specs", Key: "x", Version: &v, TimeoutMs: 30000})
	time.Sleep(100 * time.Millisecond)
	if _, err := s.Delete(ctx(t), protocol.StateDeleteParams{Collection: "specs", Key: "x", ExpectedVersion: v}); err != nil {
		t.Fatal(err)
	}
	select {
	case w := <-done:
		if w.err != nil || !w.r.Changed || w.r.Version != nil {
			t.Fatalf("wait %+v %v", w.r, w.err)
		}
	case <-time.After(time.Second):
		t.Fatal("the wait did not end within 1 s of the delete")
	}
}

// R4.S4
func TestWaitTimeout(t *testing.T) {
	s, _ := open(t, t.TempDir())
	v := put(t, s, "specs", "x", 1, nil, nil).Version
	w := <-waitAsync(s, protocol.WatchWaitParams{Collection: "specs", Key: "x", Version: &v, TimeoutMs: 200})
	if w.r.Changed {
		t.Fatalf("wait %+v", w.r)
	}
	if w.at < 180*time.Millisecond || w.at > 1500*time.Millisecond {
		t.Fatalf("the wait took %v, want about 200 ms", w.at)
	}
}

func TestWaitAtOnceAndStream(t *testing.T) {
	s, _ := open(t, t.TempDir())
	// The host has no document, and none exists: no change.
	w := <-waitAsync(s, protocol.WatchWaitParams{Collection: "specs", Key: "y", Version: nil, TimeoutMs: 50})
	if w.r.Changed {
		t.Fatalf("wait on a missing document: %+v", w.r)
	}
	put(t, s, "specs", "y", 1, nil, nil)
	w = <-waitAsync(s, protocol.WatchWaitParams{Collection: "specs", Key: "y", Version: nil, TimeoutMs: 5000})
	if !w.r.Changed || w.at > time.Second {
		t.Fatalf("wait on a document the host does not have: %+v after %v", w.r, w.at)
	}
	// A stream with an event after the host's cursor: at once, with the
	// cursor of the last event.
	r, err := s.Append(ctx(t), protocol.EventsAppendParams{Stream: "spec:x", Events: []protocol.NewEvent{{Type: "A", Data: raw(1)}, {Type: "B", Data: raw(2)}}})
	if err != nil {
		t.Fatal(err)
	}
	w = <-waitAsync(s, protocol.WatchWaitParams{Stream: "spec:x", After: &r.Cursors[0], TimeoutMs: 5000})
	if !w.r.Changed || w.r.Cursor == nil || *w.r.Cursor != r.Cursors[1] || w.at > time.Second {
		t.Fatalf("wait on a stream with a newer event: %+v after %v", w.r, w.at)
	}
	_, err = s.Wait(ctx(t), protocol.WatchWaitParams{Stream: "spec:x", After: str("x1"), TimeoutMs: 50})
	if !protocol.Is(err, protocol.Refused) {
		t.Fatalf("stream wait with a cursor of another plugin: %v, want refused", err)
	}
}

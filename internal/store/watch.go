package store

import (
	"context"
	"strconv"
	"sync"
	"time"

	"github.com/spec-studio-app/plugin-json/internal/files"
	"github.com/spec-studio-app/plugin-sdk/go/protocol"
)

// PollInterval is how often a wait checks the file, for a change from
// another process on the same data folder.
var PollInterval = 250 * time.Millisecond

// hub wakes the waits of a document or a stream when this process changes
// it. A stream has the collection "", which no document has.
type hub struct {
	mu     sync.Mutex
	chans  map[string]chan struct{}
	closed chan struct{}
	once   sync.Once
}

func newHub() *hub {
	return &hub{chans: map[string]chan struct{}{}, closed: make(chan struct{})}
}

func hubKey(collection, key string) string { return collection + "\x00" + key }

// channel gives the channel that the next change of the document closes.
func (h *hub) channel(collection, key string) chan struct{} {
	h.mu.Lock()
	defer h.mu.Unlock()
	k := hubKey(collection, key)
	c := h.chans[k]
	if c == nil {
		c = make(chan struct{})
		h.chans[k] = c
	}
	return c
}

// notify wakes every wait of the document.
func (h *hub) notify(collection, key string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	k := hubKey(collection, key)
	if c := h.chans[k]; c != nil {
		close(c)
		delete(h.chans, k)
	}
}

func (h *hub) closeAll() { h.once.Do(func() { close(h.closed) }) }

// Wait ends when the stored version of the document is not the version of
// the host: at once, at a change in this process, at a change from another
// process (seen by a check of the file), or when ctx ends. A wait on a
// stream ends when the stream has an event after the host's cursor.
func (s *Store) Wait(ctx context.Context, p protocol.WatchWaitParams) (protocol.WatchWaitResult, error) {
	if p.IsStream() {
		return s.waitStream(ctx, p)
	}
	path := files.DocPath(s.stateRoot(), p.Collection, p.Key)
	tick := time.NewTicker(PollInterval)
	defer tick.Stop()
	for {
		// Take the channel before the read, so no change goes between them.
		changed := s.waiters.channel(p.Collection, p.Key)
		d, err := files.ReadDoc(path)
		if err != nil || (d != nil && d.Key != p.Key) {
			return protocol.WatchWaitResult{}, unavailable("The plugin cannot read the document %s / %s. Correct or remove its file in the data folder.", p.Collection, p.Key)
		}
		var cur *string
		if d != nil {
			v := d.Version
			cur = &v
		}
		if differs(cur, p.Version) {
			return protocol.WatchWaitResult{Changed: true, Version: cur}, nil
		}
		select {
		case <-ctx.Done():
			return protocol.WatchWaitResult{}, ctx.Err()
		case <-s.waiters.closed:
			return protocol.WatchWaitResult{}, nil
		case <-changed:
		case <-tick.C:
		}
	}
}

// waitStream ends when the stream's length is more than the cursor of the
// host, and gives the cursor of the last event.
func (s *Store) waitStream(ctx context.Context, p protocol.WatchWaitParams) (protocol.WatchWaitResult, error) {
	var after int64
	if p.After != nil && *p.After != "" {
		n, err := parseCursor(*p.After)
		if err != nil {
			return protocol.WatchWaitResult{}, err
		}
		after = n
	}
	tick := time.NewTicker(PollInterval)
	defer tick.Stop()
	for {
		changed := s.waiters.channel("", p.Stream)
		n, err := s.streamLength(p.Stream)
		if err != nil {
			return protocol.WatchWaitResult{}, err
		}
		if n > after {
			c := strconv.FormatInt(n, 10)
			return protocol.WatchWaitResult{Changed: true, Cursor: &c}, nil
		}
		select {
		case <-ctx.Done():
			return protocol.WatchWaitResult{}, ctx.Err()
		case <-s.waiters.closed:
			return protocol.WatchWaitResult{}, nil
		case <-changed:
		case <-tick.C:
		}
	}
}

func differs(a, b *string) bool {
	if a == nil || b == nil {
		return (a == nil) != (b == nil)
	}
	return *a != *b
}

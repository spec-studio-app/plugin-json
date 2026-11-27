package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/spec-studio-app/plugin-json/internal/files"
	"github.com/spec-studio-app/plugin-sdk/go/protocol"
)

// The files of a stream, in the folder events/ of the data folder:
//
//	<name>.jsonl     the events, one JSON object on each line, in order. The
//	                 first line can be a header {"base":N}: N events came
//	                 before the first line, and a delete removed them.
//	<name>.lock      the lock of the stream.
//	<name>.doc.json  the marker: the document that state.commit writes with
//	                 the stream. Only a stream that a commit wrote has one.
//
// <name> is the file name of the stream name, as for a document key. The
// cursor of an event is its number in the stream (1 for the first event
// that the stream ever had), as a decimal string. A delete keeps the count,
// so a cursor is never used again.
const (
	streamExt = ".jsonl"
	lockExt   = ".lock"
	markerExt = ".doc.json"
)

// eventLine is one line of a stream file.
type eventLine struct {
	Type string          `json:"type"`
	At   time.Time       `json:"at"`
	Data json.RawMessage `json:"data"`
}

// streamHeader is the first line of a stream file after a delete.
type streamHeader struct {
	Base int64 `json:"base"`
}

// marker names the document whose commits write the stream. Base is the
// length of the stream when the marker was made: a cut back never goes
// below it.
type marker struct {
	Stream     string `json:"stream"`
	Collection string `json:"collection"`
	Key        string `json:"key"`
	Base       int64  `json:"base"`
}

// streamFile is a stream file as read from the disk.
type streamFile struct {
	base   int64    // the number of events before the first line
	header int64    // the size in bytes of the header line, 0 when none
	lines  [][]byte // the complete event lines, with no newline
	ends   []int64  // the byte offset after each event line
	clean  int64    // the byte offset after the last complete line
	size   int64    // the size of the file
}

// total is the length of the stream: the deleted events and the events in
// the file.
func (f *streamFile) total() int64 { return f.base + int64(len(f.lines)) }

// sizeAt is the file size that keeps the first n events of the stream.
func (f *streamFile) sizeAt(n int64) int64 {
	keep := n - f.base
	if keep <= 0 {
		return f.header
	}
	return f.ends[keep-1]
}

func (s *Store) eventsRoot() string { return filepath.Join(s.dir, "events") }

func (s *Store) streamPath(stream string) string {
	return filepath.Join(s.eventsRoot(), files.FileName(stream)+streamExt)
}

func (s *Store) markerPath(stream string) string {
	return filepath.Join(s.eventsRoot(), files.FileName(stream)+markerExt)
}

// streamLock takes the lock of a stream: a mutex in this process and a flock
// for other processes. A caller that also takes a collection lock takes the
// collection lock first, so two calls never wait on each other.
func (s *Store) streamLock(stream string) (func(), error) {
	s.mu.Lock()
	m := s.streamLocks[stream]
	if m == nil {
		m = &sync.Mutex{}
		s.streamLocks[stream] = m
	}
	s.mu.Unlock()
	m.Lock()
	unlock, err := files.LockFile(filepath.Join(s.eventsRoot(), files.FileName(stream)+lockExt))
	if err != nil {
		m.Unlock()
		return nil, unavailable("The plugin cannot lock the stream %s.", stream)
	}
	return func() {
		unlock()
		m.Unlock()
	}, nil
}

// readStreamFile reads a stream file. A missing file is an empty stream. A
// last line with no newline is a part of an append that a crash stopped: it
// is not an event.
func readStreamFile(path string) (*streamFile, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return &streamFile{}, nil
	}
	if err != nil {
		return nil, err
	}
	f := &streamFile{size: int64(len(data))}
	var pos int64
	first := true
	for {
		i := bytes.IndexByte(data[pos:], '\n')
		if i < 0 {
			break
		}
		line := data[pos : pos+int64(i)]
		end := pos + int64(i) + 1
		if first && bytes.HasPrefix(line, []byte(`{"base":`)) {
			var h streamHeader
			if err := json.Unmarshal(line, &h); err != nil || h.Base < 0 {
				return nil, errBadStream
			}
			f.base, f.header = h.Base, end
		} else {
			f.lines = append(f.lines, line)
			f.ends = append(f.ends, end)
		}
		first = false
		pos = end
	}
	f.clean = pos
	return f, nil
}

var errBadStream = errors.New("the stream file is not valid")

func readMarker(path string) (*marker, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var m marker
	if err := json.Unmarshal(data, &m); err != nil || m.Collection == "" || m.Key == "" {
		return nil, errBadStream
	}
	return &m, nil
}

// loadStream reads a stream and cuts it back to its committed length. The
// caller holds the lock of the stream.
//
// A commit appends the events, then renames the new document file, which
// records the stream's length. A crash between the two steps leaves events
// that no document records. So when the stream has a marker, its length is
// at most the length that the marker's document records (or the marker's
// base when the document has no record), and the events after it go. A
// part of a line that a crash left goes too.
func (s *Store) loadStream(stream string) (*streamFile, error) {
	path := s.streamPath(stream)
	f, err := readStreamFile(path)
	if err != nil {
		return nil, unavailable("The plugin cannot read the stream %s. Correct or remove its file in the data folder.", stream)
	}
	target := f.total()
	m, err := readMarker(s.markerPath(stream))
	if err != nil {
		return nil, unavailable("The plugin cannot read the marker of the stream %s. Correct or remove its file in the data folder.", stream)
	}
	if m != nil {
		committed := m.Base
		d, err := s.readDoc(m.Collection, m.Key)
		if err != nil {
			return nil, err
		}
		if d != nil && d.Commit != nil && d.Commit.Stream == stream && d.Commit.Length > committed {
			committed = d.Commit.Length
		}
		if committed < f.base {
			committed = f.base
		}
		if committed < target {
			target = committed
		}
	}
	size := f.clean
	if target < f.total() {
		size = f.sizeAt(target)
	}
	if size == f.size {
		return f, nil
	}
	if err := truncate(path, size); err != nil {
		return nil, unavailable("The plugin cannot cut back the stream %s after a crash. Check the access of the data folder.", stream)
	}
	if target < f.total() {
		s.log.Info("stream_cut_back", "stream", stream, "events", f.total(), "kept", target)
		f.lines, f.ends = f.lines[:target-f.base], f.ends[:target-f.base]
	}
	f.clean, f.size = size, size
	return f, nil
}

// truncate cuts a file to size and flushes it.
func truncate(path string, size int64) error {
	fh, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	defer fh.Close()
	if err := fh.Truncate(size); err != nil {
		return err
	}
	return fh.Sync()
}

// appendEvents adds the events at the end of the stream file in one write
// and flushes it. It returns the cursors of the events.
func (s *Store) appendEvents(stream string, f *streamFile, events []protocol.NewEvent) ([]string, error) {
	now := time.Now().UTC()
	var buf bytes.Buffer
	cursors := make([]string, len(events))
	for i, e := range events {
		at := now
		if e.At != nil {
			at = e.At.UTC()
		}
		data := e.Data
		if len(data) == 0 {
			data = json.RawMessage("null")
		}
		line, err := protocol.MarshalNoEscape(eventLine{Type: e.Type, At: at, Data: data})
		if err != nil {
			return nil, protocol.RPCError(protocol.RPCInvalidParams, "Event %d has data that is not valid JSON.", i)
		}
		buf.Write(bytes.TrimRight(line, "\n"))
		buf.WriteByte('\n')
		cursors[i] = strconv.FormatInt(f.total()+int64(i)+1, 10)
	}
	path := s.streamPath(stream)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, appendError(stream)
	}
	fh, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return nil, appendError(stream)
	}
	_, werr := fh.Write(buf.Bytes())
	serr := fh.Sync()
	cerr := fh.Close()
	if werr != nil || serr != nil || cerr != nil {
		// Remove a part of the write, so the stream stays as it was.
		_ = truncate(path, f.size)
		return nil, appendError(stream)
	}
	if f.size == 0 {
		files.SyncDir(filepath.Dir(path))
	}
	return cursors, nil
}

func appendError(stream string) error {
	return unavailable("The plugin cannot append to the stream %s. Check the free space and the access of the data folder.", stream)
}

// Append adds the events at the end of the stream.
func (s *Store) Append(ctx context.Context, p protocol.EventsAppendParams) (protocol.EventsAppendResult, error) {
	unlock, err := s.streamLock(p.Stream)
	if err != nil {
		return protocol.EventsAppendResult{}, err
	}
	defer unlock()
	f, err := s.loadStream(p.Stream)
	if err != nil {
		return protocol.EventsAppendResult{}, err
	}
	cursors, err := s.appendEvents(p.Stream, f, p.Events)
	if err != nil {
		return protocol.EventsAppendResult{}, err
	}
	// The stream has a committing document: a later read must keep these
	// events, so the marker's base moves to the new length.
	if err := s.moveMarkerBase(p.Stream, f.total()+int64(len(p.Events))); err != nil {
		return protocol.EventsAppendResult{}, err
	}
	s.waiters.notify("", p.Stream)
	return protocol.EventsAppendResult{Cursors: cursors}, nil
}

// moveMarkerBase sets the base of the stream's marker, when it has one. A
// crash before this step loses the events of an append that did not answer.
func (s *Store) moveMarkerBase(stream string, base int64) error {
	path := s.markerPath(stream)
	m, err := readMarker(path)
	if err != nil {
		return unavailable("The plugin cannot read the marker of the stream %s. Correct or remove its file in the data folder.", stream)
	}
	if m == nil {
		return nil
	}
	m.Base = base
	return s.writeMarker(m)
}

func (s *Store) writeMarker(m *marker) error {
	data, _ := json.Marshal(m)
	if err := files.WriteAtomic(s.markerPath(m.Stream), data); err != nil {
		return unavailable("The plugin cannot write the marker of the stream %s. Check the free space and the access of the data folder.", m.Stream)
	}
	return nil
}

// parseCursor reads a cursor of this plugin: a decimal number, 0 or more.
func parseCursor(c string) (int64, error) {
	n, err := strconv.ParseInt(c, 10, 64)
	if err != nil || n < 0 || strconv.FormatInt(n, 10) != c {
		return 0, protocol.NewError(protocol.Refused, "The cursor %q is not a cursor of this plugin. Read the stream again with no cursor.", c)
	}
	return n, nil
}

// Read gives one page of events after the cursor, in the time range.
func (s *Store) Read(ctx context.Context, p protocol.EventsReadParams) (protocol.EventsReadResult, error) {
	var after int64
	if p.After != nil && *p.After != "" {
		n, err := parseCursor(*p.After)
		if err != nil {
			return protocol.EventsReadResult{}, err
		}
		after = n
	}
	limit := p.Limit
	if limit <= 0 {
		limit = protocol.DefaultPageSize
	}
	unlock, err := s.streamLock(p.Stream)
	if err != nil {
		return protocol.EventsReadResult{}, err
	}
	f, err := s.loadStream(p.Stream)
	unlock()
	if err != nil {
		return protocol.EventsReadResult{}, err
	}
	res := protocol.EventsReadResult{Events: []protocol.Event{}, Cursor: p.After}
	start := after - f.base
	if start < 0 {
		start = 0
	}
	for i := start; i < int64(len(f.lines)); i++ {
		var e eventLine
		if err := json.Unmarshal(f.lines[i], &e); err != nil || e.Type == "" {
			return protocol.EventsReadResult{}, unavailable("The plugin cannot read the event %d of the stream %s. Correct or remove its file in the data folder.", f.base+i+1, p.Stream)
		}
		if p.From != nil && e.At.Before(*p.From) {
			continue
		}
		if p.To != nil && !e.At.Before(*p.To) {
			continue
		}
		if len(res.Events) == limit {
			res.More = true
			break
		}
		c := strconv.FormatInt(f.base+i+1, 10)
		res.Events = append(res.Events, protocol.Event{Cursor: c, Type: e.Type, At: e.At, Data: e.Data})
		res.Cursor = &c
	}
	return res, nil
}

// DeleteStream removes every event of the stream. The file keeps the count
// in its header, so a cursor is never used again and the length that a
// document records stays true.
func (s *Store) DeleteStream(ctx context.Context, p protocol.EventsDeleteParams) (protocol.EventsDeleteResult, error) {
	unlock, err := s.streamLock(p.Stream)
	if err != nil {
		return protocol.EventsDeleteResult{}, err
	}
	defer unlock()
	f, err := s.loadStream(p.Stream)
	if err != nil {
		return protocol.EventsDeleteResult{}, err
	}
	if len(f.lines) == 0 {
		return protocol.EventsDeleteResult{Deleted: false}, nil
	}
	header, _ := json.Marshal(streamHeader{Base: f.total()})
	if err := files.WriteAtomic(s.streamPath(p.Stream), append(header, '\n')); err != nil {
		return protocol.EventsDeleteResult{}, unavailable("The plugin cannot delete the stream %s. Check the access of the data folder.", p.Stream)
	}
	s.waiters.notify("", p.Stream)
	return protocol.EventsDeleteResult{Deleted: true}, nil
}

// streamLength gives the length of a stream, read under its lock.
func (s *Store) streamLength(stream string) (int64, error) {
	unlock, err := s.streamLock(stream)
	if err != nil {
		return 0, err
	}
	defer unlock()
	f, err := s.loadStream(stream)
	if err != nil {
		return 0, err
	}
	return f.total(), nil
}

// releaseDoc settles each stream whose marker names the document, except
// the stream keep (see releaseStream). The caller holds the lock of the
// document's collection, so no new marker names the document during the
// call. It takes one stream lock at a time.
func (s *Store) releaseDoc(collection, key, keep string) error {
	entries, err := os.ReadDir(s.eventsRoot())
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return unavailable("The plugin cannot read the events folder of its data folder (%s).", s.dir)
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, markerExt) || len(name) <= len(markerExt) {
			continue
		}
		m, err := readMarker(filepath.Join(s.eventsRoot(), name))
		if err != nil {
			return unavailable("The plugin cannot read the stream marker %s in its data folder. Correct or remove it.", name)
		}
		if m == nil || m.Collection != collection || m.Key != key || m.Stream == keep {
			continue
		}
		if err := s.releaseStream(m.Stream, collection, key); err != nil {
			return err
		}
	}
	return nil
}

// releaseStream settles a stream whose marker names the document: it cuts
// the stream back to its committed length (a commit that a crash stopped
// goes), then removes the marker. After that, every event in the stream
// stays. The caller holds the lock of the document's collection.
func (s *Store) releaseStream(stream, collection, key string) error {
	unlock, err := s.streamLock(stream)
	if err != nil {
		return err
	}
	defer unlock()
	path := s.markerPath(stream)
	m, err := readMarker(path)
	if err != nil {
		return unavailable("The plugin cannot read the marker of the stream %s. Correct or remove its file in the data folder.", stream)
	}
	if m == nil || m.Collection != collection || m.Key != key {
		return nil
	}
	if _, err := s.loadStream(stream); err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return unavailable("The plugin cannot remove the marker of the stream %s.", stream)
	}
	files.SyncDir(filepath.Dir(path))
	return nil
}

// recoverStreams cuts back each stream that has a marker. Open calls it.
func (s *Store) recoverStreams() error {
	entries, err := os.ReadDir(s.eventsRoot())
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return unavailable("The plugin cannot read the events folder of its data folder (%s).", s.dir)
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || len(name) <= len(markerExt) || name[len(name)-len(markerExt):] != markerExt {
			continue
		}
		m, err := readMarker(filepath.Join(s.eventsRoot(), name))
		if err != nil || m == nil || !protocol.ValidName(m.Stream) || s.markerPath(m.Stream) != filepath.Join(s.eventsRoot(), name) {
			return unavailable("The plugin cannot read the stream marker %s in its data folder. Correct or remove it.", name)
		}
		if _, err := s.streamLength(m.Stream); err != nil {
			return err
		}
	}
	return nil
}

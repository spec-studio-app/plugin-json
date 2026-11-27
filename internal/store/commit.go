package store

import (
	"context"

	"github.com/spec-studio-app/plugin-json/internal/files"
	"github.com/spec-studio-app/plugin-sdk/go/protocol"
)

// afterCommitAppend is a test hook. It runs after a commit appends its
// events and before it renames the new document file. When it returns an
// error, the commit stops there, as a crash does.
var afterCommitAppend func() error

// Commit checks the expected version, appends the events and writes the
// document, as one step (protocol 1.1, state.commit).
//
// The commit holds the lock of the collection and then the lock of the
// stream. It appends the events, then renames the new document file. That
// file records the stream and its length after the append, and the rename is
// the moment of the commit: a crash before it leaves events that the next
// read of the stream cuts back (see loadStream). A version that is not the
// expected one writes nothing.
func (s *Store) Commit(ctx context.Context, p protocol.StateCommitParams) (protocol.StateCommitResult, error) {
	unlockCollection, err := s.collectionLock(p.Collection)
	if err != nil {
		return protocol.StateCommitResult{}, err
	}
	defer unlockCollection()
	unlockStream, err := s.streamLock(p.Stream)
	if err != nil {
		return protocol.StateCommitResult{}, err
	}
	defer unlockStream()

	f, err := s.loadStream(p.Stream)
	if err != nil {
		return protocol.StateCommitResult{}, err
	}
	cur, err := s.readDoc(p.Collection, p.Key)
	if err != nil {
		return protocol.StateCommitResult{}, err
	}
	if err := checkVersion(cur, p.ExpectedVersion); err != nil {
		return protocol.StateCommitResult{}, named(err, p.Collection, p.Key)
	}

	// The marker names this document as the one that commits to the stream.
	// Its base is the length now, so a cut back keeps the earlier events.
	m, err := readMarker(s.markerPath(p.Stream))
	if err != nil {
		return protocol.StateCommitResult{}, unavailable("The plugin cannot read the marker of the stream %s. Correct or remove its file in the data folder.", p.Stream)
	}
	if m == nil || m.Collection != p.Collection || m.Key != p.Key {
		if err := s.writeMarker(&marker{Stream: p.Stream, Collection: p.Collection, Key: p.Key, Base: f.total()}); err != nil {
			return protocol.StateCommitResult{}, err
		}
	}

	cursors := []string{}
	if len(p.Events) > 0 {
		cursors, err = s.appendEvents(p.Stream, f, p.Events)
		if err != nil {
			return protocol.StateCommitResult{}, err
		}
	}
	length := f.total() + int64(len(p.Events))
	if afterCommitAppend != nil {
		if err := afterCommitAppend(); err != nil {
			return protocol.StateCommitResult{}, unavailable("The plugin stopped the commit of %s / %s (test hook).", p.Collection, p.Key)
		}
	}
	d, err := files.WriteDoc(s.stateRoot(), p.Collection, p.Key, p.Value, p.Attributes, &files.StreamRecord{Stream: p.Stream, Length: length})
	if err != nil {
		// Take the events back at once. Else the next read cuts them back.
		_ = truncate(s.streamPath(p.Stream), f.size)
		return protocol.StateCommitResult{}, unavailable("The plugin cannot write the document %s / %s. Check the free space and the access of the data folder.", p.Collection, p.Key)
	}
	s.waiters.notify(p.Collection, p.Key)
	if len(p.Events) > 0 {
		s.waiters.notify("", p.Stream)
	}
	return protocol.StateCommitResult{Version: d.Version, UpdatedAt: d.UpdatedAt, Cursors: cursors}, nil
}

// StoreID names the data folder, for the storeId of the handshake: two
// processes on the same data folder give the same value.
func (s *Store) StoreID() string {
	if s.dir == "" {
		return ""
	}
	return "json:" + s.dir
}

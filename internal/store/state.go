package store

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/spec-studio-app/plugin-json/internal/files"
	"github.com/spec-studio-app/plugin-sdk/go/protocol"
)

// readDoc reads a document. A file that is not valid gives unavailable: the
// message names the collection and the key, never the value.
func (s *Store) readDoc(collection, key string) (*files.Doc, error) {
	d, err := files.ReadDoc(files.DocPath(s.stateRoot(), collection, key))
	if err != nil || (d != nil && d.Key != key) {
		return nil, unavailable("The plugin cannot read the document %s / %s. Correct or remove its file in the data folder.", collection, key)
	}
	return d, nil
}

// Get gives a document, or Exists false.
func (s *Store) Get(ctx context.Context, p protocol.StateGetParams) (protocol.StateGetResult, error) {
	d, err := s.readDoc(p.Collection, p.Key)
	if err != nil || d == nil {
		return protocol.StateGetResult{}, err
	}
	at := d.UpdatedAt
	return protocol.StateGetResult{Exists: true, Value: d.Value, Attributes: d.Attributes, Version: d.Version, UpdatedAt: &at}, nil
}

// Put checks the expected version and writes, as one step under the lock of
// the collection.
func (s *Store) Put(ctx context.Context, p protocol.StatePutParams) (protocol.StatePutResult, error) {
	unlock, err := s.collectionLock(p.Collection)
	if err != nil {
		return protocol.StatePutResult{}, err
	}
	defer unlock()
	cur, err := s.readDoc(p.Collection, p.Key)
	if err != nil {
		return protocol.StatePutResult{}, err
	}
	if err := checkVersion(cur, p.ExpectedVersion); err != nil {
		return protocol.StatePutResult{}, named(err, p.Collection, p.Key)
	}
	// Keep the stream record of the last commit: the cut back of the stream
	// on a read uses it.
	var rec *files.StreamRecord
	if cur != nil {
		rec = cur.Commit
	}
	d, err := files.WriteDoc(s.stateRoot(), p.Collection, p.Key, p.Value, p.Attributes, rec)
	if err != nil {
		return protocol.StatePutResult{}, unavailable("The plugin cannot write the document %s / %s. Check the free space and the access of the data folder.", p.Collection, p.Key)
	}
	s.waiters.notify(p.Collection, p.Key)
	return protocol.StatePutResult{Version: d.Version, UpdatedAt: d.UpdatedAt}, nil
}

// checkVersion: nil expects no document; a version expects that version.
func checkVersion(cur *files.Doc, expected *string) error {
	var stored *string
	if cur != nil {
		v := cur.Version
		stored = &v
	}
	if expected == nil && stored == nil {
		return nil
	}
	if expected != nil && stored != nil && *expected == *stored {
		return nil
	}
	return protocol.ConflictError(stored)
}

// named gives a conflict error a message that names the document, never its
// value.
func named(err error, collection, key string) error {
	if e, ok := err.(*protocol.Error); ok {
		e.Message = fmt.Sprintf("The document %s / %s has another version than the expected one. Read it again, then retry.", collection, key)
	}
	return err
}

// Delete removes a document with the expected version.
func (s *Store) Delete(ctx context.Context, p protocol.StateDeleteParams) (protocol.StateDeleteResult, error) {
	unlock, err := s.collectionLock(p.Collection)
	if err != nil {
		return protocol.StateDeleteResult{}, err
	}
	defer unlock()
	cur, err := s.readDoc(p.Collection, p.Key)
	if err != nil {
		return protocol.StateDeleteResult{}, err
	}
	if cur == nil {
		return protocol.StateDeleteResult{}, protocol.NewError(protocol.NotFound, "The document %s / %s does not exist.", p.Collection, p.Key)
	}
	expected := p.ExpectedVersion
	if err := checkVersion(cur, &expected); err != nil {
		return protocol.StateDeleteResult{}, named(err, p.Collection, p.Key)
	}
	// A commit wrote the document: its streams no longer have a committing
	// document. Remove each marker that names the document first (the
	// stream of the last commit, and an older one that a crash left), so a
	// crash between the two steps cuts back no committed event.
	if err := s.releaseDoc(p.Collection, p.Key, ""); err != nil {
		return protocol.StateDeleteResult{}, err
	}
	path := files.DocPath(s.stateRoot(), p.Collection, p.Key)
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return protocol.StateDeleteResult{}, unavailable("The plugin cannot remove the document %s / %s.", p.Collection, p.Key)
	}
	files.SyncDir(files.CollectionDir(s.stateRoot(), p.Collection))
	s.waiters.notify(p.Collection, p.Key)
	return protocol.StateDeleteResult{Deleted: true}, nil
}

// List reads each document of the collection, filters on the attributes,
// sorts by key in byte order, and gives one page after the cursor. The cursor
// is the last key of the page, in base64url.
func (s *Store) List(ctx context.Context, p protocol.StateListParams) (protocol.StateListResult, error) {
	pageSize := p.PageSize
	if pageSize <= 0 {
		pageSize = protocol.DefaultPageSize
	}
	var after string
	if p.Cursor != nil && *p.Cursor != "" {
		b, err := base64.RawURLEncoding.DecodeString(*p.Cursor)
		if err != nil || len(b) == 0 {
			return protocol.StateListResult{}, protocol.NewError(protocol.Refused, "The list cursor is not a cursor of this plugin. Start the list again with no cursor.")
		}
		after = string(b)
	}
	dir := files.CollectionDir(s.stateRoot(), p.Collection)
	entries, err := os.ReadDir(dir)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return protocol.StateListResult{}, unavailable("The plugin cannot read the collection %s.", p.Collection)
	}
	var docs []*files.Doc
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, files.DocExt) || strings.HasPrefix(name, ".") {
			continue
		}
		d, err := files.ReadDoc(dir + "/" + name)
		if err != nil {
			return protocol.StateListResult{}, unavailable("The plugin cannot read the file %s of the collection %s. Correct or remove it.", name, p.Collection)
		}
		if d == nil || d.Key <= after || !matches(d.Attributes, p.Filter) {
			continue
		}
		docs = append(docs, d)
	}
	sort.Slice(docs, func(i, j int) bool { return docs[i].Key < docs[j].Key })
	res := protocol.StateListResult{Items: []protocol.Document{}}
	for i, d := range docs {
		if i == pageSize {
			c := base64.RawURLEncoding.EncodeToString([]byte(docs[i-1].Key))
			res.Cursor = &c
			break
		}
		item := protocol.Document{Key: d.Key, Attributes: d.Attributes, Version: d.Version, UpdatedAt: d.UpdatedAt.In(time.UTC)}
		if p.WithValues {
			item.Value = d.Value
		}
		res.Items = append(res.Items, item)
	}
	return res, nil
}

// matches is true when each attribute of the filter has the same value in the
// document. A missing attribute has the value null.
func matches(attrs, filter map[string]any) bool {
	for k, want := range filter {
		got, ok := attrs[k]
		if !ok {
			got = nil
		}
		if !reflect.DeepEqual(got, want) {
			return false
		}
	}
	return true
}

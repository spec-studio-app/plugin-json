package files

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

// Doc is the file of one document: state/<collection>/<key>.json.
type Doc struct {
	Key        string          `json:"key"`
	Value      json.RawMessage `json:"value"`
	Attributes map[string]any  `json:"attributes"`
	Version    string          `json:"version"`
	UpdatedAt  time.Time       `json:"updatedAt"`
	// Commit is the stream that state.commit last appended to for this
	// document, and the length of that stream after the append. It is nil
	// for a document that no commit wrote.
	Commit *StreamRecord `json:"commit,omitempty"`
}

// StreamRecord is the stream of a commit and its length (the number of
// events, deleted events included) after the commit.
type StreamRecord struct {
	Stream string `json:"stream"`
	Length int64  `json:"length"`
}

// DocExt ends the name of each document file.
const DocExt = ".json"

// CollectionDir is the folder of a collection under the state folder.
func CollectionDir(stateRoot, collection string) string {
	return filepath.Join(stateRoot, FileName(collection))
}

// DocPath is the file of a document under the state folder.
func DocPath(stateRoot, collection, key string) string {
	return filepath.Join(CollectionDir(stateRoot, collection), FileName(key)+DocExt)
}

// NewVersion gives a new version: 16 random bytes in hex. It is never used
// again, also after a delete and across processes.
func NewVersion() string { return randomHex(16) }

// ErrBadDoc is the error of a document file that is not valid.
var ErrBadDoc = errors.New("the document file is not valid")

// ReadDoc reads a document file. It returns nil and no error when the file
// does not exist.
func ReadDoc(path string) (*Doc, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var d Doc
	if err := json.Unmarshal(data, &d); err != nil || d.Key == "" || d.Version == "" {
		return nil, ErrBadDoc
	}
	if d.Attributes == nil {
		d.Attributes = map[string]any{}
	}
	return &d, nil
}

// WriteDoc writes a document file with a new version and the time now. The
// caller holds the lock of the collection. rec is the stream record to keep
// in the file, or nil.
func WriteDoc(stateRoot, collection, key string, value json.RawMessage, attrs map[string]any, rec *StreamRecord) (*Doc, error) {
	if attrs == nil {
		attrs = map[string]any{}
	}
	d := &Doc{Key: key, Value: value, Attributes: attrs, Version: NewVersion(), UpdatedAt: time.Now().UTC(), Commit: rec}
	data, err := json.Marshal(d)
	if err != nil {
		return nil, err
	}
	if err := WriteAtomic(DocPath(stateRoot, collection, key), data); err != nil {
		return nil, err
	}
	return d, nil
}

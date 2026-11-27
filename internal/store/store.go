// Package store is the storage of plugin-json: documents as JSON files,
// blobs as files named by SHA-256, and waits on documents, in one data folder.
package store

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sync"

	"github.com/spec-studio-app/plugin-json/internal/files"
	"github.com/spec-studio-app/plugin-json/internal/legacy"
	"github.com/spec-studio-app/plugin-sdk/go/plugin"
	"github.com/spec-studio-app/plugin-sdk/go/protocol"
)

// SettingDataDir is the name of the data folder setting.
const SettingDataDir = "data_dir"

// Layout is the content of layout.json: the folder has the plugin's layout.
type Layout struct {
	Layout  string `json:"layout"`
	Version int    `json:"version"`
}

// LayoutName and LayoutVersion are the layout that this plugin writes.
const (
	LayoutName    = "plugin-json"
	LayoutVersion = 1
)

// Store is the plugin's store. It implements plugin.Store, plugin.BlobStore,
// plugin.Watcher and plugin.Durable.
type Store struct {
	dir     string
	temp    bool
	log     *slog.Logger
	mu      sync.Mutex
	locks   map[string]*sync.Mutex
	waiters *hub
	// getenv and home are for the tests of the default data folder.
	getenv func(string) string
	home   func() (string, error)
}

// New makes a store. The SDK calls Open at the first handshake.
func New() *Store {
	return &Store{locks: map[string]*sync.Mutex{}, waiters: newHub(), getenv: os.Getenv, home: os.UserHomeDir}
}

// Durable is true: the data stays after the plugin stops.
func (s *Store) Durable() bool { return true }

// Dir is the data folder, after Open.
func (s *Store) Dir() string { return s.dir }

func (s *Store) stateRoot() string { return filepath.Join(s.dir, "state") }

// DefaultDataDir gives the data folder for a host kind when the setting has
// no value: /data for the relay (the relay's RELAY_DATA_DIR default), the
// app's support folder for the app, and "" for a test host (a new temporary
// folder).
func DefaultDataDir(kind string, getenv func(string) string, home func() (string, error)) (string, error) {
	switch kind {
	case "relay":
		return "/data", nil
	case "app":
		if support := getenv("SPEC_STUDIO_SUPPORT"); support != "" {
			return filepath.Join(support, "backend", "json"), nil
		}
		h, err := home()
		if err != nil {
			return "", err
		}
		return filepath.Join(h, "Library", "Application Support", "Spec Studio", "backend", "json"), nil
	default:
		return "", nil
	}
}

func dataDirError(dir string) error {
	return protocol.NewError(protocol.BadSettings,
		"The plugin cannot write to data_dir (%s). Give the plugin's user write access to the folder, or set data_dir to a folder that it can write.", dir)
}

// Open finds the data folder, checks that it takes a write, imports the
// relay's old JSON store when the folder has one, and removes the temporary
// files that a crash left.
func (s *Store) Open(ctx context.Context, o plugin.OpenOptions) (err error) {
	s.temp, s.dir = false, ""
	s.log = o.Log
	if s.log == nil {
		s.log = slog.New(slog.NewTextHandler(os.Stderr, nil))
	}
	dir := o.Settings[SettingDataDir]
	if dir == "" {
		d, err := DefaultDataDir(o.Host.Kind, s.getenv, s.home)
		if err != nil {
			return protocol.NewError(protocol.BadSettings, "The plugin cannot find the home folder for the default data_dir. Set data_dir.")
		}
		dir = d
	}
	if dir == "" {
		d, err := os.MkdirTemp("", "plugin-json-test-*")
		if err != nil {
			return protocol.NewError(protocol.Unavailable, "The plugin cannot make a temporary data folder for the test host. Set data_dir.")
		}
		dir, s.temp = d, true
		defer func() {
			if err != nil {
				os.RemoveAll(d)
				s.temp, s.dir = false, ""
			}
		}()
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return dataDirError(dir)
	}
	dir = abs
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return dataDirError(dir)
	}
	probe := filepath.Join(dir, ".probe-"+files.NewVersion())
	if err := files.WriteAtomic(probe, []byte("ok")); err != nil {
		return dataDirError(dir)
	}
	os.Remove(probe)
	s.dir = dir

	// The folder lock keeps a second process out while this one imports.
	unlock, err := files.LockFile(filepath.Join(dir, ".lock"))
	if err != nil {
		return protocol.NewError(protocol.Unavailable, "The plugin cannot lock its data folder (%s). Put the data folder on a local disk.", dir)
	}
	defer unlock()
	if err := s.prepare(); err != nil {
		return err
	}
	s.log.Info("open", "dataDir", dir)
	return nil
}

func (s *Store) prepare() error {
	layoutPath := filepath.Join(s.dir, "layout.json")
	data, err := os.ReadFile(layoutPath)
	switch {
	case err == nil:
		var l Layout
		if json.Unmarshal(data, &l) != nil || l.Layout != LayoutName {
			return protocol.NewError(protocol.Unavailable, "The file layout.json in the data folder (%s) is not valid. Correct it, or use another data folder.", s.dir)
		}
		if l.Version > LayoutVersion {
			return protocol.NewError(protocol.Unavailable, "A newer plugin-json wrote the data folder (%s), layout version %d. Update plugin-json.", s.dir, l.Version)
		}
		if legacy.HasOld(s.dir) {
			s.log.Info("legacy_files_ignored", "reason", "the data folder has the plugin layout already")
		}
	case errors.Is(err, fs.ErrNotExist):
		layout, _ := json.Marshal(Layout{Layout: LayoutName, Version: LayoutVersion})
		if err := legacy.Import(s.dir, s.log, layout); err != nil {
			return err
		}
	default:
		return protocol.NewError(protocol.Unavailable, "The plugin cannot read layout.json in the data folder (%s).", s.dir)
	}
	if err := files.SweepTemps(s.stateRoot()); err != nil {
		return protocol.NewError(protocol.Unavailable, "The plugin cannot clean the temporary files in its data folder (%s).", s.dir)
	}
	if err := files.SweepTemps(filepath.Join(s.dir, "blobs")); err != nil {
		return protocol.NewError(protocol.Unavailable, "The plugin cannot clean the temporary files in its data folder (%s).", s.dir)
	}
	return nil
}

// Close ends the waits. A temporary data folder of a test host goes away.
func (s *Store) Close(ctx context.Context) error {
	s.waiters.closeAll()
	if s.temp && s.dir != "" {
		return os.RemoveAll(s.dir)
	}
	return nil
}

// collectionLock takes the lock of a collection: a mutex in this process and
// a flock for other processes.
func (s *Store) collectionLock(collection string) (func(), error) {
	s.mu.Lock()
	m := s.locks[collection]
	if m == nil {
		m = &sync.Mutex{}
		s.locks[collection] = m
	}
	s.mu.Unlock()
	m.Lock()
	unlock, err := files.LockFile(filepath.Join(files.CollectionDir(s.stateRoot(), collection), ".lock"))
	if err != nil {
		m.Unlock()
		return nil, unavailable("The plugin cannot lock the collection %s.", collection)
	}
	return func() {
		unlock()
		m.Unlock()
	}, nil
}

func unavailable(format string, args ...any) error {
	return protocol.NewError(protocol.Unavailable, format, args...)
}

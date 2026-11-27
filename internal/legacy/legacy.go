// Package legacy imports the relay's old JSON store (relay 0.6 and older,
// src/store/json in spec-studio-app/relay) into the plugin's layout, once.
//
// The old layout has no versions and no attributes, and names its spec files
// by a hash, so the plugin cannot use it as it is. The import copies the old
// files to a backup folder, writes one document for each old record with the
// names of PROTOCOL.md section 14, marks the folder with layout.json, and only
// then removes the old files. A crash before the mark leaves the old files, so
// the next start does the import again.
//
// This is the only place where the plugin knows a Spec Studio format: the
// version-1 files of the relay, which no longer change.
package legacy

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/spec-studio-app/plugin-json/internal/files"
	"github.com/spec-studio-app/plugin-sdk/go/protocol"
)

// The old files and folders, relative to the data folder.
const (
	oldConfig   = "config.json"
	oldPairCode = "pair-code.json"
	oldClosed   = "closed.json"
	oldSpecs    = "specs"
	oldDesks    = "desks"
)

var oldNames = []string{oldConfig, oldPairCode, oldClosed, oldSpecs, oldDesks}

// BackupPrefix starts the name of each backup folder.
const BackupPrefix = "backup-relay-json-"

const partialSuffix = ".partial"

// AfterState is a test hook. It runs after the documents are in state/ and
// before layout.json. An error stops the import there, as a crash does.
var AfterState func() error

// Now is the clock of the backup name (tests set it).
var Now = time.Now

// Getenv reads the environment (tests set it).
var Getenv = os.Getenv

// RelayDeployID is the relay's deploy ID, with the relay's own rule:
// DEPLOY_ID with no space around it, or "default" when it is empty.
func RelayDeployID() string {
	if v := strings.TrimSpace(Getenv("DEPLOY_ID")); v != "" {
		return v
	}
	return "default"
}

// HasOld is true when the data folder has a file or a folder of the old store.
func HasOld(dir string) bool {
	for _, name := range oldNames {
		if _, err := os.Lstat(filepath.Join(dir, name)); err == nil {
			return true
		}
	}
	return false
}

// Result counts what an import did.
type Result struct {
	Specs, Desks, ClosedSpecs, Configs, Skipped int
	Backup                                      string
}

// Import runs at an open, under the folder lock, when the data folder has no
// layout.json. A folder with no old file is new: Import only writes layout.
// Else it imports the old store and writes layout.
func Import(dir string, log *slog.Logger, layout []byte) error {
	layoutPath := filepath.Join(dir, "layout.json")
	if !HasOld(dir) {
		if err := files.WriteAtomic(layoutPath, layout); err != nil {
			return unavailable("The plugin cannot write layout.json in its data folder (%s).", dir)
		}
		return nil
	}
	res, err := run(dir, layout)
	if err != nil {
		return err
	}
	log.Info("legacy_import",
		"specs", res.Specs, "desks", res.Desks, "closedSpecs", res.ClosedSpecs,
		"configs", res.Configs, "skipped", res.Skipped, "backup", res.Backup)
	return nil
}

func unavailable(format string, args ...any) error {
	return protocol.NewError(protocol.Unavailable, format, args...)
}

func badFile(rel, why string) error {
	return unavailable("The plugin cannot import the relay's old file %s: %s. Correct the file, or remove it, then start the plugin again.", rel, why)
}

// doc is one document to write.
type doc struct {
	collection, key string
	value           json.RawMessage
	attrs           map[string]any
	from            string
}

func run(dir string, layout []byte) (Result, error) {
	var res Result
	// 1. Remove what an import that stopped left.
	for _, p := range []string{filepath.Join(dir, ".import"), filepath.Join(dir, "state")} {
		if err := os.RemoveAll(p); err != nil {
			return res, unavailable("The plugin cannot clean an earlier import in its data folder (%s).", dir)
		}
	}
	if partials, _ := filepath.Glob(filepath.Join(dir, BackupPrefix+"*"+partialSuffix)); len(partials) > 0 {
		for _, p := range partials {
			os.RemoveAll(p)
		}
	}

	// 2. Read and check every old file. Nothing changes before this ends.
	docs, skipped, err := readOld(dir)
	if err != nil {
		return res, err
	}
	res.Skipped = skipped
	for _, d := range docs {
		switch d.collection {
		case "specs":
			res.Specs++
		case "desks":
			res.Desks++
		case "closed-specs":
			res.ClosedSpecs++
		case "relay":
			res.Configs++
		}
	}

	// 3. Copy the old files to the backup folder.
	name := BackupPrefix + Now().UTC().Format("20060102T150405Z")
	if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
		name += "-" + files.NewVersion()[:6]
	}
	partial := filepath.Join(dir, name+partialSuffix)
	for _, old := range oldNames {
		if err := copyTree(filepath.Join(dir, old), filepath.Join(partial, old)); err != nil {
			return res, unavailable("The plugin cannot copy the relay's old files to a backup in its data folder (%s). Check the free space.", dir)
		}
	}
	if err := os.Rename(partial, filepath.Join(dir, name)); err != nil {
		return res, unavailable("The plugin cannot make the backup folder %s.", name)
	}
	files.SyncDir(dir)
	res.Backup = name

	// 4. Write the documents in .import/state, then move it to state.
	staging := filepath.Join(dir, ".import", "state")
	for _, d := range docs {
		if _, err := files.WriteDoc(staging, d.collection, d.key, d.value, d.attrs); err != nil {
			return res, unavailable("The plugin cannot write the imported document %s / %s.", d.collection, d.key)
		}
	}
	if err := os.MkdirAll(staging, 0o700); err != nil {
		return res, unavailable("The plugin cannot write in its data folder (%s).", dir)
	}
	if err := os.Rename(staging, filepath.Join(dir, "state")); err != nil {
		return res, unavailable("The plugin cannot move the imported documents in its data folder (%s).", dir)
	}
	os.RemoveAll(filepath.Join(dir, ".import"))
	files.SyncDir(dir)
	if AfterState != nil {
		if err := AfterState(); err != nil {
			return res, err
		}
	}

	// 5. Mark the folder, then remove the old files.
	if err := files.WriteAtomic(filepath.Join(dir, "layout.json"), layout); err != nil {
		return res, unavailable("The plugin cannot write layout.json in its data folder (%s).", dir)
	}
	for _, old := range oldNames {
		os.RemoveAll(filepath.Join(dir, old))
	}
	files.SyncDir(dir)
	return res, nil
}

// copyTree copies a file or a folder. A missing source is not an error.
func copyTree(src, dst string) error {
	info, err := os.Lstat(src)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return copyFile(src, dst, info.Mode().Perm())
	}
	return filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, path)
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o700)
		}
		if !d.Type().IsRegular() {
			return nil
		}
		i, err := d.Info()
		if err != nil {
			return err
		}
		return copyFile(path, target, i.Mode().Perm())
	})
}

func copyFile(src, dst string, perm fs.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, perm|0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	if err := out.Sync(); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// ---- the old files ----

type configFile struct {
	Version    int             `json:"version"`
	State      json.RawMessage `json:"state"`
	PairCodeID *string         `json:"pairCodeId"`
}

type configState struct {
	DeployID string            `json:"deployId"`
	Agents   []json.RawMessage `json:"agents"`
}

type pairCodeFile struct {
	Version   int      `json:"version"`
	ID        *string  `json:"id"`
	Hash      *string  `json:"hash"`
	ExpiresAt *float64 `json:"expiresAt"`
}

type inboxes struct {
	Mac struct {
		Messages []json.RawMessage `json:"messages"`
	} `json:"mac"`
	Coordinator struct {
		Messages []struct {
			Seq float64 `json:"seq"`
		} `json:"messages"`
		ReadUpTo float64 `json:"readUpTo"`
	} `json:"coordinator"`
}

func (i inboxes) pendingForMac() bool { return len(i.Mac.Messages) > 0 }

func (i inboxes) pendingForCoordinator() bool {
	for _, m := range i.Coordinator.Messages {
		if m.Seq > i.Coordinator.ReadUpTo {
			return true
		}
	}
	return false
}

type specFile struct {
	Version int `json:"version"`
	State   *struct {
		inboxes
		DeployID string `json:"deployId"`
		Spec     struct {
			RepoKey string `json:"repoKey"`
			Slug    string `json:"slug"`
		} `json:"spec"`
		State map[string]json.RawMessage `json:"state"`
	} `json:"state"`
	LastActivityAt float64 `json:"lastActivityAt"`
}

type deskFile struct {
	Version int `json:"version"`
	State   *struct {
		inboxes
		DeployID string `json:"deployId"`
		Desk     struct {
			RepoKey string `json:"repoKey"`
		} `json:"desk"`
	} `json:"state"`
}

type closedFile struct {
	Version int               `json:"version"`
	Closed  []json.RawMessage `json:"closed"`
}

type closedItem struct {
	RepoKey  string   `json:"repoKey"`
	Slug     string   `json:"slug"`
	ClosedAt *float64 `json:"closedAt"`
}

// stage is the text of state.state.stage, or "" (as the relay's mirrorFields).
func stage(state map[string]json.RawMessage) string {
	var s string
	if raw, ok := state["stage"]; ok && json.Unmarshal(raw, &s) == nil {
		return s
	}
	return ""
}

// compact gives the file's JSON with no extra space. The value is the same
// JSON as the file.
func compact(data []byte) json.RawMessage {
	var b bytes.Buffer
	if json.Compact(&b, data) != nil {
		return json.RawMessage(data)
	}
	return json.RawMessage(b.Bytes())
}

// jsonFiles lists the *.json files of an old folder, as the relay loads them.
func jsonFiles(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var names []string
	for _, e := range entries {
		if e.Type().IsRegular() && strings.HasSuffix(e.Name(), ".json") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	return names, nil
}

func readOld(dir string) ([]doc, int, error) {
	var docs []doc
	// The relay loads the files of its own deploy ID: DEPLOY_ID, trimmed, or
	// "default" (src/config.ts). A relay starts the plugin as its child, so the
	// plugin has the relay's environment.
	deployID := RelayDeployID()

	// Each old file must be readable, else the backup cannot copy it. A file
	// that does not parse is another case, below.
	for _, name := range []string{oldConfig, oldPairCode, oldClosed} {
		if f, err := os.Open(filepath.Join(dir, name)); err == nil {
			f.Close()
		} else if !errors.Is(err, fs.ErrNotExist) {
			return nil, 0, badFile(name, "the plugin cannot read it")
		}
	}

	// relay / config, with a pair-code.json that the relay did not take yet.
	if data, err := os.ReadFile(filepath.Join(dir, oldConfig)); err == nil {
		var f configFile
		var st configState
		if json.Unmarshal(data, &f) != nil || f.Version != 1 || len(f.State) == 0 ||
			json.Unmarshal(f.State, &st) != nil || st.Agents == nil {
			return nil, 0, badFile(oldConfig, "it is not a version-1 relay config file")
		}
		value := compact(data)
		if pc, err := os.ReadFile(filepath.Join(dir, oldPairCode)); err == nil {
			var p pairCodeFile
			// The relay counts a pair-code.json that does not parse as none.
			valid := json.Unmarshal(pc, &p) == nil && p.Version == 1 && p.ID != nil && p.Hash != nil && p.ExpiresAt != nil
			if valid && (f.PairCodeID == nil || *f.PairCodeID != *p.ID) {
				v, err := applyPairCode(data, p)
				if err != nil {
					return nil, 0, badFile(oldConfig, "it is not a version-1 relay config file")
				}
				value = v
			}
		}
		docs = append(docs, doc{collection: "relay", key: "config", value: value, attrs: map[string]any{}, from: oldConfig})
	} else if !errors.Is(err, fs.ErrNotExist) {
		return nil, 0, badFile(oldConfig, "the plugin cannot read it")
	}

	// specs / <repoKey>:<slug>
	type specDoc struct {
		doc
		deployID string
		last     float64
	}
	specs := map[string]specDoc{}
	skipped := 0
	names, err := jsonFiles(filepath.Join(dir, oldSpecs))
	if err != nil {
		return nil, 0, unavailable("The plugin cannot read the folder specs in its data folder (%s).", dir)
	}
	for _, name := range names {
		rel := oldSpecs + "/" + name
		data, err := os.ReadFile(filepath.Join(dir, oldSpecs, name))
		if err != nil {
			return nil, 0, badFile(rel, "the plugin cannot read it")
		}
		var f specFile
		if json.Unmarshal(data, &f) != nil {
			return nil, 0, badFile(rel, "it is not valid JSON")
		}
		if f.Version != 1 || f.State == nil || f.State.Spec.RepoKey == "" || f.State.Spec.Slug == "" {
			return nil, 0, badFile(rel, "it is not a version-1 relay spec file")
		}
		key := f.State.Spec.RepoKey + ":" + f.State.Spec.Slug
		if !protocol.ValidName(key) {
			return nil, 0, badFile(rel, "its repo key and slug do not make a valid document key")
		}
		d := specDoc{
			doc: doc{collection: "specs", key: key, value: compact(data), from: rel, attrs: map[string]any{
				"repoKey":               f.State.Spec.RepoKey,
				"deployId":              f.State.DeployID,
				"open":                  stage(f.State.State) != "merged",
				"pendingForMac":         f.State.pendingForMac(),
				"pendingForCoordinator": f.State.pendingForCoordinator(),
			}},
			deployID: f.State.DeployID,
			last:     f.LastActivityAt,
		}
		// The relay loads only the files of its own deploy ID; the config
		// holds it. Other files stay in the backup only.
		if deployID != "" && d.deployID != deployID {
			skipped++
			continue
		}
		if old, ok := specs[key]; ok {
			skipped++
			if d.last <= old.last {
				continue
			}
		}
		specs[key] = d
	}
	keys := make([]string, 0, len(specs))
	for k := range specs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		docs = append(docs, specs[k].doc)
	}

	// desks / <repoKey>
	type deskDoc struct {
		doc
		deployID string
	}
	desks := map[string]deskDoc{}
	names, err = jsonFiles(filepath.Join(dir, oldDesks))
	if err != nil {
		return nil, 0, unavailable("The plugin cannot read the folder desks in its data folder (%s).", dir)
	}
	for _, name := range names {
		rel := oldDesks + "/" + name
		data, err := os.ReadFile(filepath.Join(dir, oldDesks, name))
		if err != nil {
			return nil, 0, badFile(rel, "the plugin cannot read it")
		}
		var f deskFile
		if json.Unmarshal(data, &f) != nil {
			return nil, 0, badFile(rel, "it is not valid JSON")
		}
		if f.Version != 1 || f.State == nil || f.State.Desk.RepoKey == "" {
			return nil, 0, badFile(rel, "it is not a version-1 relay desk file")
		}
		key := f.State.Desk.RepoKey
		if !protocol.ValidName(key) {
			return nil, 0, badFile(rel, "its repo key is not a valid document key")
		}
		d := deskDoc{doc: doc{collection: "desks", key: key, value: compact(data), from: rel, attrs: map[string]any{
			"repoKey":               key,
			"deployId":              f.State.DeployID,
			"pendingForMac":         f.State.pendingForMac(),
			"pendingForCoordinator": f.State.pendingForCoordinator(),
		}}, deployID: f.State.DeployID}
		if deployID != "" && d.deployID != deployID {
			skipped++
			continue
		}
		if _, ok := desks[key]; ok {
			skipped++
			continue
		}
		desks[key] = d
	}
	keys = keys[:0]
	for k := range desks {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		docs = append(docs, desks[k].doc)
	}

	// closed-specs / <repoKey>:<slug>. The relay ignores a closed.json that
	// does not parse (it only logs), so the import does the same.
	if data, err := os.ReadFile(filepath.Join(dir, oldClosed)); err == nil {
		var f closedFile
		if json.Unmarshal(data, &f) == nil && f.Version == 1 {
			seen := map[string]int{}
			for _, raw := range f.Closed {
				var c closedItem
				if json.Unmarshal(raw, &c) != nil || c.RepoKey == "" || c.Slug == "" || c.ClosedAt == nil {
					skipped++
					continue
				}
				key := c.RepoKey + ":" + c.Slug
				if !protocol.ValidName(key) {
					skipped++
					continue
				}
				d := doc{collection: "closed-specs", key: key, value: compact(raw), from: oldClosed,
					attrs: map[string]any{"repoKey": c.RepoKey, "closedAt": *c.ClosedAt}}
				if i, ok := seen[key]; ok {
					skipped++
					docs[i] = d
					continue
				}
				seen[key] = len(docs)
				docs = append(docs, d)
			}
		} else {
			skipped++
		}
	}
	return docs, skipped, nil
}

// applyPairCode sets state.pairCode and pairCodeId as the relay's
// JsonConfigStore.load does with a pair-code.json that it did not take yet.
func applyPairCode(config []byte, p pairCodeFile) (json.RawMessage, error) {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(config, &top); err != nil {
		return nil, err
	}
	var state map[string]json.RawMessage
	if err := json.Unmarshal(top["state"], &state); err != nil {
		return nil, err
	}
	code, err := json.Marshal(map[string]any{"hash": *p.Hash, "expiresAt": *p.ExpiresAt})
	if err != nil {
		return nil, err
	}
	state["pairCode"] = code
	if top["state"], err = marshalOrdered(state); err != nil {
		return nil, err
	}
	if top["pairCodeId"], err = json.Marshal(*p.ID); err != nil {
		return nil, err
	}
	return marshalOrdered(top)
}

// marshalOrdered writes an object with sorted keys (encoding/json sorts map
// keys), with the raw values as they are.
func marshalOrdered(m map[string]json.RawMessage) (json.RawMessage, error) {
	b, err := json.Marshal(m)
	if err != nil {
		return nil, fmt.Errorf("marshal: %w", err)
	}
	return b, nil
}

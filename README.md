# Spec Studio JSON plugin

`plugin-json` is the default backend plugin of Spec Studio. It keeps the data of a host in JSON files in one data folder. The hosts are the Spec Studio relay and the Spec Studio app on your Mac.

The plugin implements the protocol in [plugin-sdk](https://github.com/spec-studio-app/plugin-sdk). Read [`PROTOCOL.md`](https://github.com/spec-studio-app/plugin-sdk/blob/main/PROTOCOL.md) for the methods, the errors and the rules.

## What the plugin does

The plugin speaks protocol 1.0 and 1.1. It gives four capabilities of the protocol and the call `state.commit`:

| Capability or call | What the plugin does |
|---|---|
| `state` | It keeps each document in one JSON file, with its value, its attributes, its version and the time of its last write. `state.list` filters on the attributes. |
| `events` | It keeps each stream in one JSON Lines file: one event on each line, in order. `events.read` gives pages with `after`, `from`, `to`, `limit` and `more`. |
| `state.commit` (1.1) | It checks the version, writes the document and appends the events to the stream as one atomic step, or does none of them. The app writes each change of a spec with it: the spec's state and its activity. |
| `blobs` | It keeps each blob in one file. The name of the file is the SHA-256 of the blob. |
| `watch` | `watch.wait` on a document ends at the next change of the document. `watch.wait` on a stream ends at the next event. The relay uses this for the long poll of `read_messages`. |

The plugin keeps its data after it stops (`durable: true`).

In a 1.1 handshake, the plugin gives `storeId`: `json:<absolute data folder>`. Two plugin processes on the same data folder give the same `storeId`.

Each write is atomic. The plugin writes a temporary file in the same folder, flushes it, and renames it over the old file. A reader sees the old file or the new file, never a part of a file. After a crash, the next start removes the temporary files.

Each compare and swap is atomic, also between two processes on the same data folder. The plugin holds an OS file lock (`flock`) on the collection for each `state.put`, `state.delete` and `state.commit`, and an OS file lock on the stream for each append, read and delete of events. `state.commit` takes the lock of the collection first and then the lock of the stream, so two calls never wait on each other. Thus the relay and `relay pair-code`, or the app and the `studio` CLI, can use the same folder at the same time. A wait sees a change from another process within 250 ms.

### How `state.commit` stays atomic

A commit has two writes: the events and the document. The plugin does them in this order, under the two locks:

1. It checks the expected version. When it is false, the commit stops with `conflict` and writes nothing.
2. It appends the events to the stream file in one write, and flushes the file.
3. It writes the new document file and renames it over the old file. The document file records the stream and the length of the stream after the append. The rename is the moment of the commit.

A crash between step 2 and step 3 leaves events that no document records. The stream file has a marker that names the document that commits to it. At each start, and at each read of the stream under its lock, the plugin cuts the stream file back to the length that the document records. Thus a crash leaves the old state and no new event.

A stream with no committing document, for example the events of a desk, uses `events.append` only. The plugin never cuts it back. A part of a line that a crash left at the end of any stream file is not an event, and the plugin removes it.

The plugin never writes a value, a token, a token hash or a pairing code hash to its log or to an error message. An error message names the collection and the key only.

## Settings

| Setting | Required | Meaning |
|---|---|---|
| `data_dir` | No | The data folder. |

When `data_dir` has no value, the plugin uses the host kind from the handshake:

| Host kind | Data folder |
|---|---|
| `relay` | `/data`. This is the default of `RELAY_DATA_DIR` in the relay. |
| `app` | `$SPEC_STUDIO_SUPPORT/backend/json` when the variable is set. Else `~/Library/Application Support/Spec Studio/backend/json`. |
| `cli` | The same folder as `app`. The `studio` CLI and the app use one data folder. |
| `test` | A new temporary folder. The plugin removes it at shutdown. |

The plugin makes the folder when it does not exist. When the plugin cannot write to the folder, the handshake fails with `bad_settings`. Give the plugin's user write access to the folder, or set `data_dir` to another folder.

Put the data folder on a local disk or a block volume. Some network file systems do not give `flock`.

The flags of the Go SDK also apply:

```sh
plugin-json                                   # stdio: the host starts the plugin
plugin-json --listen 127.0.0.1:7070           # HTTP at /rpc, for a sidecar
plugin-json --listen 0.0.0.0:7070 --token "$PLUGIN_TOKEN"
```

## The data folder

```
<data_dir>/
  layout.json                      {"layout":"plugin-json","version":1}
  .lock                            the lock of the folder, held during an import only
  state/<collection>/.lock         the lock of a collection, held during one write
  state/<collection>/<key>.json    one document
  events/<stream>.jsonl            the events of one stream, one event on each line
  events/<stream>.lock             the lock of a stream, held during one append, read or delete
  events/<stream>.doc.json         the marker: the document that state.commit writes with the stream
  blobs/<aa>/<sha256>              one blob; <aa> is the first two characters of the hash
  backup-relay-json-<time>/        the old files of the relay, after an import
```

A document file has this shape:

```json
{
  "key": "1a2b3c4d5e6f:drop-spec",
  "value": { "…": "…" },
  "attributes": { "repoKey": "1a2b3c4d5e6f", "open": true },
  "version": "9f2c…",
  "updatedAt": "2026-11-27T10:00:00.123456Z",
  "commit": { "stream": "spec:1a2b3c4d5e6f:drop-spec", "length": 42 }
}
```

`commit` is in the file only after a `state.commit`. It is the stream of the last commit and the length of the stream after it. A `state.put` keeps it.

When a document commits to another stream, the plugin first removes the marker of the old stream. A `state.delete` removes each marker that names the document. Thus the events that the document committed to a stream always stay.

A line of a stream file has this shape:

```json
{"type":"activity","at":"2026-11-27T10:00:00Z","data":{"kind":"status"}}
```

The cursor of an event is its number in the stream, as a string: `"1"` for the first event that the stream ever had. A host must not read a meaning into a cursor. `events.delete` keeps the count in a first line `{"base":N}`, so a new event never gets the cursor of a deleted event, and the length that a document records stays true.

The name of a document file comes from its key, and the name of a stream file comes from its stream name. The characters `a-z 0-9 . _ -` stay. Each other character becomes `%XX` (its hex code). Thus `Ab` and `ab` are two files, also on a disk that ignores case. A name that is longer than 200 characters after this step becomes `%h<SHA-256 of the key>`. No other name gives a file name that starts with `%h`. The plugin always reads the key from the file, not from the file name.

A version is 16 random bytes in hex. The plugin never uses a version again for the same document.

Do not change the files while a plugin process uses the folder.

## The import of the relay's old store

The relay 0.6 and older keeps its data in its own JSON layout: `config.json`, `pair-code.json`, `closed.json`, `specs/` and `desks/`. That layout has no versions and no attributes, so the plugin cannot use it as it is.

When the plugin starts on a folder with the old files and with no `layout.json`, it imports the old files one time:

1. It reads and checks each old file. When `config.json`, a spec file or a desk file is not valid, the start stops with `unavailable`. The message names the file. Correct the file or remove it, then start the plugin again. The plugin changes no file in this case. A `closed.json` or a `pair-code.json` that does not parse does not stop the start: the relay ignores these files too, and the import skips them. An old file that the plugin cannot read (for example, no read access) stops the start, as it stops the relay.
2. It copies the old files to `backup-relay-json-<time>/` in the data folder.
3. It writes one document for each old record, with the names in section 14 of `PROTOCOL.md`.
4. It writes `layout.json`. Then it removes the old files.
5. It writes one log line, `legacy_import`, with the number of specs, desks, closed specs and configs, and the name of the backup folder.

A crash before step 4 leaves the old files. The next start does the import again. After step 4, the plugin does not import again. When old files come back next to `layout.json`, the plugin ignores them and logs `legacy_files_ignored`.

The documents of the import:

| Old file | Document | Value | Attributes |
|---|---|---|---|
| `config.json` | `relay` / `config` | The JSON object of the file. A `pair-code.json` that the relay did not take yet is applied first, as the relay does: `state.pairCode` and `pairCodeId` get its values. | None |
| `specs/<hash>.json` | `specs` / `<repoKey>:<slug>` | The JSON object of the file: `{ "version": 1, "state": …, "lastActivityAt": … }` | `repoKey`, `deployId`, `open`, `pendingForMac`, `pendingForCoordinator` |
| `desks/<hash>.json` | `desks` / `<repoKey>` | The JSON object of the file: `{ "version": 1, "state": … }` | `repoKey`, `deployId`, `pendingForMac`, `pendingForCoordinator` |
| Each item of `closed.json` | `closed-specs` / `<repoKey>:<slug>` | The item: `{ "repoKey", "slug", "closedAt", "memo" }` | `repoKey`, `closedAt` |

The attributes come from the same rules as in the relay:

- `open` is true when the stage of the spec is not `merged`.
- `pendingForMac` is true when a message waits for the Mac.
- `pendingForCoordinator` is true when a message for Claude AI has a `seq` above `readUpTo`.

The relay writes the attributes again at its next write to each document.

The relay loads only the spec files and desk files of its own deploy ID. The import does the same, with the relay's rule: the deploy ID is `DEPLOY_ID` with no space around it, or `default` when `DEPLOY_ID` is empty. The relay starts the plugin as its child process, so the plugin reads `DEPLOY_ID` from the same environment. For a sidecar, set the same `DEPLOY_ID` as the relay. The import skips the files of another deploy ID. When two spec files of that deploy ID have the same repo key and slug, the file with the later last activity wins. The log line counts the files and records that the import did not use. They stay in the backup.

To go back to an older relay, stop the relay. Then copy the files from the backup folder to the data folder and remove `layout.json` and `state/`.

`testdata/relay-v1/` is a data folder that the relay's own JSON store wrote. `scripts/make-relay-fixture.mjs` makes it from a relay checkout. The tests in `internal/legacy` import a copy of it and compare each document with `testdata/relay-v1.expected.json`.

## Conformance

The plugin passes the conformance runner of plugin-sdk over stdio and over HTTP. CI runs it on each push.

To run it on your computer, you need Go 1.24 and git:

```sh
scripts/conformance.sh stdio
scripts/conformance.sh http
```

The script builds the runner from the plugin-sdk commit in `go.mod`, with the schemas of the same commit. To use a plugin-sdk checkout that you have, set `SDK_DIR=<path to plugin-sdk>`.

You can also run the runner yourself. Set `data_dir`, because a test host with no `data_dir` gets a new temporary folder at each start, and the restart test then finds no data:

```sh
go build -o plugin-json .
plugin-conformance --schemas <plugin-sdk>/schema --setting data_dir=/tmp/plugin-json-test -- ./plugin-json
```

The runner runs the tests of `state.commit` (protocol 1.1) and of `events`. It skips the `continueStream` test hook, because a stream of this plugin is always one file, and `secret-canary`, because the plugin has no secret setting.

## Develop

```sh
go vet ./...
go test -race ./...
scripts/check-readme.sh
```

The `TestOldUpload` test waits for the upload sweep of the SDK, which runs one time each minute.

## Release

A tag `vX.Y.Z` on `main` starts `.github/workflows/release.yml`:

1. The tests run.
2. The workflow builds one static binary (`CGO_ENABLED=0`) for macOS arm64, macOS amd64, Linux arm64 and Linux amd64.
3. On a macOS runner, `scripts/universal.sh` joins the two macOS binaries with `lipo` into one universal binary (arm64 and x86_64).
4. It attaches `plugin-json_<X.Y.Z>_<os>_<arch>.tar.gz`, `plugin-json-<X.Y.Z>-darwin-universal.tar.gz` and `checksums.txt` to the GitHub release. `checksums.txt` has one SHA-256 line for each archive, the universal archive included.
5. It writes `Formula/plugin-json.rb` to [spec-studio-app/homebrew-tap](https://github.com/spec-studio-app/homebrew-tap). This step needs the secret `HOMEBREW_TAP_TOKEN`: a token that can push to the tap.

Then:

```sh
brew install spec-studio-app/tap/plugin-json
```

Each archive has one folder with the name of the archive (with no `.tar.gz`), and `plugin-json` and `README.md` in that folder. For example, `plugin-json-0.2.0-darwin-universal.tar.gz` has `plugin-json-0.2.0-darwin-universal/plugin-json`.

The relay's image copies the Linux binary from the release. The Spec Studio app puts the universal macOS binary in its bundle.

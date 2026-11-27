# Spec Studio JSON plugin

`plugin-json` is the default backend plugin of Spec Studio. It keeps the data of a host in JSON files in one data folder. The hosts are the Spec Studio relay and the Spec Studio app on your Mac.

The plugin implements the protocol in [plugin-sdk](https://github.com/spec-studio-app/plugin-sdk). Read [`PROTOCOL.md`](https://github.com/spec-studio-app/plugin-sdk/blob/main/PROTOCOL.md) for the methods, the errors and the rules.

## What the plugin does

The plugin gives three capabilities of the protocol:

| Capability | What the plugin does |
|---|---|
| `state` | It keeps each document in one JSON file, with its value, its attributes, its version and the time of its last write. `state.list` filters on the attributes. |
| `blobs` | It keeps each blob in one file. The name of the file is the SHA-256 of the blob. |
| `watch` | `watch.wait` on a document ends at the next change of the document. The relay uses this for the long poll of `read_messages`. |

The plugin does not give `events`. History and replay come with [plugin-temporal](https://github.com/spec-studio-app/plugin-temporal).

The plugin keeps its data after it stops (`durable: true`).

Each write is atomic. The plugin writes a temporary file in the same folder, flushes it, and renames it over the old file. A reader sees the old file or the new file, never a part of a file. After a crash, the next start removes the temporary files.

Each compare and swap is atomic, also between two processes on the same data folder. The plugin holds an OS file lock (`flock`) on the collection for each `state.put` and `state.delete`. Thus the relay and `relay pair-code` can use the same folder at the same time. A wait sees a change from another process within 250 ms.

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
  "updatedAt": "2026-11-27T10:00:00.123456Z"
}
```

The name of a document file comes from its key. The characters `a-z 0-9 . _ -` stay. Each other character becomes `%XX` (its hex code). Thus `Ab` and `ab` are two files, also on a disk that ignores case. A name that is longer than 200 characters after this step becomes `%h<SHA-256 of the key>`. No other name gives a file name that starts with `%h`. The plugin always reads the key from the file, not from the file name.

A version is 16 random bytes in hex. The plugin never uses a version again for the same document.

Do not change the files while a plugin process uses the folder.

## The import of the relay's old store

The relay 0.6 and older keeps its data in its own JSON layout: `config.json`, `pair-code.json`, `closed.json`, `specs/` and `desks/`. That layout has no versions and no attributes, so the plugin cannot use it as it is.

When the plugin starts on a folder with the old files and with no `layout.json`, it imports the old files one time:

1. It reads and checks each old file. When `config.json`, a spec file or a desk file is not valid, the start stops with `unavailable`. The message names the file. Correct the file or remove it, then start the plugin again. The plugin changes no file in this case. A `closed.json` or a `pair-code.json` that does not parse does not stop the start: the relay ignores these files too, and the import skips them. A `pair-code.json` that the plugin cannot read stops the start, as it stops the relay.
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

The relay loads only the spec files and desk files of its own deploy ID (`DEPLOY_ID`). The import does the same: it skips the files of another deploy ID. The relay starts the plugin as its child process, so the plugin reads `DEPLOY_ID` from the same environment. When `DEPLOY_ID` is not set (for example in a sidecar), the import uses the deploy ID in `config.json`, which is the deploy ID of the relay's first start. Set `DEPLOY_ID` for a sidecar when the relay has one. When no `config.json` exists and two spec files have the same repo key and slug, the file with the later last activity wins. The log line counts the files and records that the import did not use. They stay in the backup.

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

The runner skips the tests of `events`, because the plugin does not give `events`.

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
3. It attaches `plugin-json_<X.Y.Z>_<os>_<arch>.tar.gz` and `checksums.txt` to the GitHub release.
4. It writes `Formula/plugin-json.rb` to [spec-studio-app/homebrew-tap](https://github.com/spec-studio-app/homebrew-tap). This step needs the secret `HOMEBREW_TAP_TOKEN`: a token that can push to the tap.

Then:

```sh
brew install spec-studio-app/tap/plugin-json
```

The relay's image copies the Linux binary from the release.

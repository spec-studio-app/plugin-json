# The JSON backend plugin

Spec Studio needs a default backend plugin that keeps a host's data in plain files, for the relay and for the Mac app. This change makes `plugin-json`: a Go program, built with the plugin-sdk, that gives the `state`, `blobs` and `watch` capabilities of the protocol over JSON files in one data folder. A relay that moves to it keeps all its data: on its first start, the plugin imports the relay's old JSON store into its own layout, keeps a backup, and logs what it did.

## Depends on

The spec `plugin-protocol` in spec-studio-app/plugin-sdk is merged (commit `394b06f` on `main`). The plugin follows `PROTOCOL.md` 1.0 as merged, uses the Go SDK in `go/`, and passes the conformance runner in `conformance/`. The plugin-sdk has no release tag yet, so the plugin uses the Go SDK at that commit and CI builds the runner from source at that commit.

## Scope

### In scope
- The `state` capability: documents as JSON files, with versions, attributes, list filters and pages, and an atomic compare and swap that also holds across two processes on the same folder.
- The `blobs` capability: files named by SHA-256 in the same data folder, with chunked uploads.
- The `watch` capability for documents, so the relay's long poll for `read_messages` works.
- The `data_dir` setting, with a default for each host kind.
- A one-time import of the relay's old JSON store (`config.json`, `specs/`, `desks/`, `closed.json`, `pair-code.json`) into the plugin's layout, with a backup and a log.
- Stdio and HTTP transports, from the SDK.
- Unit tests, the conformance runner over stdio and HTTP in CI, and an import test with a real relay data folder.
- A release workflow: on a tag, four static binaries on a GitHub release, and the Homebrew formula `plugin-json` written to spec-studio-app/homebrew-tap.
- A README in ASD-STE100 Simplified Technical English.

### Out of scope
- The `events` capability. History and replay come with plugin-temporal.
- The change in the relay that calls the plugin (spec `relay-plugin-host` in the relay repo), and the change in its image.
- The change in the app that calls the plugin (spec `backend-plugin-host` in the app repo).
- A license file.
- An import of the app's local files: the app sends its own data through the protocol.

## Requirements

### R1 Documents in JSON files
The plugin keeps each document of the `state` capability in one JSON file in the data folder, with its value, its attributes, its version and its time of change. A read after a restart gives the same document.

#### R1.S1 A document stays after a restart
- Given a plugin on an empty data folder
- When the host puts the document `specs` / `1a2b3c4d5e6f:drop-spec` with a value and the attribute `open: true`, and the plugin restarts
- Then `state.get` on that document gives the same value, the same attributes and the same version

#### R1.S2 A missing document is not an error
- Given a plugin on an empty data folder
- When the host calls `state.get` on `desks` / `none`
- Then the result is `{ "exists": false }`

#### R1.S3 A list filters on attributes and comes in key order
- Given the documents `specs` / `b`, `specs` / `a` with `open: true` and `specs` / `c` with `open: false`
- When the host calls `state.list` on `specs` with the filter `{ "open": true }` and a page size of 1
- Then the first page has `a` and a cursor, the second page has `b` and the cursor `null`

#### R1.S4 A bad name gets refused
- Given a plugin on a data folder
- When the host calls `state.put` with the key `../config` or the collection `.`
- Then the plugin answers `refused` and makes no file outside its data folder

#### R1.S5 The plugin says that it is durable
- Given a plugin on a data folder
- When the host calls `plugin.describe`
- Then the result has the capabilities `state`, `blobs` and `watch`, no `events`, the setting `data_dir`, and `durable: true`

### R2 Atomic writes and compare and swap
Each write goes to a temporary file in the same folder and then a rename replaces the old file, so a reader never sees half a file. The check of the expected version and the write are one atomic step, also when two plugin processes use the same data folder.

#### R2.S1 Two writes with the same expected version
- Given the document `relay` / `config` at version V
- When two calls of `state.put` with `expectedVersion: V` run at the same time
- Then exactly one succeeds, and the other gets `conflict` with `data.version` set to the new version

#### R2.S2 Two processes on one folder
- Given two plugin processes on the same data folder, as the relay and `relay pair-code` are
- When each process puts `relay` / `config` with the same expected version at the same time
- Then exactly one succeeds and the other gets `conflict`

#### R2.S3 A crash leaves no half file
- Given a write that stops after the temporary file is written and before the rename
- When the plugin starts again
- Then `state.get` gives the document as it was before that write, and the plugin removes the temporary file

#### R2.S4 A version is never used again
- Given a document that the host deletes and then makes again
- When the host reads it
- Then its version is not any version that the document had before

### R3 Blobs by SHA-256
The plugin keeps each blob as one file whose name is its SHA-256, in the same data folder. An upload goes in chunks and becomes a blob only when its size and hash are correct.

#### R3.S1 A chunked upload
- Given a file of 3 MiB
- When the host sends `blobs.begin`, three `blobs.write` calls and `blobs.commit`
- Then `blobs.has` lists its hash, and `blobs.get` in chunks gives the same bytes

#### R3.S2 A wrong hash keeps nothing
- Given an upload whose bytes do not match its `sha256`
- When the host sends `blobs.commit`
- Then the plugin answers `refused`, and `blobs.has` does not list the hash

#### R3.S3 An old upload ends
- Given an upload that gets no call for 10 minutes
- When the host sends `blobs.write` with its `uploadId`
- Then the plugin answers `not_found` and its partial bytes are gone from the data folder

### R4 Watch for documents
`watch.wait` on a document ends at the first change of its version, from this process or from another process on the same folder, or after the timeout. The plugin does not give `events`, so a wait on a stream gets `refused`.

#### R4.S1 A put ends a wait
- Given a host that waits on `specs` / `x` with its current version and a timeout of 30 s
- When another call puts `specs` / `x`
- Then the wait ends within 1 s with `changed: true` and the new version

#### R4.S2 A change from another process ends a wait
- Given a host that waits on `relay` / `config` in one plugin process
- When a second plugin process on the same folder puts `relay` / `config`
- Then the wait ends within 2 s with `changed: true`

#### R4.S3 A delete ends a wait
- Given a host that waits on `specs` / `x` with its current version
- When another call deletes `specs` / `x`
- Then the wait ends with `changed: true` and `version: null`

#### R4.S4 No change
- Given a host that waits on a document with a timeout of 200 ms
- When no call changes the document
- Then the wait ends after about 200 ms with `changed: false`

### R5 The data folder setting
The plugin reads its data folder from the setting `data_dir`. With no value, it uses the default of the host kind from the handshake. It makes the folder when it does not exist, and fails the handshake with a clear message when it cannot write there.

#### R5.S1 The relay's default
- Given a handshake from host kind `relay` with no `data_dir`
- When the plugin opens its storage
- Then it uses `/data`, the relay's current `RELAY_DATA_DIR` default

#### R5.S2 The app's default
- Given a handshake from host kind `app` with no `data_dir`
- When the plugin opens its storage
- Then it uses `~/Library/Application Support/Spec Studio/backend/json`, or `$SPEC_STUDIO_SUPPORT/backend/json` when that variable is set

#### R5.S3 A folder that the plugin cannot write
- Given a `data_dir` that the plugin's user cannot write
- When the host sends the handshake
- Then the handshake fails with `bad_settings`, and the message names `data_dir` and says to give the plugin's user write access to the folder

### R6 Import of the relay's old JSON store
On its first start on a folder that has the relay's old JSON store and no plugin layout, the plugin imports the old files into documents with the names of `PROTOCOL.md` section 14. It copies the old files to a backup folder first, logs what it did, and gives the same data back through the protocol. A relay that moves to the plugin keeps its pairing, its repos, its OAuth clients and tokens, its specs, its desks and its messages, with no step from the owner.

#### R6.S1 A real relay folder reads back the same
- Given a copy of a relay data folder with `config.json`, spec files, desk files and `closed.json`, made by the current relay's tests
- When the plugin starts on it
- Then `relay` / `config` has the old config, each old spec file is the document `specs` / `<repoKey>:<slug>`, each desk file is `desks` / `<repoKey>`, each closed record is `closed-specs` / `<repoKey>:<slug>`, and each value is the old file's content, unchanged

#### R6.S2 The attributes match what the relay filters on
- Given an imported spec with one message for the Mac, one unread message for Claude AI, and the stage `build`
- When the host lists `specs` with the filter `{ "pendingForMac": true, "open": true }`
- Then the list has the spec, with the attributes `repoKey`, `deployId`, `open`, `pendingForMac` and `pendingForCoordinator` as the relay computes them

#### R6.S3 A backup and a log
- Given a folder with the old store
- When the import ends
- Then the old files are in `backup-relay-json-<time>/` in the data folder, unchanged, and the log has one line for the import with the counts of specs, desks, closed specs and the config, and no secret and no hash

#### R6.S4 A second start does not import again
- Given a folder that the plugin imported before
- When the plugin starts again
- Then it imports nothing and logs no import

#### R6.S5 A crash during the import
- Given an import that stops before its end
- When the plugin starts again
- Then it does the import again from the old files, and no document shows twice

#### R6.S6 A file that the import cannot read
- Given a folder whose old spec file is not valid JSON
- When the plugin starts on it
- Then the handshake fails with `unavailable`, the message names the file and says to correct or remove it, and the plugin changes no file

### R7 No secret in a log
The plugin never writes a secret, a token, a token hash, a pairing code hash or a document value to its log or to an error message.

#### R7.S1 The import log
- Given an old `config.json` with agent hashes, OAuth clients and token hashes
- When the plugin imports it
- Then the plugin's stderr has none of those values

#### R7.S2 An error message
- Given a `conflict` or `refused` error on `relay` / `config`
- When the plugin answers
- Then the message names the collection and the key, and has no part of the value

### R8 Release and Homebrew
A tag `vX.Y.Z` builds one static binary for macOS arm64, macOS amd64, Linux arm64 and Linux amd64, attaches them to a GitHub release with their SHA-256 sums, and writes the formula `plugin-json` to spec-studio-app/homebrew-tap.

#### R8.S1 A tag makes a release
- Given a tag `v0.1.0` on `main` and passing tests
- When the release workflow runs
- Then the release `v0.1.0` has four archives, `plugin-json_0.1.0_<os>_<arch>.tar.gz`, and a `checksums.txt`

#### R8.S2 The binary is static
- Given the Linux amd64 binary of a release
- When a host runs it in a container image with no libc
- Then `plugin-json` answers `plugin.describe` over stdio

#### R8.S3 The Homebrew formula
- Given a release `v0.1.0`
- When the release workflow ends
- Then `Formula/plugin-json.rb` in spec-studio-app/homebrew-tap installs version 0.1.0 with the SHA-256 of each archive, and `brew install spec-studio-app/tap/plugin-json` gives a `plugin-json` on the path

### R9 CI and conformance
CI runs the unit tests with the race detector, and the plugin-sdk conformance runner over stdio and over HTTP, on each push and pull request.

#### R9.S1 Conformance over stdio
- Given the plugin binary built in CI
- When CI runs `plugin-conformance --schemas schema -- ./plugin-json` with a temporary `data_dir`
- Then every test passes or is skipped for the `events` capability, and the job passes

#### R9.S2 Conformance over HTTP
- Given the plugin running with `--listen 127.0.0.1:7070`
- When CI runs `plugin-conformance --url http://127.0.0.1:7070/rpc`
- Then every test passes or is skipped for the `events` capability, and the job passes

### R10 README
The README says what the plugin does, its settings, the data layout, the import of the relay's old store and its backup, and how to run the conformance runner. It links to plugin-sdk. It is in ASD-STE100 Simplified Technical English. The repo has no license file.

#### R10.S1 The README has the sections
- Given the merged README
- When a reader opens it
- Then it has sections for what the plugin does, the settings, the data layout, the import, conformance, and a link to plugin-sdk

## Edge cases

- **A name with `..` inside**, such as the slug `fix..login`: valid, and stays one file name.
- **A key with `:` and `@`**: valid names. The file name encodes each name so that it is safe on macOS (case-insensitive) and Linux: two keys that differ only in case are two files.
- **A document near the size limit**: the SDK refuses values over `maxMessageBytes` less 4 KiB; the plugin keeps the files that it accepts with no other limit.
- **A temporary file from a crash**: removed at start; never read as a document.
- **A folder with the plugin layout and old files too** (for example, a person copied old files back): the plugin uses its own layout and logs once that it ignores the old files.
- **`pair-code.json` from `relay pair-code`**: when the relay did not take it yet (its ID is not the config's `pairCodeId`), the import applies it as the relay does at its next call: the config gets its hash and expiry. Else the file goes to the backup only.
- **An expired closed record**: imported as it is; the host removes it after 30 days.
- **A shutdown during a write**: the write ends or does not start; no half file.
- **A disk that is full**: the write fails with `unavailable` and the old file stays.
- **Many waits on one document**: one change ends all of them.

## Assumptions

- The setting name is `data_dir`. The app passes it as `<support>/backend/json` (the app spec says so); the plugin has the same default for host kind `app`. Host kind `test` with no `data_dir` gets a new temporary folder.
- The plugin's own layout is a new folder tree in the data folder (`state/<collection>/<key>.json`, `blobs/`, `uploads/`, `layout.json`). The relay's old layout cannot carry versions, attributes and the protocol's document names (its spec files are named by a hash), so the plugin imports it once (the description allows this).
- The import knows the old relay file format, and computes the five attributes of section 14 from it (`repoKey`, `deployId`, `open` from the stage, `pendingForMac` from the Mac inbox, `pendingForCoordinator` from the unread coordinator messages). This is the only Spec Studio knowledge in the plugin, and only for the import of frozen version-1 files. The relay recomputes the attributes at its next write.
- An imported value is the old file's JSON object as it is (for example `{ "version": 1, "state": …, "lastActivityAt": … }` for a spec). The relay's adapter in `relay-plugin-host` reads this shape. The README documents it.
- Two processes share a folder with an OS file lock that the plugin holds only during each compare and swap, so `relay pair-code` can run while the relay runs. A watch sees a change from another process by a check of the file every 500 ms or less.
- The Homebrew formula is a template in this repo. The release workflow renders it and pushes it to the tap with a token secret (`HOMEBREW_TAP_TOKEN`). A person must add that secret and push the first tag.
- The Go version is 1.24, as in plugin-sdk. The binaries build with `CGO_ENABLED=0`.

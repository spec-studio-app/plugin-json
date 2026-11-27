# Plan: The JSON backend plugin

## Approach

`plugin-json` is one Go module, `github.com/spec-studio-app/plugin-json`, with `main.go` at the root, so `go install github.com/spec-studio-app/plugin-json@<tag>` also works. `main.go` calls `plugin.Main` of the plugin-sdk Go SDK with one setting, `data_dir`, and a store from `internal/store`. The SDK does the transports, the handshake, the settings check, the name check, the chunks of a blob upload and the shutdown. The plugin writes only the storage.

**Two small changes in plugin-sdk first.** The study of the merged SDK found two gaps:

1. `plugin.OpenOptions` has the settings and the log, but not the host from the handshake. R5 needs the host kind for the default data folder. The change adds `Host protocol.Host` to `OpenOptions`, and `Server.open` passes it. It only adds a field, so it is a minor change for other plugins.
2. The conformance test `watch-stream` needs only `watch`, but it calls `events.append`. A plugin with `watch` and no `events` (this plugin) fails it. The change skips the test when the plugin does not declare `events`.

Why not a workaround in plugin-json: to read the host kind without the SDK, the plugin would have to parse the handshake itself, around the SDK. And the conformance gap is a bug in the runner that every thin plugin would meet. plugin-sdk has no release tag yet, so plugin-json uses the SDK by commit (a Go pseudo-version). Before the plugin-json PR merges, its `go.mod` moves to the commit of the merged plugin-sdk PR.

**The data folder layout.**

```
<data_dir>/
  layout.json                      {"layout":"plugin-json","version":1}
  .lock                            the folder lock (flock), held only for an import
  state/<collection>/.lock         the collection lock (flock), held for each compare and swap
  state/<collection>/<file>.json   {"key","value","attributes","version","updatedAt"}
  blobs/<aa>/<sha256>              the bytes of a blob; <aa> is the first two hex characters
  backup-relay-json-<time>/        the old relay files, after an import
```

- **File names.** A name has `A-Z a-z 0-9 . _ : @ -`. The file name keeps `a-z 0-9 . _ -` and writes each other byte as `%XX`, so `Ab` and `ab` are two files on a macOS disk that ignores case, and `:` never reaches the Finder. When the result is longer than 200 bytes (a key of 200 characters with many capitals), the file name is `h-<sha256 of the name>.json`. The file always has its key inside, so a list reads keys from the files, not from the names.
- **Versions.** Each write makes a new version: 16 random bytes in hex. A version is never used again, also after a delete, also across processes (R2.S4).
- **Atomic write.** The write goes to `<file>.<pid>.<random>.tmp` in the same folder, with `fsync`, then `rename`, then an `fsync` of the folder: the same steps as the relay's `writeAtomic`. At open, the plugin removes each `*.tmp` file (R2.S3).
- **Compare and swap.** `Put` and `Delete` take an exclusive `flock` on `state/<collection>/.lock`, read the stored version, check it, write, and let go of the lock. `flock` locks are per open file, so they keep two goroutines apart and two processes apart (R2.S1, R2.S2). A read (`Get`, `List`) takes no lock: a rename is atomic, so it sees the old file or the new one. The lock is held only during one call, so the relay and `relay pair-code` can use one folder at the same time. The rule 3 of the protocol is met with no refusal.
- **List.** `List` reads each `*.json` file of the collection folder, filters on the attributes (a missing attribute is `null`), sorts by key in byte order, and starts after the cursor. The cursor is the last key of the page, in base64url. A relay has tens of documents in a collection, so a read of each file is fast enough, and it always sees the writes of another process.
- **Blobs.** The SDK keeps the chunks of an upload in a temporary file and checks the size and the hash; then it calls `PutBlob`. `PutBlob` copies the bytes to a temporary file in `blobs/<aa>/` and renames it to the hash. An upload never writes to the data folder before its commit, so a wrong hash keeps nothing there (R3.S2), and the SDK's sweep removes an old upload (R3.S3). The spec's assumption names an `uploads/` folder; it is not needed, because the SDK keeps the chunks.
- **Watch.** Each process keeps a list of waiters for each document. A `Put` or `Delete` in the process wakes the waiters of that document at once (R4.S1, R4.S3). For a change from another process, each waiter also checks the version in the file every 250 ms (R4.S2). A `Wait` on a stream gets `refused`, as for any call of the `events` capability.
- **`durable`.** The store implements `plugin.Durable` with `true`.

**The data folder setting.** `data_dir` is not required. With no value, `Open` uses the host kind from `OpenOptions.Host`: `relay` gives `/data`; `app` gives `$SPEC_STUDIO_SUPPORT/backend/json` when the variable is set, else `~/Library/Application Support/Spec Studio/backend/json`; `test` gives a new folder from `os.MkdirTemp`, removed at `Close`. `Open` makes the folder, writes and removes a probe file, and on a failure answers `bad_settings` with the text of the relay's `dataDirText`, with `data_dir` in place of `RELAY_DATA_DIR` (R5.S3).

**The import of the relay's old store** (`internal/legacy`). At `Open`, under the folder lock:

1. When `layout.json` exists, there is no import. When old files are there too (`config.json`, `specs/`, `desks/`, `closed.json`, `pair-code.json`), the plugin logs `legacy_files_ignored` once.
2. When `layout.json` is missing and no old file is there, the folder is new: the plugin writes `layout.json`.
3. Else the plugin imports:
   1. It removes what an earlier import that stopped left: `.import/` and a `state/` folder with no `layout.json`.
   2. It reads and parses every old file first. A file that is not valid JSON, or that is not a version-1 relay file, stops the open with `unavailable`, and the message names the file and says to correct or remove it. No file changes (R6.S6).
   3. It copies the old files to `backup-relay-json-<UTC time>.partial/`, flushes them, and renames the folder to `backup-relay-json-<UTC time>/`.
   4. It writes the documents to `.import/state/` with the plugin's own writer, then renames `.import/state` to `state`.
   5. It writes `layout.json`. From here, a restart does not import again (R6.S4). Then it removes the old files. A crash before this step leaves the old files, and the next start does the import again from them (R6.S5).
   6. It logs one line, `legacy_import`, with the counts of specs, desks, closed specs and configs, and the backup folder name (R6.S3). The line has no value, no hash and no key.

The documents of the import, with the names of `PROTOCOL.md` section 14:

| Old file | Document | Value | Attributes |
|---|---|---|---|
| `config.json` (with `pair-code.json` applied) | `relay` / `config` | the file's JSON object | none |
| `specs/<hash>.json` | `specs` / `<state.spec.repoKey>:<state.spec.slug>` | the file's JSON object | `repoKey`, `deployId`, `open`, `pendingForMac`, `pendingForCoordinator` |
| `desks/<hash>.json` | `desks` / `<state.desk.repoKey>` | the file's JSON object | `repoKey`, `deployId`, `pendingForMac`, `pendingForCoordinator` |
| each item of `closed.json` | `closed-specs` / `<repoKey>:<slug>` | the item | `repoKey`, `closedAt` |

The attributes come from the version-1 files with the relay's rules of today (`src/workflows/types.ts` and `specRules.ts` at relay `3f50e4a`): `open` is `state.state.stage != "merged"`; `pendingForMac` is `state.mac.messages` not empty; `pendingForCoordinator` is a message in `state.coordinator.messages` with `seq > state.coordinator.readUpTo`. A `pair-code.json` whose `id` is not the config's `pairCodeId` is applied as the relay's `JsonConfigStore.load` does: `state.pairCode` gets its `hash` and `expiresAt`, and `pairCodeId` gets its `id`. When two old spec files give the same key (two deploy IDs in one folder), the file with the config's `deployId` wins, else the one with the later `lastActivityAt`; the log line counts the skipped files.

**Logs.** The plugin logs only through `OpenOptions.Log` (stderr, with secrets redacted by the SDK). It logs event names, counts, collection names and folder names; never a value, an attribute, a key of `relay`, a hash or a token. Its own error messages name the collection and the key, never the value (R7).

**Release.** `.github/workflows/release.yml` runs on a tag `v*.*.*`: the tests, then a matrix of four builds with `CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=<X.Y.Z>"`, each in `plugin-json_<X.Y.Z>_<os>_<arch>.tar.gz` with the README, then `checksums.txt` and `gh release create`. A last job renders `packaging/plugin-json.rb.tmpl` with the version and the four SHA-256 sums and pushes `Formula/plugin-json.rb` to spec-studio-app/homebrew-tap, with the `HOMEBREW_TAP_TOKEN` secret. A formula with no release cannot have correct sums, so the tap gets the formula at the first release, not in a PR now; a person adds the secret and pushes the first tag. The pattern follows plugin-sdk's `release.yml`.

**CI.** `.github/workflows/ci.yml` on push and pull request: `go vet`, `go test -race ./...`, a check that the Linux amd64 binary runs in a `scratch` container (R8.S2), and the conformance runner over stdio and over HTTP. The runner is built from plugin-sdk at the commit in `go.mod` (a script reads it with `go list -m`), and the schemas come from the same checkout.

## Repos
| Repo | Why it changes | Merge order |
|---|---|---|
| plugin-sdk | `OpenOptions.Host` for the default data folder; the `watch-stream` conformance test skips a plugin with no `events` | 1 |
| plugin-json | The plugin, its tests, CI, the release workflow, the formula template and the README | 2 |

## Steps
1. plugin-sdk: add `Host protocol.Host` to `plugin.OpenOptions` in `go/plugin/plugin.go`, pass it from `Server.handshake` through `Server.open` in `go/plugin/server.go`, and test it in `go/plugin` with a store that records its `OpenOptions`. Mention the field in the README's store example. Covers R5.
2. plugin-sdk: in `conformance/tests.go`, make `testWatchStream` skip with "the plugin does not declare events" when the plugin has no `events`; add a test in `conformance` with the memory plugin's capabilities cut to `state` and `watch`. Covers R9.
3. plugin-json: the Go module (`go.mod` with the plugin-sdk commit of step 1 and 2, `go 1.24`), `main.go` with the setting `data_dir` and `main.version`, `.tool-versions`, `.gitignore`. Covers R1, R5.
4. plugin-json: `internal/store`: `names.go` (file names), `files.go` (atomic write, temp sweep), `lock_unix.go` (`flock`), `store.go` (`Open` with the data folder default and the probe, `Close`, `Durable`), `state.go` (`Get`, `Put`, `Delete`, `List`), with unit tests. Covers R1, R2, R5, R7.
5. plugin-json: `internal/store/blobs.go` (`HasBlobs`, `PutBlob`, `GetBlob`, `DeleteBlob`) with unit tests. Covers R3.
6. plugin-json: `internal/store/watch.go` (`Wait` with in-process waiters and a 250 ms file check) with unit tests, also with two stores on one folder. Covers R4.
7. plugin-json: `internal/legacy` (detect, read and check, backup, write, mark, clean, log), called from `store.Open` under the folder lock, with unit tests. Covers R6, R7.
8. plugin-json: the relay fixture: `testdata/relay-v1/` made by `scripts/make-relay-fixture.ts`, which runs the relay's own JSON store (from a relay checkout at `3f50e4a`) to pair, register an OAuth client, issue tokens, sync two specs, send messages both ways, report and send to a desk, and close a spec; it writes `testdata/relay-v1.expected.json` with each document name, value and the attributes from the relay's `specMemo` and `deskMemo`. `internal/legacy/fixture_test.go` imports a copy and compares through the plugin's `Get` and `List`. Covers R6.
9. plugin-json: `.github/workflows/ci.yml` and `scripts/conformance.sh` (build the runner from the plugin-sdk commit, run it over stdio with `--setting data_dir=<temp>` and over HTTP with `--listen`), and the `scratch` container check. Covers R8, R9.
10. plugin-json: `.github/workflows/release.yml` and `packaging/plugin-json.rb.tmpl`, with `scripts/render-formula.sh` and a test of the script in CI with fake sums. Covers R8.
11. plugin-json: the README in ASD-STE100: what the plugin does, the settings, the data layout, the import and its backup (and how to go back to an older relay with the backup), the shapes of the imported values for the relay's adapter, conformance, the release, and a link to plugin-sdk. No license file. Covers R10.

## Tests
- R1.S1: `internal/store/state_test.go` `TestPutRestartGet`: put, close the store, open a new store on the folder, get. Also the conformance test `restart` over stdio.
- R1.S2: `state_test.go` `TestGetMissing`; conformance `state-missing`.
- R1.S3: `state_test.go` `TestListFilterPages` with keys `b`, `a`, `c` and page size 1; conformance `state-list-filter`, `state-list-pages`.
- R1.S4: `main_test.go` `TestBadNameRefused` through the SDK server (stdio in memory) with `../config` and `.`, then a walk of the parent of the data folder finds no new file; conformance `bad-name`.
- R1.S5: `main_test.go` `TestDescribe`: capabilities `state`, `blobs`, `watch`, the setting `data_dir`, `durable: true`; conformance `describe`.
- R2.S1: `state_test.go` `TestConcurrentPut`: 20 goroutines put with the same expected version; exactly one succeeds, the others get `conflict` with the new version; conformance `state-race`.
- R2.S2: `state_test.go` `TestTwoProcesses`: the test binary starts itself twice as a helper process (`GO_WANT_HELPER`) on one folder; each puts with the same expected version after a barrier file; exactly one succeeds.
- R2.S3: `files_test.go` `TestCrashBeforeRename`: a test hook stops the write after the temp file; a new store gets the old document, and the `*.tmp` file is gone.
- R2.S4: `state_test.go` `TestVersionNotReused`: put, delete, put 100 times; no version repeats.
- R3.S1: `blobs_test.go` `TestChunkedUpload` through the SDK server with a 3 MiB blob in 1 MiB chunks; conformance `blobs-roundtrip`, `blobs-large`.
- R3.S2: `blobs_test.go` `TestWrongHash`: commit gets `refused`, `blobs.has` is empty, `blobs/` has no file; conformance `blobs-wrong-hash`.
- R3.S3: `blobs_test.go` `TestOldUpload` with `plugin.UploadIdle` set to 50 ms: a later `blobs.write` gets `not_found`, and the data folder has no partial file.
- R4.S1: `watch_test.go` `TestWaitEndsOnPut`: ends within 1 s with the new version; conformance `watch-change`.
- R4.S2: `watch_test.go` `TestWaitSeesOtherStore`: two stores on one folder (two processes share only the files; the second store has no shared memory); ends within 2 s.
- R4.S3: `watch_test.go` `TestWaitEndsOnDelete`: `changed: true`, `version: null`.
- R4.S4: `watch_test.go` `TestWaitTimeout` with 200 ms; conformance `watch-timeout`.
- R5.S1: `store_test.go` `TestDefaultDataDir` for host `relay`: `/data` (the function that picks the folder, with no write).
- R5.S2: `store_test.go` `TestDefaultDataDir` for host `app`, with and with no `SPEC_STUDIO_SUPPORT`.
- R5.S3: `store_test.go` `TestUnwritableDataDir`: a folder with mode 0500; `Open` gives `bad_settings` and the message names `data_dir` (skipped as root).
- R6.S1: `internal/legacy/fixture_test.go` `TestRelayFixture`: every document of `relay-v1.expected.json` reads back with the same value (JSON equal).
- R6.S2: `fixture_test.go` `TestRelayFixtureAttributes`: the attributes equal the expected ones, and `List` on `specs` with `{pendingForMac: true, open: true}` gives the expected keys.
- R6.S3: `legacy_test.go` `TestBackupAndLog`: the backup folder has the old files byte for byte; the log has one `legacy_import` line with the counts.
- R6.S4: `legacy_test.go` `TestNoSecondImport`: a second `Open` logs no `legacy_import` and changes no file.
- R6.S5: `legacy_test.go` `TestCrashDuringImport`: a test hook stops the import after step 4; the next `Open` imports again; each key is there once and `backup-*.partial` folders are gone.
- R6.S6: `legacy_test.go` `TestBadOldFile`: `Open` gives `unavailable` with the file name; a hash of the folder before and after is the same.
- R7.S1: `legacy_test.go` `TestImportLogHasNoSecret`: the log of the fixture import has no agent hash, no client secret, no token hash and no pairing code hash from the fixture.
- R7.S2: `state_test.go` `TestErrorHasNoValue`: a `conflict` and a `refused` on `relay` / `config` with a canary in the value; the error message has no canary.
- R8.S1: `release.yml` on a tag; before the merge, the build matrix of `ci.yml` makes the four archives and `checksums.txt` (with no upload), so a person sees it work on the PR.
- R8.S2: `ci.yml` job `static`: `docker run -i` of a `scratch` image with the Linux amd64 binary, with a `plugin.describe` line on stdin; the answer has `"name":"json"`.
- R8.S3: `ci.yml` step that runs `scripts/render-formula.sh` with fake sums and checks it with `ruby -c`; the push to the tap runs on the first tag.
- R9.S1: `ci.yml` job `conformance` with `scripts/conformance.sh stdio`.
- R9.S2: `ci.yml` job `conformance` with `scripts/conformance.sh http`.
- R10.S1: a review of the README against the sections; `scripts/check-readme.sh` in CI checks the six headings and the plugin-sdk link, and that no `LICENSE` file exists.

## Risks
- **The plugin-sdk PR must merge first.** Its merge makes a new commit on `main`, so the plugin-json PR updates `go.mod` and the runner commit to it before its own merge. The Merge stage does this.
- **The relay's adapter must read the imported shape.** The relay spec `relay-plugin-host` is not built yet. The README documents the shapes (the old file object as the value) so that the adapter reads them; the relay recomputes the attributes at its next write.
- **The fixture needs the relay's code.** `scripts/make-relay-fixture.ts` runs with the relay checkout's `node_modules` (`npx tsx`). The fixture is committed, so CI does not need the relay.
- **`flock` on a network volume.** Some network file systems do not give `flock`. The relay's volume is a local or block volume in its Helm chart and Compose file. The README says that the data folder must be on a local disk.
- **A list reads every file.** This is slow only with many thousands of documents in one collection, more than a relay has. A later change can add an index.
- **The person must add `HOMEBREW_TAP_TOKEN` and push the first tag.** The summary says so.

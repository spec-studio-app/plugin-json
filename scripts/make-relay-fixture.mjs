// Makes testdata/relay-v1/: a data folder of the relay's old JSON store,
// written by the relay's own code, and testdata/relay-v1.expected.json: each
// document that the plugin must give after the import, with the attributes
// from the relay's own memo rules.
//
// Usage (from the plugin-json repo):
//   git -C <relay checkout> checkout 3f50e4a && (cd <relay checkout> && npm ci && npm run build)
//   node scripts/make-relay-fixture.mjs <relay checkout>
//
// The fixture is committed, so CI does not need the relay.
import { mkdirSync, readdirSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { join, resolve } from "node:path";
import { pathToFileURL } from "node:url";

const relay = resolve(process.argv[2] ?? "");
if (!process.argv[2]) {
  console.error("usage: node scripts/make-relay-fixture.mjs <relay checkout, built>");
  process.exit(2);
}
const dist = (p) => import(pathToFileURL(join(relay, "dist", "src", p)).href);
const { openJsonStore, JsonConfigStore } = await dist("store/json/index.js");
const { specAddress, repoKeyOf } = await dist("store/address.js");
const { pairCodeHash } = await dist("pairing.js");
const { specMemo } = await dist("workflows/specRules.js");
const { deskMemo } = await dist("workflows/deskRules.js");
const { mirrorFields } = await dist("workflows/types.js");

const root = resolve("testdata");
const dir = join(root, "relay-v1");
rmSync(dir, { recursive: true, force: true });
mkdirSync(dir, { recursive: true });

const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
const config = { dataDir: dir, deployId: "default", publicUrl: "https://relay.example.com" };
const store = openJsonStore(config, { bucket: null, log: () => {}, graceMs: 50 });
await store.start({ log: () => {}, print: () => {} });

// The pairing, the repos, an OAuth client and its tokens.
await store.config.setPairCode({ hash: pairCodeHash("ABCD-EFGH"), expiresAt: Date.now() + 60_000 });
await store.config.pair(pairCodeHash("ABCD-EFGH"), "a".repeat(64), new Date("2026-11-20T10:00:00Z"));
await store.config.setRepos(["/Users/me/code/app", "/Users/me/code/api"], new Date("2026-11-20T10:01:00Z"));
await store.config.registerClient({ client_id: "client-1", client_name: "Claude", redirect_uris: ["https://claude.ai/api/mcp/auth_callback"] });
await store.config.markSignedIn("client-1");
const token = { clientId: "client-1", scopes: ["mcp"], expiresAt: Date.UTC(2099, 0, 1) };
await store.config.issueTokens({ access: { hash: "b".repeat(64), token }, refresh: { hash: "c".repeat(64), token } });

const show = (slug, over = {}) => ({ slug, title: slug, stage: "build", status: "working", mode: "autopilot", createdAt: "2026-11-01T00:00:00Z", ...over });
const app = "/Users/me/code/app";
const api = "/Users/me/code/api";

// A spec with a message for the Mac and an unread message for Claude AI.
const busy = specAddress(app, "busy-spec");
await store.specs.sync(busy, {
  lines: [
    { line: 1, event: { t: "2026-11-24T10:00:00Z", kind: "status" } },
    { line: 2, event: { t: "2026-11-24T10:01:00Z", kind: "message", id: "u1", text: "Unread for Claude AI" } },
  ],
  first: true,
  state: show("busy-spec"),
  turn: { turn: "yours", needsYou: true },
});
await store.specs.send(busy, { text: "Waits for the Mac" });

// A merged spec with a message for the Mac (so it stays open in the store).
const merged = specAddress(app, "fix..login");
await store.specs.sync(merged, { lines: [{ line: 1, event: { t: "2026-11-24T11:00:00Z", kind: "status" } }], first: true, state: show("fix..login", { stage: "merged", status: "done" }), turn: null });
await store.specs.send(merged, { text: "Still waits" });

// A quiet spec whose messages were read.
const quiet = specAddress(api, "quiet");
await store.specs.sync(quiet, { lines: [{ line: 1, event: { t: "2026-11-24T12:00:00Z", kind: "message", id: "q1", text: "Read" } }], first: true, state: show("quiet", { stage: "plan" }), turn: null });
await store.specs.read(quiet, undefined, 0);

// A spec that the Mac dropped: it closes after the grace period.
const gone = specAddress(api, "dropped-spec");
await store.specs.sync(gone, { lines: [{ line: 1, event: { t: "2026-11-24T13:00:00Z", kind: "status" } }], first: true, state: show("dropped-spec"), turn: null });
await store.specs.gone(gone);
await sleep(500);

// Two desks: one with a message for the Mac, one with an unread reply.
const deskReport = (path, over = {}) => ({ repoKey: repoKeyOf(path), repo: path, open: true, turn: { turn: "idle", needsYou: false }, canMessage: true, reason: null, ...over });
await store.desks.report(deskReport(app));
await store.desks.send({ repoKey: repoKeyOf(app) }, { text: "Is CI green on main?" });
await store.desks.report(deskReport(api));
await store.desks.sync({ repoKey: repoKeyOf(api), repo: api }, [{ line: 1, event: { t: "2026-11-24T14:00:00Z", kind: "message", id: "r1", text: "CI is green." } }]);

// A new pairing code from `relay pair-code` that the relay did not take yet.
await store.config.setPairCode({ hash: pairCodeHash("WXYZ-2345"), expiresAt: Date.UTC(2099, 0, 1) });
await store.close();

// The expected documents, read from the files that the relay wrote.
const read = (p) => JSON.parse(readFileSync(p, "utf8"));
const docs = [];
// The relay takes the waiting pair-code.json at its next call; the import does the same.
const cfg = read(join(dir, "config.json"));
const code = read(join(dir, "pair-code.json"));
const taken = await new JsonConfigStore({ dir, deployId: "default" }).read();
docs.push({ collection: "relay", key: "config", value: { ...cfg, state: taken, pairCodeId: code.id }, attributes: {} });
for (const name of readdirSync(join(dir, "specs")).filter((n) => n.endsWith(".json")).sort()) {
  const f = read(join(dir, "specs", name));
  const s = f.state;
  const memo = specMemo(s);
  docs.push({
    collection: "specs",
    key: `${s.spec.repoKey}:${s.spec.slug}`,
    value: f,
    attributes: {
      repoKey: s.spec.repoKey,
      deployId: s.deployId,
      open: mirrorFields(s.state).stage !== "merged",
      pendingForMac: memo.waitingForMac > 0,
      pendingForCoordinator: memo.unreadForCoordinator > 0,
    },
  });
}
for (const name of readdirSync(join(dir, "desks")).filter((n) => n.endsWith(".json")).sort()) {
  const f = read(join(dir, "desks", name));
  const memo = deskMemo(f.state);
  docs.push({
    collection: "desks",
    key: f.state.desk.repoKey,
    value: f,
    attributes: { repoKey: f.state.desk.repoKey, deployId: f.state.deployId, pendingForMac: memo.waitingForMac > 0, pendingForCoordinator: memo.unreadForCoordinator > 0 },
  });
}
for (const c of read(join(dir, "closed.json")).closed) {
  docs.push({ collection: "closed-specs", key: `${c.repoKey}:${c.slug}`, value: c, attributes: { repoKey: c.repoKey, closedAt: c.closedAt } });
}
writeFileSync(join(root, "relay-v1.expected.json"), JSON.stringify({ relayCommit: "3f50e4a", documents: docs }, null, 2) + "\n");
console.log(`wrote ${docs.length} documents`);

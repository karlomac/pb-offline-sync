/**
 * Provisions the e2e schema. Run against the server in PLAIN mode (no SYNC_*
 * env) before starting it with the plugins, because the sync plugin validates
 * the collections at boot.
 *
 * Creates: the `media` collection (protected file fields, owner-scoped rules,
 * locked writes), the `notes` collection (locked REST rules; the sync routes
 * are the only access path), and test users. Also makes `users` signup-enabled
 * with `verified = true` so the auth scenarios can run.
 */
const BASE = process.env.E2E_BASE_URL ?? "http://127.0.0.1:8090";
const ADMIN = {
  identity: process.env.E2E_ADMIN_EMAIL ?? "e2e-admin@example.com",
  password: process.env.E2E_ADMIN_PASSWORD ?? "e2e-admin-password",
};
const MAX_FILE = Number(process.env.E2E_MAX_FILE_BYTES ?? 52428800);
const LOAD_USERS = Number(process.env.E2E_LOAD_USERS ?? 0);
const PASSWORD = "e2e-password-123";

async function api(method, path, body, token) {
  const res = await fetch(BASE + path, {
    method,
    headers: {
      "Content-Type": "application/json",
      ...(token ? { Authorization: token } : {}),
    },
    body: body === undefined ? undefined : JSON.stringify(body),
  });
  const text = await res.text();
  if (!res.ok) throw new Error(`${method} ${path} -> ${res.status}: ${text}`);
  return text ? JSON.parse(text) : null;
}

const admin = await api("POST", "/api/collections/_superusers/auth-with-password", ADMIN);
const su = admin.token;
const users = await api("GET", "/api/collections/users", undefined, su);

const autodate = [
  { name: "created", type: "autodate", onCreate: true },
  { name: "updated", type: "autodate", onCreate: true, onUpdate: true },
];

async function ensure(payload) {
  try {
    return await api("GET", `/api/collections/${payload.name}`, undefined, su);
  } catch {
    return await api("POST", "/api/collections", payload, su);
  }
}

const media = await ensure({
  name: "media",
  type: "base",
  fields: [
    ...autodate,
    { name: "sha256", type: "text" },
    { name: "size", type: "number", onlyInt: true },
    { name: "mime", type: "text" },
    { name: "filename", type: "text" },
    { name: "file", type: "file", maxSelect: 1, maxSize: MAX_FILE, protected: true },
    { name: "poster", type: "file", maxSelect: 1, maxSize: 8388608, protected: true },
    { name: "owner", type: "relation", collectionId: users.id, maxSelect: 1, cascadeDelete: false },
  ],
  // PocketBase serves a protected file only when the record passes this rule.
  listRule: "owner = @request.auth.id",
  viewRule: "owner = @request.auth.id",
  createRule: null,
  updateRule: null,
  deleteRule: null,
});

await ensure({
  name: "notes",
  type: "base",
  fields: [
    ...autodate,
    { name: "title", type: "text" },
    { name: "body", type: "text" },
    { name: "attachment", type: "relation", collectionId: media.id, maxSelect: 1, cascadeDelete: false },
    { name: "owner", type: "relation", collectionId: users.id, maxSelect: 1, cascadeDelete: false },
  ],
  // Locked: records are only reachable through the plugin's owner-filtered routes.
  listRule: null,
  viewRule: null,
  createRule: null,
  updateRule: null,
  deleteRule: null,
});

// Signup-enabled + verification-required, as the auth plugin expects at boot.
const usersFull = await api("GET", `/api/collections/${users.id}`, undefined, su);
usersFull.createRule = "";
usersFull.authRule = "verified = true";
await api("PATCH", `/api/collections/${users.id}`, usersFull, su);

async function user(email, verified = true) {
  const filter = encodeURIComponent(`email="${email}"`);
  const found = await api("GET", `/api/collections/users/records?filter=${filter}`, undefined, su);
  if (found.items.length > 0) return found.items[0];
  return api("POST", "/api/collections/users/records", {
    email,
    password: PASSWORD,
    passwordConfirm: PASSWORD,
    verified,
  }, su);
}

await user("alice@e2e.test");
await user("bob@e2e.test");
for (let i = 0; i < LOAD_USERS; i++) await user(`load${i}@e2e.test`);

console.log(JSON.stringify({ ok: true, usersId: users.id, mediaId: media.id, loadUsers: LOAD_USERS }));

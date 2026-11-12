/**
 * k6 load for the sync endpoints. Requires provisioned load users
 * (E2E_LOAD_USERS) and rate limiting disabled (see the test plan).
 *
 *   E2E_BASE_URL=http://127.0.0.1:8090 E2E_LOAD_USERS=50 \
 *   E2E_DURATION=2m E2E_VUS=25 k6 run e2e/load.k6.js
 */
import http from "k6/http";
import { check } from "k6";

const BASE = __ENV.E2E_BASE_URL ?? "http://127.0.0.1:8090";
const USERS = Number(__ENV.E2E_LOAD_USERS ?? 10);
const VUS = Number(__ENV.E2E_VUS ?? 25);
const DURATION = __ENV.E2E_DURATION ?? "2m";
const PASSWORD = "e2e-password-123";

export const options = {
  scenarios: {
    push: { executor: "constant-vus", vus: VUS, duration: DURATION, exec: "pushLoop" },
    pull: { executor: "constant-vus", vus: VUS, duration: DURATION, exec: "pullLoop" },
  },
  thresholds: {
    http_req_failed: ["rate<0.001"],
    "http_req_duration{scenario:push}": ["p(95)<500"],
    "http_req_duration{scenario:pull}": ["p(95)<800"],
  },
};

export function setup() {
  const tokens = [];
  for (let i = 0; i < USERS; i++) {
    const res = http.post(
      `${BASE}/api/collections/users/auth-with-password`,
      JSON.stringify({ identity: `load${i}@e2e.test`, password: PASSWORD }),
      { headers: { "Content-Type": "application/json" } },
    );
    if (res.status !== 200) throw new Error(`login load${i}: ${res.status} ${res.body}`);
    tokens.push(res.json("token"));
  }
  return { tokens };
}

const headers = (token) => ({
  "Content-Type": "application/json",
  "X-Sync-Protocol": "1",
  Authorization: token,
});

const rid = (n) => "l" + String(n).padStart(14, "0");

export function pushLoop(data) {
  const token = data.tokens[(__VU - 1) % data.tokens.length];
  const mutations = [];
  for (let i = 0; i < 10; i++) {
    mutations.push({
      id: `k6-${__VU}-${__ITER}-${i}`,
      collection: "notes",
      recordId: rid(__VU * 1000000 + (__ITER % 1000) * 10 + i),
      op: "create",
      fields: { title: `vu${__VU}-iter${__ITER}-${i}` },
    });
  }
  const res = http.post(`${BASE}/api/sync/push`, JSON.stringify({ mutations }), {
    headers: headers(token),
  });
  check(res, {
    "push 200": (r) => r.status === 200,
    "push results": (r) =>
      (r.json("results") ?? []).every((x) => x.status === "ok" || x.status === "duplicate"),
  });
}

const cursors = {}; // per-VU: first pull snapshots, subsequent ones are incremental

export function pullLoop(data) {
  const token = data.tokens[(__VU - 1) % data.tokens.length];
  const cursor = cursors[__VU];
  const url = `${BASE}/api/sync/pull?limit=200` + (cursor ? `&cursor=${encodeURIComponent(cursor)}` : "");
  const res = http.get(url, { headers: headers(token) });
  if (res.status === 200) cursors[__VU] = res.json("cursor");
  check(res, {
    "pull 200": (r) => r.status === 200,
    "pull shape": (r) => Array.isArray(r.json("changes")) && typeof r.json("hasMore") === "boolean",
  });
}

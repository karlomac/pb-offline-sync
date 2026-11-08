/**
 * Opt-in live auth-gate suite (test plan A1–A6). Requires RC-C:
 * AUTH_SIGNUP_MODE=password, AUTH_ALLOWED_EMAIL_DOMAINS=example.com,
 * AUTH_DISPOSABLE_DOMAINS=mailinator.com, and the loopback Turnstile stub.
 */
import { afterEach, describe, expect, it } from "vitest";

const BASE = process.env.E2E_BASE_URL;
const STUB = "http://127.0.0.1:8787";
const live = BASE ? describe : describe.skip;
const PASSWORD = "signup-password-123";
let n = 0;

function email(prefix: string, domain = "example.com"): string {
  return `${prefix}-${Date.now()}-${n++}@${domain}`;
}

async function signup(address: string, token?: string) {
  return fetch(`${BASE}/api/collections/users/records`, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({
      email: address,
      password: PASSWORD,
      passwordConfirm: PASSWORD,
      ...(token ? { turnstileToken: token } : {}),
    }),
  });
}

async function login(address: string) {
  return fetch(`${BASE}/api/collections/users/auth-with-password`, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ identity: address, password: PASSWORD }),
  });
}

async function adminToken(): Promise<string> {
  const res = await fetch(`${BASE}/api/collections/_superusers/auth-with-password`, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({
      identity: process.env.E2E_ADMIN_EMAIL ?? "e2e-admin@example.com",
      password: process.env.E2E_ADMIN_PASSWORD ?? "e2e-admin-password",
    }),
  });
  if (res.status !== 200) throw new Error(`admin login ${res.status}`);
  return ((await res.json()) as { token: string }).token;
}

async function setStubMode(mode: "ok" | "fail" | "hostname") {
  await fetch(`${STUB}/mode?value=${mode}`);
}

live("auth signup gate", () => {
  afterEach(async () => setStubMode("ok"));

  it("A1 signup without a Turnstile token is rejected", async () => {
    const res = await signup(email("no-token"));
    expect(res.status).toBe(400);
  });

  it("A2 a gated signup succeeds and A3 unverified cannot sign in", async () => {
    const address = email("gated");
    const created = await signup(address, "ok");
    expect(created.status).toBe(200);
    const record = (await created.json()) as { id: string; verified: boolean };
    expect(record.verified).toBe(false);

    const blocked = await login(address);
    expect([400, 403]).toContain(blocked.status);

    // A4: an admin verifies the account and login then works.
    const su = await adminToken();
    const patch = await fetch(`${BASE}/api/collections/users/records/${record.id}`, {
      method: "PATCH",
      headers: { "Content-Type": "application/json", Authorization: su },
      body: JSON.stringify({ verified: true }),
    });
    expect(patch.status).toBe(200);
    const ok = await login(address);
    expect(ok.status).toBe(200);
    expect(((await ok.json()) as { token: string }).token).toBeTruthy();
  });

  it("A5 allowed, blocked and disposable-subdomain addresses", async () => {
    expect((await signup(email("allowed", "example.com"), "ok")).status).toBe(200);
    expect((await signup(email("blocked", "mailinator.com"), "ok")).status).toBe(400);
    expect((await signup(email("sub", "rotating.mailinator.com"), "ok")).status).toBe(400);
  });

  it("A6 a token solved for the wrong hostname is rejected", async () => {
    await setStubMode("hostname");
    expect((await signup(email("hostname"), "ok")).status).toBe(400);
    await setStubMode("ok");
    expect((await signup(email("hostname-ok"), "ok")).status).toBe(200);
  });
});

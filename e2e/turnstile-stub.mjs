/**
 * Loopback Turnstile verifier for the auth scenarios.
 *   GET  /mode?value=ok|fail|hostname
 *   POST /siteverify -> {success, hostname, error-codes}
 */
import { createServer } from "node:http";

let mode = "ok";
const hostname = "localhost";

createServer((req, res) => {
  if (req.url?.startsWith("/mode?value=")) {
    mode = new URL(req.url, "http://stub").searchParams.get("value") ?? "ok";
    res.end(`mode=${mode}\n`);
    return;
  }
  req.resume();
  req.on("end", () => {
    const ok = mode !== "fail";
    res.setHeader("Content-Type", "application/json");
    res.end(JSON.stringify({
      success: ok,
      hostname: mode === "hostname" ? "evil.example" : hostname,
      "error-codes": ok ? [] : ["invalid-input-response"],
    }));
  });
}).listen(8787, "127.0.0.1", () => console.log("turnstile stub on 127.0.0.1:8787"));

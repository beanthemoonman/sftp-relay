import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { api, BASE, compose, post, put, servers, waitFor, DEST_ROOT, SOURCE_DIR } from "./helpers";

const KEYS = fileURLToPath(new URL("./keys/", import.meta.url));

/**
 * Configures the app through its own API, never by touching SQLite. lftp_path is
 * left empty on purpose: the startup probe that fills it in is under test.
 */
export default async function globalSetup() {
  await waitFor(
    "the relay to answer /api/health",
    async () => (await fetch(BASE + "/api/health")).status,
    (s) => s === 200,
    120_000,
  );

  await put("/api/settings", {
    nas_host: "fake-nas",
    nas_user: "nas",
    nas_port: "22",
    nas_tmp: `${DEST_ROOT}/.tmp`,
    allowed_dest_roots: DEST_ROOT,
    concurrency: "2",
    segments: "4",
    parallel: "2",
    lftp_path: "",
  });

  const privateKey = readFileSync(KEYS + "remote_key", "utf8");
  const have = new Set((await servers()).map((s) => s.name));

  const wanted = [
    {
      name: "source-key",
      username: "keyuser",
      auth_type: "key",
      private_key: privateKey,
      password: "",
    },
    {
      name: "source-pass",
      username: "passuser",
      auth_type: "password",
      password: "passpw",
      private_key: "",
    },
  ];

  for (const w of wanted) {
    if (have.has(w.name)) continue;
    const created = await post<{ id: number }>("/api/servers", {
      ...w,
      host: "sftp-source",
      port: 22,
      passphrase: "",
      host_key: "",
      default_remote_path: SOURCE_DIR,
    });
    // Testing the connection is what pins the remote host key (trust on first
    // connect). Without it a transfer has no key to hand ssh on the NAS.
    const res = await post<{ ok: boolean; description?: string }>(
      `/api/servers/${created.id}/test`,
    );
    if (!res.ok) throw new Error(`${w.name}: connection test failed: ${res.description}`);
  }

  // The NAS was unconfigured when the container booted, so the startup tool
  // probe skipped itself. Restart once now that it is configured: that runs the
  // real boot path — pinning nas_host_key, sweeping stale workspaces and
  // resolving lftp off the non-interactive PATH — and fails setup loudly if the
  // NAS is wrong, rather than failing inside the first transfer spec.
  compose("restart", "relay");
  await waitFor(
    "the relay to answer /api/health after the restart",
    async () => (await fetch(BASE + "/api/health")).status,
    (s) => s === 200,
    120_000,
  );
  await waitFor(
    "the lftp probe to resolve a path on the NAS",
    () => api<Record<string, string>>("/api/settings"),
    (s) => s.lftp_path !== "" && s.nas_host_key !== "",
    60_000,
  );
}

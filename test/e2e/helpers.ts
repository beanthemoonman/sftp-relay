import { execFileSync } from "node:child_process";
import { fileURLToPath } from "node:url";

// E2E_PORT lets the suite dodge a dev stack already sitting on 8088.
export const PORT = process.env.E2E_PORT ?? "8088";
export const BASE = `http://localhost:${PORT}`;
export const AUTH = "Basic " + Buffer.from("e2e:e2e-pass").toString("base64");
export const DEST_ROOT = "/volume1/media";
// SOURCE_DIR is what the relay sees: atmoz chroots the sftp session to the
// user's home. SOURCE_HOME is the same tree as a shell inside the container
// sees it, which is what `docker compose exec` needs.
export const SOURCE_DIR = "/upload";
export const SOURCE_HOME = "/home/keyuser/upload";

const CWD = fileURLToPath(new URL(".", import.meta.url));

export type Job = {
  id: number;
  status: string;
  percent: number;
  total_bytes: number;
  transferred_bytes: number;
  error: string;
  remote_path: string;
  dest_path: string;
};

/** api talks to the relay exactly as the browser does: through nginx, with basic auth. */
export async function api<T>(path: string, init: RequestInit = {}): Promise<T> {
  const res = await fetch(BASE + path, {
    ...init,
    headers: {
      Authorization: AUTH,
      ...(init.body ? { "Content-Type": "application/json" } : {}),
      ...(init.headers ?? {}),
    },
  });
  if (!res.ok) {
    throw new Error(`${init.method ?? "GET"} ${path} → ${res.status}: ${await res.text()}`);
  }
  return res.status === 204 ? (null as T) : ((await res.json()) as T);
}

export const post = <T,>(path: string, body?: unknown) =>
  api<T>(path, { method: "POST", body: body === undefined ? undefined : JSON.stringify(body) });
export const put = <T,>(path: string, body: unknown) =>
  api<T>(path, { method: "PUT", body: JSON.stringify(body) });

/** compose runs a docker compose subcommand against the e2e stack. */
export function compose(...args: string[]): string {
  return execFileSync("docker", ["compose", ...args], {
    cwd: CWD,
    encoding: "utf8",
    maxBuffer: 64 * 1024 * 1024,
  });
}

/** exec runs a POSIX sh command inside one of the stack's containers. */
export const exec = (service: string, cmd: string) =>
  compose("exec", "-T", service, "sh", "-c", cmd).trim();

export const nas = (cmd: string) => exec("fake-nas", cmd);
export const source = (cmd: string) => exec("sftp-source", cmd);

/** sha256 of a path inside fake-nas, or "" when it is not there. */
export const nasSum = (p: string) =>
  nas(`sha256sum ${sh(p)} 2>/dev/null | cut -d' ' -f1`);

export const sh = (s: string) => "'" + s.replaceAll("'", `'\''`) + "'";

/** fixtureSums maps "./small.txt" style keys from the seeded SHA256SUMS file. */
export function fixtureSums(): Map<string, string> {
  const out = new Map<string, string>();
  for (const line of source(`cat ${SOURCE_HOME}/SHA256SUMS`).split("\n")) {
    const m = /^(\S+)\s+(.+)$/.exec(line.trim());
    if (m) out.set(m[2].replace(/^\.\//, ""), m[1]);
  }
  return out;
}

export async function servers() {
  return api<{ id: number; name: string }[]>("/api/servers");
}

export async function serverID(name: string): Promise<number> {
  const s = (await servers()).find((x) => x.name === name);
  if (!s) throw new Error(`no server named ${name}`);
  return s.id;
}

/**
 * throttle caps the *next* lftp run's total rate (bytes/sec, lftp suffixes
 * allowed). Two containers on a loopback move 200 MB in under a second, which
 * leaves the progress, cancel and restart-resume scenarios nothing in flight to
 * observe. See test/e2e/fake-nas/lftp-wrapper.sh.
 */
export const throttle = (rate: string) => nas(`echo ${sh(rate)} > /tmp/e2e-throttle`);
export const unthrottle = () => nas("rm -f /tmp/e2e-throttle");

export type QueueItem = { path: string; is_dir: boolean };

export async function queue(server: number, dest: string, items: QueueItem[]): Promise<Job[]> {
  const r = await post<{ jobs: Job[] }>("/api/jobs", {
    server_id: server,
    dest_path: dest,
    items,
  });
  return r.jobs;
}

export const getJob = (id: number) => api<Job>(`/api/jobs/${id}`);

/**
 * waitFor polls until predicate holds. Polling, not sleeping: the DoD forbids
 * wall-clock synchronisation, and a poll fails loudly with the last value seen.
 */
export async function waitFor<T>(
  what: string,
  read: () => Promise<T> | T,
  ok: (v: T) => boolean,
  timeoutMs = 120_000,
  everyMs = 300,
): Promise<T> {
  const deadline = Date.now() + timeoutMs;
  let last: T | undefined;
  let lastErr: unknown;
  for (;;) {
    try {
      last = await read();
      lastErr = undefined;
      if (ok(last)) return last;
    } catch (e) {
      lastErr = e;
    }
    if (Date.now() > deadline) {
      throw new Error(
        `timed out waiting for ${what}; last=${JSON.stringify(last)}` +
          (lastErr ? ` lastError=${String(lastErr)}` : ""),
      );
    }
    await new Promise((r) => setTimeout(r, everyMs));
  }
}

export const TERMINAL = ["done", "failed", "cancelled"];

export const waitForJob = (id: number, ok: (j: Job) => boolean, timeoutMs?: number) =>
  waitFor(`job ${id}`, () => getJob(id), ok, timeoutMs);

export const waitForDone = (id: number, timeoutMs?: number) =>
  waitForJob(
    id,
    (j) => TERMINAL.includes(j.status),
    timeoutMs,
  ).then((j) => {
    if (j.status !== "done") throw new Error(`job ${id} ended ${j.status}: ${j.error}`);
    return j;
  });

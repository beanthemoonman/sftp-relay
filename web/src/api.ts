// Thin fetch wrapper plus the wire types. Basic auth is handled by the browser,
// so nothing here touches credentials.

export type Server = {
  id: number;
  name: string;
  host: string;
  port: number;
  username: string;
  auth_type: "password" | "key";
  host_key: string;
  default_remote_path: string;
  has_password: boolean;
  has_private_key: boolean;
};

export type ServerInput = {
  name: string;
  host: string;
  port: number;
  username: string;
  auth_type: "password" | "key";
  password?: string;
  private_key?: string;
  passphrase?: string;
  host_key: string;
  default_remote_path: string;
};

export type RemoteEntry = {
  name: string;
  size: number;
  mode: string;
  mod_time: string;
  is_dir: boolean;
};

export type RemoteListing = { path: string; parent: string; entries: RemoteEntry[] };

export type NasEntry = { name: string; path: string; is_dir: boolean };

export type NasListing = {
  path: string;
  parent: string;
  entries: NasEntry[];
  free_bytes: number;
  total_bytes: number;
};

export type JobStatus =
  | "queued"
  | "running"
  | "paused"
  | "done"
  | "failed"
  | "cancelled"
  | "interrupted";

export type Job = {
  id: number;
  server_id: number;
  server_name: string;
  kind: "file" | "dir";
  remote_path: string;
  dest_path: string;
  status: JobStatus;
  total_bytes: number;
  transferred_bytes: number;
  speed_bps: number;
  eta_seconds: number;
  percent: number;
  exit_code: number | null;
  error: string;
  created_at: string;
  started_at: string;
  finished_at: string;
};

export type TestResult = {
  ok: boolean;
  entries?: number;
  elapsed_ms?: number;
  host_key?: string;
  host_key_new?: boolean;
  description?: string;
};

export type Settings = Record<string, string>;

export const ACTIVE: JobStatus[] = ["queued", "running", "paused", "interrupted"];

export const isActive = (j: Job) => ACTIVE.includes(j.status);

async function req<T>(url: string, init?: RequestInit): Promise<T> {
  const res = await fetch(url, {
    ...init,
    headers: init?.body ? { "Content-Type": "application/json" } : undefined,
  });
  if (!res.ok) {
    const body = await res.json().catch(() => null);
    throw new Error(body?.error || `${res.status} ${res.statusText}`);
  }
  return res.status === 204 ? (null as T) : ((await res.json()) as T);
}

const send = (method: string) => <T,>(url: string, body?: unknown) =>
  req<T>(url, { method, body: body === undefined ? undefined : JSON.stringify(body) });

export const api = {
  get: <T,>(url: string) => req<T>(url),
  post: send("POST"),
  put: send("PUT"),
  del: send("DELETE"),

  servers: () => req<Server[]>("/api/servers"),
  browse: (id: number, path: string) =>
    req<RemoteListing>(`/api/servers/${id}/browse?path=${encodeURIComponent(path)}`),
  nasBrowse: (path: string) =>
    req<NasListing>(`/api/nas/browse?path=${encodeURIComponent(path)}`),
  // limit=200 must stay in step with the server's SSE snapshot cap, so a
  // reconnect never shrinks the list a client has already loaded.
  jobs: (status = "") =>
    req<{ jobs: Job[] }>(`/api/jobs?limit=200&status=${status}`).then((r) => r.jobs ?? []),
  jobLog: (id: number) =>
    req<{ lines: string[] }>(`/api/jobs/${id}/log`).then((r) => r.lines ?? []),
  settings: () => req<Settings>("/api/settings"),
};

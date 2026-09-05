// Formatting and aggregation helpers. Kept pure and dependency-free so they are
// cheap to test — the selection bar's totals are computed here, not in the view.

export function bytes(n: number): string {
  if (!isFinite(n) || n <= 0) return "0 B";
  const units = ["B", "KB", "MB", "GB", "TB", "PB"];
  const i = Math.min(units.length - 1, Math.floor(Math.log(n) / Math.log(1024)));
  const v = n / Math.pow(1024, i);
  return `${i === 0 ? v : v.toFixed(v < 10 ? 1 : 0)} ${units[i]}`;
}

export const speed = (bps: number): string => (bps > 0 ? `${bytes(bps)}/s` : "—");

export function eta(seconds: number): string {
  if (!isFinite(seconds) || seconds <= 0) return "—";
  const s = Math.round(seconds);
  const h = Math.floor(s / 3600);
  const m = Math.floor((s % 3600) / 60);
  if (h > 0) return `${h}h ${m}m`;
  if (m > 0) return `${m}m ${s % 60}s`;
  return `${s}s`;
}

/** join builds a child path without ever producing a double slash. */
export const join = (dir: string, name: string): string =>
  dir === "/" ? `/${name}` : `${dir.replace(/\/+$/, "")}/${name}`;

export const basename = (p: string): string => p.split("/").filter(Boolean).pop() ?? "/";

/** crumbs turns /a/b/c into the clickable breadcrumb trail, root first. */
export function crumbs(path: string): { name: string; path: string }[] {
  const parts = path.split("/").filter(Boolean);
  const out = [{ name: "/", path: "/" }];
  let acc = "";
  for (const part of parts) {
    acc += `/${part}`;
    out.push({ name: part, path: acc });
  }
  return out;
}

export const when = (ts: string): string =>
  ts ? new Date(ts.includes("T") ? ts : ts.replace(" ", "T") + "Z").toLocaleString() : "—";

export type Selectable = { size: number; is_dir: boolean };

/**
 * summarise aggregates the selection bar. Directory sizes are unknown until the
 * server walks them, so they are counted separately rather than folded into a
 * byte total that would be a lie.
 */
export function summarise(items: Iterable<Selectable>) {
  let files = 0;
  let dirs = 0;
  let size = 0;
  for (const e of items) {
    if (e.is_dir) dirs++;
    else {
      files++;
      size += e.size;
    }
  }
  return { count: files + dirs, files, dirs, bytes: size };
}

import { useEffect, useMemo, useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { api, type NasEntry, type RemoteEntry, type RemoteListing } from "../api";
import { bytes, crumbs, join, summarise, when } from "../format";
import { Card, Empty, Err, Field, ghost, input, primary } from "../ui";

type Picked = { path: string; size: number; is_dir: boolean };

export default function Browse() {
  const qc = useQueryClient();
  const servers = useQuery({ queryKey: ["servers"], queryFn: api.servers });

  const [serverID, setServerID] = useState(0);
  const [path, setPath] = useState("");
  const [picked, setPicked] = useState<Map<string, Picked>>(new Map());
  const [dest, setDest] = useState("");
  const [pane, setPane] = useState<"source" | "dest">("source");

  // Default to the first server, and reset the browse position when it changes.
  useEffect(() => {
    const list = servers.data;
    if (!list?.length) return;
    if (!list.some((s) => s.id === serverID)) {
      setServerID(list[0].id);
      setPath(list[0].default_remote_path || "/");
      setPicked(new Map());
    }
  }, [servers.data, serverID]);

  const listing = useQuery({
    queryKey: ["browse", serverID, path],
    queryFn: () => api.browse(serverID, path),
    enabled: serverID > 0,
  });

  const totals = useMemo(() => summarise(picked.values()), [picked]);

  const queue = useMutation({
    mutationFn: () =>
      api.post("/api/jobs", {
        server_id: serverID,
        dest_path: dest,
        items: [...picked.values()].map((p) => ({ path: p.path, is_dir: p.is_dir })),
      }),
    onSuccess: () => {
      setPicked(new Map());
      void qc.invalidateQueries({ queryKey: ["jobs"] });
      window.location.hash = "#queue";
    },
  });

  const toggle = (e: RemoteEntry) => {
    const full = join(listing.data?.path ?? path, e.name);
    setPicked((prev) => {
      const next = new Map(prev);
      if (next.has(full)) next.delete(full);
      else next.set(full, { path: full, size: e.size, is_dir: e.is_dir });
      return next;
    });
  };

  if (servers.isLoading) return <Empty>Loading…</Empty>;
  if (!servers.data?.length)
    return (
      <Empty>
        No servers yet. Add one on the{" "}
        <a className="text-sky-400 underline" href="#servers">
          Servers
        </a>{" "}
        page.
      </Empty>
    );

  return (
    <div className="space-y-3">
      <select
        className={input}
        value={serverID}
        onChange={(e) => {
          const id = Number(e.target.value);
          const s = servers.data.find((x) => x.id === id);
          setServerID(id);
          setPath(s?.default_remote_path || "/");
          setPicked(new Map());
        }}
      >
        {servers.data.map((s) => (
          <option key={s.id} value={s.id}>
            {s.name} — {s.username}@{s.host}
          </option>
        ))}
      </select>

      {/* Mobile shows one pane at a time; desktop shows both. */}
      <div className="grid grid-cols-2 gap-1 sm:hidden">
        <button className={pane === "source" ? primary : ghost} onClick={() => setPane("source")}>
          Source
        </button>
        <button className={pane === "dest" ? primary : ghost} onClick={() => setPane("dest")}>
          Destination
        </button>
      </div>

      <div className="grid gap-3 sm:grid-cols-2">
        <div className={pane === "source" ? "" : "hidden sm:block"}>
          <SourcePane
            data={listing.data}
            error={listing.error}
            loading={listing.isFetching}
            path={path}
            setPath={setPath}
            picked={picked}
            toggle={toggle}
          />
        </div>
        <div className={pane === "dest" ? "" : "hidden sm:block"}>
          <DestPane dest={dest} setDest={setDest} />
        </div>
      </div>

      {totals.count > 0 && (
        <div className="fixed inset-x-0 bottom-14 z-20 border-t border-slate-800 bg-slate-900/95 p-3 backdrop-blur sm:bottom-0">
          <div className="mx-auto flex max-w-5xl items-center gap-3">
            <div className="min-w-0 text-sm">
              <div className="font-medium">
                {totals.count} selected · {bytes(totals.bytes)}
                {totals.dirs > 0 && ` + ${totals.dirs} folder${totals.dirs > 1 ? "s" : ""}`}
              </div>
              <div className="truncate text-xs text-slate-400">
                → {dest || "pick a destination"}
              </div>
            </div>
            <button className={`${ghost} ml-auto`} onClick={() => setPicked(new Map())}>
              Clear
            </button>
            <button
              className={primary}
              disabled={!dest || queue.isPending}
              onClick={() => queue.mutate()}
            >
              {queue.isPending ? "Queueing…" : "Download"}
            </button>
          </div>
          {queue.error ? (
            <div className="mx-auto mt-2 max-w-5xl">
              <Err error={queue.error} />
            </div>
          ) : null}
        </div>
      )}
    </div>
  );
}

function Crumbs({ path, onGo }: { path: string; onGo: (p: string) => void }) {
  return (
    <div className="flex flex-wrap items-center gap-1 text-xs text-slate-400">
      {crumbs(path).map((c, i) => (
        <span key={c.path} className="flex items-center gap-1">
          {i > 0 && <span className="text-slate-600">/</span>}
          <button className="hover:text-sky-400" onClick={() => onGo(c.path)}>
            {c.name}
          </button>
        </span>
      ))}
    </div>
  );
}

function SourcePane({
  data,
  error,
  loading,
  path,
  setPath,
  picked,
  toggle,
}: {
  data?: RemoteListing;
  error: unknown;
  loading: boolean;
  path: string;
  setPath: (p: string) => void;
  picked: Map<string, Picked>;
  toggle: (e: RemoteEntry) => void;
}) {
  const here = data?.path ?? path;
  return (
    <Card className="space-y-2">
      <Crumbs path={here} onGo={setPath} />
      <Err error={error} />
      {loading && <p className="text-xs text-slate-500">Loading…</p>}
      <ul className="divide-y divide-slate-800">
        {data?.entries.length === 0 && <Empty>Empty folder</Empty>}
        {data?.entries.map((e) => {
          const full = join(here, e.name);
          return (
            <li key={e.name} className="flex items-center gap-2 py-2">
              <input
                type="checkbox"
                aria-label={e.name}
                className="h-5 w-5 shrink-0 accent-sky-500"
                checked={picked.has(full)}
                onChange={() => toggle(e)}
              />
              <button
                className="min-w-0 flex-1 text-left"
                onClick={() => (e.is_dir ? setPath(full) : toggle(e))}
              >
                <div className="truncate text-sm">
                  {e.is_dir ? "📁 " : "📄 "}
                  {e.name}
                </div>
                <div className="text-xs text-slate-500">
                  {e.is_dir ? "folder" : bytes(e.size)} · {when(e.mod_time)}
                </div>
              </button>
            </li>
          );
        })}
      </ul>
    </Card>
  );
}

function DestPane({ dest, setDest }: { dest: string; setDest: (p: string) => void }) {
  const qc = useQueryClient();
  const [at, setAt] = useState("");
  const [folder, setFolder] = useState("");
  const listing = useQuery({ queryKey: ["nas", at], queryFn: () => api.nasBrowse(at) });

  const mkdir = useMutation({
    mutationFn: (name: string) => api.post("/api/nas/mkdir", { path: join(at || "/", name) }),
    onSuccess: () => {
      setFolder("");
      void qc.invalidateQueries({ queryKey: ["nas"] });
    },
  });

  const go = (e: NasEntry) => {
    setAt(e.path);
    setDest(e.path);
  };

  return (
    <Card className="space-y-2">
      {at ? (
        <Crumbs
          path={at}
          onGo={(p) => {
            setAt(p === "/" ? "" : p);
            setDest(p === "/" ? "" : p);
          }}
        />
      ) : (
        <p className="text-xs text-slate-400">Allowed destination roots</p>
      )}
      <Err error={listing.error} />
      <ul className="divide-y divide-slate-800">
        {listing.data?.entries.length === 0 && <Empty>No sub-folders</Empty>}
        {listing.data?.entries.map((e) => (
          <li key={e.path}>
            <button className="w-full truncate py-2 text-left text-sm" onClick={() => go(e)}>
              📁 {e.name}
            </button>
          </li>
        ))}
      </ul>
      {listing.data && listing.data.total_bytes > 0 ? (
        <p className="text-xs text-slate-500">
          {bytes(listing.data.free_bytes)} free of {bytes(listing.data.total_bytes)}
        </p>
      ) : null}
      {at ? (
        <div className="space-y-2 border-t border-slate-800 pt-2">
          <Field label="New folder here">
            <div className="flex gap-2">
              <input
                className={input}
                value={folder}
                placeholder="name"
                onChange={(e) => setFolder(e.target.value)}
              />
              <button
                className={ghost}
                disabled={!folder || mkdir.isPending}
                onClick={() => mkdir.mutate(folder)}
              >
                Create
              </button>
            </div>
          </Field>
          <Err error={mkdir.error} />
          <p className="truncate text-xs text-slate-400">Destination: {dest || "—"}</p>
        </div>
      ) : null}
    </Card>
  );
}

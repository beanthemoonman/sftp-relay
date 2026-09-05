import { useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { api, type Job } from "./api";
import { basename, bytes, eta, speed, when } from "./format";
import { Card, Err, danger, ghost } from "./ui";

const TONE: Record<Job["status"], string> = {
  queued: "bg-slate-700 text-slate-200",
  running: "bg-sky-600 text-white",
  paused: "bg-amber-700 text-amber-50",
  interrupted: "bg-amber-700 text-amber-50",
  done: "bg-emerald-700 text-emerald-50",
  failed: "bg-rose-700 text-rose-50",
  cancelled: "bg-slate-700 text-slate-300",
};

/** useJobs seeds the cache the SSE stream then keeps up to date. */
export function useJobs() {
  return useQuery({ queryKey: ["jobs"], queryFn: () => api.jobs() });
}

export function JobCard({ job, position }: { job: Job; position?: number }) {
  const qc = useQueryClient();
  const [showLog, setShowLog] = useState(false);
  const refresh = () => void qc.invalidateQueries({ queryKey: ["jobs"] });

  const cancel = useMutation({
    mutationFn: () => api.post(`/api/jobs/${job.id}/cancel`),
    onSuccess: refresh,
  });
  const retry = useMutation({
    mutationFn: () => api.post(`/api/jobs/${job.id}/retry`),
    onSuccess: refresh,
  });
  const remove = useMutation({
    mutationFn: () => api.del(`/api/jobs/${job.id}`),
    onSuccess: refresh,
  });

  const live = job.status === "running";
  const terminal = ["done", "failed", "cancelled"].includes(job.status);

  return (
    <Card className="space-y-2">
      <div className="flex items-start gap-2">
        <div className="min-w-0 flex-1">
          <div className="truncate text-sm font-medium">
            {job.kind === "dir" ? "📁 " : "📄 "}
            {basename(job.remote_path)}
          </div>
          <div className="truncate text-xs text-slate-500">
            {job.server_name} · {job.remote_path}
          </div>
          <div className="truncate text-xs text-slate-500">→ {job.dest_path}</div>
        </div>
        <span className={`rounded px-2 py-0.5 text-xs font-medium ${TONE[job.status]}`}>
          {job.status}
          {position ? ` #${position}` : ""}
        </span>
      </div>

      {!terminal && (
        <div className="h-1.5 overflow-hidden rounded-full bg-slate-800">
          <div
            className="h-full rounded-full bg-sky-500 transition-[width] duration-500"
            style={{ width: `${job.percent}%` }}
          />
        </div>
      )}

      <div className="flex flex-wrap items-center gap-x-3 gap-y-1 text-xs text-slate-400">
        <span>
          {bytes(job.transferred_bytes)}
          {job.total_bytes > 0 && ` / ${bytes(job.total_bytes)}`} ({job.percent}%)
        </span>
        {live && <span>{speed(job.speed_bps)}</span>}
        {live && <span>ETA {eta(job.eta_seconds)}</span>}
        {terminal && <span>{when(job.finished_at)}</span>}
        {job.exit_code !== null && job.exit_code !== 0 && <span>exit {job.exit_code}</span>}
      </div>

      {job.error ? <Err error={job.error} /> : null}

      <div className="flex flex-wrap gap-2">
        {!terminal && (
          <button className={danger} disabled={cancel.isPending} onClick={() => cancel.mutate()}>
            Cancel
          </button>
        )}
        {terminal && job.status !== "done" && (
          <button className={ghost} disabled={retry.isPending} onClick={() => retry.mutate()}>
            Retry
          </button>
        )}
        {terminal && (
          <button className={ghost} disabled={remove.isPending} onClick={() => remove.mutate()}>
            Delete
          </button>
        )}
        <button className={ghost} onClick={() => setShowLog((v) => !v)}>
          {showLog ? "Hide log" : "Log"}
        </button>
      </div>

      <Err error={cancel.error || retry.error || remove.error} />
      {showLog && <JobLog id={job.id} />}
    </Card>
  );
}

function JobLog({ id }: { id: number }) {
  const log = useQuery({ queryKey: ["job-log", id], queryFn: () => api.jobLog(id) });
  return (
    <pre className="max-h-48 overflow-auto rounded-lg bg-slate-950 p-2 text-[11px] leading-relaxed text-slate-400">
      {log.data?.length ? log.data.join("\n") : "no output yet"}
    </pre>
  );
}

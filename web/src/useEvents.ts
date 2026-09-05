import { useEffect, useState } from "react";
import { useQueryClient, type QueryClient } from "@tanstack/react-query";
import type { Job } from "./api";

const DELTA = ["job.created", "job.progress", "job.done", "job.failed"];
const LOG_LINES = 500; // matches the server-side ring buffer's spirit: bounded

type Frame = { type: string; job_id?: number; data?: unknown };

/** upsert keeps the list newest-first and never duplicates a job row. */
export function upsert(prev: Job[], job: Job): Job[] {
  const i = prev.findIndex((j) => j.id === job.id);
  if (i < 0) return [job, ...prev];
  const next = prev.slice();
  next[i] = job;
  return next;
}

export function applyFrame(qc: QueryClient, f: Frame) {
  if (f.type === "snapshot") {
    qc.setQueryData(["jobs"], f.data ?? []);
    return;
  }
  if (DELTA.includes(f.type) && f.data) {
    qc.setQueryData<Job[]>(["jobs"], (prev = []) => upsert(prev, f.data as Job));
    return;
  }
  if (f.type === "job.log" && f.job_id) {
    qc.setQueryData<string[]>(["job-log", f.job_id], (prev = []) =>
      [...prev, String(f.data)].slice(-LOG_LINES),
    );
  }
}

/**
 * useEvents holds the single SSE connection open and folds every frame into the
 * query cache, so components only ever read from TanStack Query. Reconnect is
 * explicit rather than relying on EventSource's own retry, because we want
 * backoff and a visible disconnected indicator.
 */
export function useEvents(): boolean {
  const qc = useQueryClient();
  const [connected, setConnected] = useState(false);

  useEffect(() => {
    let es: EventSource | null = null;
    let timer: ReturnType<typeof setTimeout> | undefined;
    let delay = 1000;
    let stopped = false;

    const open = () => {
      es = new EventSource("/api/events");
      es.onopen = () => {
        delay = 1000;
        setConnected(true);
      };
      const on = (type: string) =>
        es!.addEventListener(type, (e) => applyFrame(qc, JSON.parse((e as MessageEvent).data)));
      ["snapshot", "job.log", ...DELTA].forEach(on);
      es.onerror = () => {
        setConnected(false);
        es?.close();
        if (stopped) return;
        timer = setTimeout(open, delay);
        delay = Math.min(delay * 2, 30000);
      };
    };
    open();

    return () => {
      stopped = true;
      if (timer) clearTimeout(timer);
      es?.close();
    };
  }, [qc]);

  return connected;
}

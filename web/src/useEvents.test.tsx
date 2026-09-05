import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { renderHook, act } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { applyFrame, upsert, useEvents } from "./useEvents";
import type { Job } from "./api";

const job = (id: number, over: Partial<Job> = {}): Job =>
  ({
    id,
    server_id: 1,
    server_name: "s",
    kind: "file",
    remote_path: `/f${id}`,
    dest_path: "/volume1/media",
    status: "queued",
    total_bytes: 100,
    transferred_bytes: 0,
    speed_bps: 0,
    eta_seconds: 0,
    percent: 0,
    exit_code: null,
    error: "",
    created_at: "",
    started_at: "",
    finished_at: "",
    ...over,
  }) as Job;

/** A minimal stand-in for EventSource so the hook can be driven deterministically. */
class FakeES {
  static live: FakeES[] = [];
  onopen: (() => void) | null = null;
  onerror: (() => void) | null = null;
  closed = false;
  listeners: Record<string, ((e: MessageEvent) => void)[]> = {};
  constructor(public url: string) {
    FakeES.live.push(this);
  }
  addEventListener(type: string, fn: (e: MessageEvent) => void) {
    (this.listeners[type] ??= []).push(fn);
  }
  close() {
    this.closed = true;
  }
  emit(type: string, data: unknown) {
    for (const fn of this.listeners[type] ?? []) fn({ data: JSON.stringify(data) } as MessageEvent);
  }
}

describe("upsert", () => {
  it("prepends an unseen job and replaces a known one", () => {
    const list = upsert([], job(1));
    expect(list).toHaveLength(1);
    expect(upsert(list, job(2))[0].id).toBe(2);
    const updated = upsert(list, job(1, { percent: 50 }));
    expect(updated).toHaveLength(1);
    expect(updated[0].percent).toBe(50);
  });
});

describe("applyFrame", () => {
  let qc: QueryClient;
  beforeEach(() => {
    qc = new QueryClient();
  });

  it("replaces the whole list on a snapshot", () => {
    qc.setQueryData(["jobs"], [job(9)]);
    applyFrame(qc, { type: "snapshot", data: [job(1), job(2)] });
    expect(qc.getQueryData<Job[]>(["jobs"])?.map((j) => j.id)).toEqual([1, 2]);
  });

  it("treats an empty snapshot as an empty list, not as no news", () => {
    qc.setQueryData(["jobs"], [job(9)]);
    applyFrame(qc, { type: "snapshot" });
    expect(qc.getQueryData<Job[]>(["jobs"])).toEqual([]);
  });

  it("folds deltas into the list", () => {
    applyFrame(qc, { type: "job.created", job_id: 1, data: job(1) });
    applyFrame(qc, { type: "job.progress", job_id: 1, data: job(1, { percent: 42 }) });
    applyFrame(qc, { type: "job.done", job_id: 1, data: job(1, { status: "done", percent: 100 }) });
    const jobs = qc.getQueryData<Job[]>(["jobs"])!;
    expect(jobs).toHaveLength(1);
    expect(jobs[0].status).toBe("done");
  });

  it("appends log lines under the job's own key", () => {
    applyFrame(qc, { type: "job.log", job_id: 7, data: "one" });
    applyFrame(qc, { type: "job.log", job_id: 7, data: "two" });
    expect(qc.getQueryData<string[]>(["job-log", 7])).toEqual(["one", "two"]);
  });

  it("ignores an unknown frame type", () => {
    applyFrame(qc, { type: "nonsense", data: job(1) });
    expect(qc.getQueryData(["jobs"])).toBeUndefined();
  });
});

describe("useEvents", () => {
  const original = globalThis.EventSource;

  beforeEach(() => {
    FakeES.live = [];
    vi.useFakeTimers();
    (globalThis as any).EventSource = FakeES;
  });
  afterEach(() => {
    vi.useRealTimers();
    (globalThis as any).EventSource = original;
  });

  const setup = () => {
    const qc = new QueryClient();
    const view = renderHook(() => useEvents(), {
      wrapper: ({ children }) => <QueryClientProvider client={qc}>{children}</QueryClientProvider>,
    });
    return { qc, view };
  };

  it("reports connected and resyncs from the snapshot", () => {
    const { qc, view } = setup();
    act(() => FakeES.live[0].onopen!());
    expect(view.result.current).toBe(true);
    act(() => FakeES.live[0].emit("snapshot", { type: "snapshot", data: [job(3)] }));
    expect(qc.getQueryData<Job[]>(["jobs"])?.[0].id).toBe(3);
  });

  it("reconnects with backoff and resyncs again", () => {
    const { qc, view } = setup();
    act(() => FakeES.live[0].onopen!());
    expect(view.result.current).toBe(true);

    act(() => FakeES.live[0].onerror!());
    expect(view.result.current).toBe(false); // the disconnected indicator
    expect(FakeES.live[0].closed).toBe(true);
    expect(FakeES.live).toHaveLength(1); // nothing reopened before the delay

    act(() => void vi.advanceTimersByTime(1000));
    expect(FakeES.live).toHaveLength(2);

    // A second failure waits twice as long.
    act(() => FakeES.live[1].onerror!());
    act(() => void vi.advanceTimersByTime(1000));
    expect(FakeES.live).toHaveLength(2);
    act(() => void vi.advanceTimersByTime(1000));
    expect(FakeES.live).toHaveLength(3);

    act(() => FakeES.live[2].onopen!());
    act(() => FakeES.live[2].emit("snapshot", { type: "snapshot", data: [job(5)] }));
    expect(view.result.current).toBe(true);
    expect(qc.getQueryData<Job[]>(["jobs"])?.[0].id).toBe(5);
  });

  it("stops reconnecting once unmounted", () => {
    const { view } = setup();
    act(() => FakeES.live[0].onerror!());
    view.unmount();
    act(() => void vi.advanceTimersByTime(60000));
    expect(FakeES.live).toHaveLength(1);
  });
});

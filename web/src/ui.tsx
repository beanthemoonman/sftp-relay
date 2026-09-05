import type { ReactNode } from "react";

// ponytail: a handful of styled primitives instead of a component library.
// Upgrade path is a real design system, if this ever grows past five screens.

export const btn =
  "inline-flex items-center justify-center gap-1.5 rounded-lg px-3 py-2 text-sm font-medium " +
  "transition-colors disabled:opacity-40 disabled:pointer-events-none";
export const primary = `${btn} bg-sky-600 hover:bg-sky-500 text-white`;
export const ghost = `${btn} bg-slate-800 hover:bg-slate-700 text-slate-200`;
export const danger = `${btn} bg-rose-700 hover:bg-rose-600 text-white`;
export const input =
  "w-full rounded-lg bg-slate-900 border border-slate-700 px-3 py-2 text-sm " +
  "text-slate-100 placeholder-slate-500 focus:outline-none focus:border-sky-500";

export function Card({ children, className = "" }: { children: ReactNode; className?: string }) {
  return (
    <div className={`rounded-xl border border-slate-800 bg-slate-900/60 p-3 ${className}`}>
      {children}
    </div>
  );
}

export function Field({ label, children }: { label: string; children: ReactNode }) {
  return (
    <label className="block">
      <span className="mb-1 block text-xs font-medium uppercase tracking-wide text-slate-400">
        {label}
      </span>
      {children}
    </label>
  );
}

export function Err({ error }: { error: unknown }) {
  if (!error) return null;
  return (
    <p className="rounded-lg border border-rose-900 bg-rose-950/60 px-3 py-2 text-sm text-rose-200">
      {error instanceof Error ? error.message : String(error)}
    </p>
  );
}

export function Empty({ children }: { children: ReactNode }) {
  return <p className="px-1 py-8 text-center text-sm text-slate-500">{children}</p>;
}

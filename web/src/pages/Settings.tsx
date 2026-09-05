import { useEffect, useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { api, type Settings as SettingsMap } from "../api";
import { Card, Err, Field, ghost, input, primary } from "../ui";

// Only these keys are editable; the probed paths and the pinned NAS host key are
// shown read-only, because they are captured by the server, not typed.
const EDITABLE = [
  ["nas_host", "NAS host"],
  ["nas_user", "NAS SSH user"],
  ["nas_port", "NAS SSH port"],
  ["nas_tmp", "NAS temp directory"],
  ["allowed_dest_roots", "Allowed destination roots (one per line)"],
  ["concurrency", "Concurrent jobs"],
  ["segments", "lftp pget segments per file"],
  ["parallel", "Files in parallel per mirror"],
  ["history_retention_days", "History retention (days)"],
] as const;

const PROBED = [
  ["lftp_path", "lftp path on the NAS"],
  ["sshpass_path", "sshpass path on the NAS"],
] as const;

export default function Settings() {
  const qc = useQueryClient();
  const settings = useQuery({ queryKey: ["settings"], queryFn: api.settings });
  const [form, setForm] = useState<SettingsMap>({});
  const [saved, setSaved] = useState(false);

  useEffect(() => {
    if (settings.data) setForm(settings.data);
  }, [settings.data]);

  const save = useMutation({
    // Takes the payload explicitly so a click can save a value it has just
    // computed, without waiting for a state round-trip.
    mutationFn: (body: SettingsMap) => api.put("/api/settings", body),
    onSuccess: () => {
      setSaved(true);
      void qc.invalidateQueries({ queryKey: ["settings"] });
      void qc.invalidateQueries({ queryKey: ["nas"] });
    },
  });

  const set = (k: string, v: string) => {
    setSaved(false);
    setForm((f) => ({ ...f, [k]: v }));
  };

  return (
    <div className="space-y-3">
      <Err error={settings.error} />
      <Card className="space-y-3">
        {EDITABLE.map(([key, label]) => (
          <Field key={key} label={label}>
            {key === "allowed_dest_roots" ? (
              <textarea
                className={`${input} h-24 font-mono text-xs`}
                placeholder="/volume1/media"
                value={form[key] ?? ""}
                onChange={(e) => set(key, e.target.value)}
              />
            ) : (
              <input
                className={input}
                value={form[key] ?? ""}
                onChange={(e) => set(key, e.target.value)}
              />
            )}
          </Field>
        ))}
        <Err error={save.error} />
        <div className="flex items-center gap-2">
          <button className={primary} disabled={save.isPending} onClick={() => save.mutate(form)}>
            {save.isPending ? "Saving…" : "Save"}
          </button>
          {saved && <span className="text-xs text-emerald-400">Saved</span>}
        </div>
      </Card>

      <Card className="space-y-2 text-xs text-slate-400">
        {PROBED.map(([key, label]) => (
          <p key={key} className="truncate">
            {label}: <span className="text-slate-200">{settings.data?.[key] || "not found"}</span>
          </p>
        ))}
        <div className="flex items-center gap-2 pt-1">
          <span className="truncate">
            NAS host key:{" "}
            <span className="font-mono text-slate-200">
              {settings.data?.nas_host_key ? settings.data.nas_host_key.slice(0, 32) + "…" : "not pinned"}
            </span>
          </span>
          <button
            className={`${ghost} ml-auto shrink-0`}
            onClick={() => {
              const next = { ...form, nas_host_key: "" };
              setForm(next);
              save.mutate(next);
            }}
          >
            Clear
          </button>
        </div>
        <p className="pt-1">
          Clearing the pinned key makes the next connection trust whatever the NAS presents.
          Do it only when you changed the NAS, not when a key change surprises you.
        </p>
      </Card>
    </div>
  );
}

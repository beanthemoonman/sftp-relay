import { useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { api, type Server, type ServerInput, type TestResult } from "../api";
import { Card, Empty, Err, Field, danger, ghost, input, primary } from "../ui";

const blank: ServerInput = {
  name: "",
  host: "",
  port: 22,
  username: "",
  auth_type: "password",
  password: "",
  private_key: "",
  passphrase: "",
  host_key: "",
  default_remote_path: "/",
};

export default function Servers() {
  const qc = useQueryClient();
  const servers = useQuery({ queryKey: ["servers"], queryFn: api.servers });
  const [editing, setEditing] = useState<Server | "new" | null>(null);

  const done = () => {
    setEditing(null);
    void qc.invalidateQueries({ queryKey: ["servers"] });
  };

  const remove = useMutation({
    mutationFn: (id: number) => api.del(`/api/servers/${id}`),
    onSuccess: done,
  });

  if (editing) {
    return (
      <ServerForm
        server={editing === "new" ? null : editing}
        onDone={done}
        onCancel={() => setEditing(null)}
      />
    );
  }

  return (
    <div className="space-y-3">
      <button className={`${primary} w-full`} onClick={() => setEditing("new")}>
        Add server
      </button>
      <Err error={servers.error || remove.error} />
      {servers.data?.length === 0 && <Empty>No servers configured.</Empty>}
      {servers.data?.map((s) => (
        <ServerRow key={s.id} server={s} onEdit={() => setEditing(s)} onDelete={() => remove.mutate(s.id)} />
      ))}
    </div>
  );
}

function ServerRow({
  server,
  onEdit,
  onDelete,
}: {
  server: Server;
  onEdit: () => void;
  onDelete: () => void;
}) {
  const test = useMutation<TestResult>({
    mutationFn: () => api.post(`/api/servers/${server.id}/test`),
  });

  return (
    <Card className="space-y-2" data-testid="server" data-name={server.name}>
      <div className="flex items-start gap-2">
        <div className="min-w-0 flex-1">
          <div className="truncate text-sm font-medium">{server.name}</div>
          <div className="truncate text-xs text-slate-500">
            {server.username}@{server.host}:{server.port} · {server.auth_type}
            {server.host_key ? " · key pinned" : " · key not pinned"}
          </div>
        </div>
      </div>
      <div className="flex flex-wrap gap-2">
        <button className={ghost} disabled={test.isPending} onClick={() => test.mutate()}>
          {test.isPending ? "Testing…" : "Test connection"}
        </button>
        <button className={ghost} onClick={onEdit}>
          Edit
        </button>
        <button className={danger} onClick={onDelete}>
          Delete
        </button>
      </div>
      {test.data && (
        <p
          className={`text-xs ${test.data.ok ? "text-emerald-400" : "text-rose-400"}`}
          role="status"
        >
          {test.data.ok
            ? `OK — ${test.data.entries} entries in ${test.data.elapsed_ms} ms${
                test.data.host_key_new ? " (host key pinned now)" : ""
              }`
            : test.data.description}
        </p>
      )}
      <Err error={test.error} />
    </Card>
  );
}

function ServerForm({
  server,
  onDone,
  onCancel,
}: {
  server: Server | null;
  onDone: () => void;
  onCancel: () => void;
}) {
  const [form, setForm] = useState<ServerInput>(
    server ? { ...blank, ...server, password: "", private_key: "", passphrase: "" } : blank,
  );
  const set = (k: keyof ServerInput) => (e: { target: { value: string } }) =>
    setForm((f) => ({ ...f, [k]: k === "port" ? Number(e.target.value) : e.target.value }));

  const save = useMutation({
    mutationFn: () =>
      server ? api.put(`/api/servers/${server.id}`, form) : api.post("/api/servers", form),
    onSuccess: onDone,
  });

  return (
    <div className="space-y-3">
      <Card className="space-y-3">
        <Field label="Name">
          <input className={input} value={form.name} onChange={set("name")} />
        </Field>
        <div className="grid grid-cols-3 gap-2">
          <div className="col-span-2">
            <Field label="Host">
              <input className={input} value={form.host} onChange={set("host")} />
            </Field>
          </div>
          <Field label="Port">
            <input className={input} type="number" value={form.port} onChange={set("port")} />
          </Field>
        </div>
        <Field label="Username">
          <input className={input} value={form.username} onChange={set("username")} />
        </Field>
        <Field label="Authentication">
          <select className={input} value={form.auth_type} onChange={set("auth_type")}>
            <option value="password">Password</option>
            <option value="key">Private key</option>
          </select>
        </Field>
        {form.auth_type === "password" ? (
          <Field label={server?.has_password ? "Password (blank keeps stored)" : "Password"}>
            <input
              className={input}
              type="password"
              autoComplete="new-password"
              value={form.password}
              onChange={set("password")}
            />
          </Field>
        ) : (
          <>
            <Field label={server?.has_private_key ? "Private key (blank keeps stored)" : "Private key"}>
              <textarea
                className={`${input} h-32 font-mono text-xs`}
                placeholder="-----BEGIN OPENSSH PRIVATE KEY-----"
                value={form.private_key}
                onChange={set("private_key")}
              />
            </Field>
            <Field label="Key passphrase (optional)">
              <input
                className={input}
                type="password"
                autoComplete="new-password"
                value={form.passphrase}
                onChange={set("passphrase")}
              />
            </Field>
          </>
        )}
        <Field label="Default remote path">
          <input
            className={input}
            value={form.default_remote_path}
            onChange={set("default_remote_path")}
          />
        </Field>
        <Field label="Pinned host key (clear it to re-trust the server)">
          <input
            className={`${input} font-mono text-xs`}
            value={form.host_key}
            onChange={set("host_key")}
          />
        </Field>
        <Err error={save.error} />
        <div className="flex gap-2">
          <button className={primary} disabled={save.isPending} onClick={() => save.mutate()}>
            Save
          </button>
          <button className={ghost} onClick={onCancel}>
            Cancel
          </button>
        </div>
      </Card>
    </div>
  );
}

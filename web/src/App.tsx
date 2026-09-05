import { useEffect, useState } from "react";
import { useEvents } from "./useEvents";
import Browse from "./pages/Browse";
import Queue from "./pages/Queue";
import History from "./pages/History";
import Servers from "./pages/Servers";
import Settings from "./pages/Settings";

// ponytail: hash routing in ten lines instead of a router dependency. Five flat
// screens, no nested routes, no params. Add react-router when that stops being true.
const ROUTES = {
  browse: { label: "Browse", icon: "🗂", el: <Browse /> },
  queue: { label: "Queue", icon: "⏳", el: <Queue /> },
  history: { label: "History", icon: "🕘", el: <History /> },
  servers: { label: "Servers", icon: "🖧", el: <Servers /> },
  settings: { label: "Settings", icon: "⚙", el: <Settings /> },
};
type Route = keyof typeof ROUTES;

function useHash(): Route {
  const read = (): Route => {
    const h = window.location.hash.replace("#", "") as Route;
    return h in ROUTES ? h : "browse";
  };
  const [route, setRoute] = useState<Route>(read);
  useEffect(() => {
    const onHash = () => setRoute(read());
    window.addEventListener("hashchange", onHash);
    return () => window.removeEventListener("hashchange", onHash);
  }, []);
  return route;
}

export default function App() {
  const route = useHash();
  const connected = useEvents();
  const keys = Object.keys(ROUTES) as Route[];

  return (
    <div className="mx-auto flex min-h-dvh max-w-5xl flex-col">
      <header className="sticky top-0 z-10 flex items-center gap-3 border-b border-slate-800 bg-slate-950/95 px-4 py-3 backdrop-blur">
        <h1 className="text-base font-semibold tracking-tight">sftp-relay</h1>
        <nav className="ml-auto hidden gap-1 sm:flex">
          {keys.map((k) => (
            <a
              key={k}
              href={`#${k}`}
              className={`rounded-lg px-3 py-1.5 text-sm ${
                route === k ? "bg-slate-800 text-white" : "text-slate-400 hover:text-slate-200"
              }`}
            >
              {ROUTES[k].label}
            </a>
          ))}
        </nav>
        <span
          title={connected ? "Live" : "Disconnected — reconnecting"}
          className={`ml-auto h-2.5 w-2.5 shrink-0 rounded-full sm:ml-2 ${
            connected ? "bg-emerald-500" : "animate-pulse bg-amber-500"
          }`}
        />
      </header>

      <main className="flex-1 p-3 pb-24 sm:pb-6">{ROUTES[route].el}</main>

      <nav className="fixed inset-x-0 bottom-0 z-10 grid grid-cols-5 border-t border-slate-800 bg-slate-950/95 pb-[env(safe-area-inset-bottom)] backdrop-blur sm:hidden">
        {keys.map((k) => (
          <a
            key={k}
            href={`#${k}`}
            className={`flex flex-col items-center gap-0.5 py-2 text-[11px] ${
              route === k ? "text-sky-400" : "text-slate-500"
            }`}
          >
            <span className="text-lg leading-none">{ROUTES[k].icon}</span>
            {ROUTES[k].label}
          </a>
        ))}
      </nav>
    </div>
  );
}

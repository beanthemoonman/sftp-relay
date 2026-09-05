import React from "react";
import { createRoot } from "react-dom/client";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import App from "./App";
import "./index.css";

const client = new QueryClient({
  defaultOptions: {
    // Jobs arrive over SSE, so background refetching is mostly wasted work on a
    // phone. Everything else is cheap enough to refetch on focus.
    queries: { retry: 1, refetchOnWindowFocus: true, staleTime: 5000 },
  },
});

createRoot(document.getElementById("root")!).render(
  <React.StrictMode>
    <QueryClientProvider client={client}>
      <App />
    </QueryClientProvider>
  </React.StrictMode>,
);

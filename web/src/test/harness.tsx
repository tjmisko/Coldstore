import { render, type RenderOptions } from "@testing-library/react";
import type { ReactElement, ReactNode } from "react";
import { ToastContext, type Toast, type ToastKind } from "../lib/toast";

/** Toasts collected during a test, so a test can assert on the message an
 *  action reported rather than on a spy that proves only that something was
 *  called. */
export interface RecordedToasts {
  all: Omit<Toast, "id">[];
  messages(): string[];
  last(): Omit<Toast, "id"> | undefined;
}

export function renderWithToasts(
  ui: ReactElement,
  options?: Omit<RenderOptions, "wrapper">,
): ReturnType<typeof render> & { toasts: RecordedToasts } {
  const all: Omit<Toast, "id">[] = [];
  const toasts: RecordedToasts = {
    all,
    messages: () => all.map((t) => t.message),
    last: () => all[all.length - 1],
  };

  const show = (kind: ToastKind, message: string) => {
    all.push({ kind, message });
  };

  const wrapper = ({ children }: { children: ReactNode }) => (
    <ToastContext value={{ show }}>{children}</ToastContext>
  );

  return { ...render(ui, { ...options, wrapper }), toasts };
}

/** A fetch stub that answers by URL+method, records every call, and fails
 *  loudly on a request no test set up -- an unexpected request is a defect,
 *  not a 404 to be silently absorbed. */
export interface StubRoute {
  method?: string;
  url: string | RegExp;
  status?: number;
  body?: unknown;
  /** Resolve only when the returned promise is settled by the test. */
  delay?: Promise<void>;
}

export interface FetchStub {
  calls: { url: string; method: string; body?: unknown }[];
  urls(): string[];
  route(route: StubRoute): void;
  restore(): void;
}

export function stubFetch(routes: StubRoute[] = []): FetchStub {
  const table = [...routes];
  const calls: FetchStub["calls"] = [];
  const original = globalThis.fetch;

  globalThis.fetch = (async (input: RequestInfo | URL, init?: RequestInit) => {
    const url = typeof input === "string" ? input : input.toString();
    const method = (init?.method ?? "GET").toUpperCase();
    const body = init?.body ? JSON.parse(String(init.body)) : undefined;
    calls.push({ url, method, body });

    // Last matching route wins, so a test can override a default.
    const match = [...table]
      .reverse()
      .find(
        (r) =>
          (r.method ?? "GET").toUpperCase() === method &&
          (typeof r.url === "string" ? url.startsWith(r.url) : r.url.test(url)),
      );

    if (!match) {
      throw new Error(`no stub for ${method} ${url}`);
    }
    if (match.delay) await match.delay;

    const status = match.status ?? 200;
    return {
      ok: status >= 200 && status < 300,
      status,
      statusText: String(status),
      json: async () => match.body,
    } as Response;
  }) as typeof fetch;

  return {
    calls,
    urls: () => calls.map((c) => c.url),
    route: (route: StubRoute) => table.push(route),
    restore: () => {
      globalThis.fetch = original;
    },
  };
}

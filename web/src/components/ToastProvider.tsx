import { useCallback, useEffect, useMemo, useRef, useState, type ReactNode } from "react";
import { ToastContext, type Toast, type ToastKind } from "../lib/toast";
import { Z } from "../lib/zLayers";

const DISMISS_MS: Record<ToastKind, number> = {
  success: 4000,
  info: 8000,
  // Errors stay long enough to be read and copied. Every per-file action and
  // the bulk bar report failures here; before this existed they went to
  // console.error, where nobody but a developer ever saw them.
  error: 20000,
};

export function ToastProvider({ children }: { children: ReactNode }) {
  const [toasts, setToasts] = useState<Toast[]>([]);
  const nextID = useRef(0);
  const timers = useRef<Set<ReturnType<typeof setTimeout>>>(new Set());

  const dismiss = useCallback((id: number) => {
    setToasts((prev) => prev.filter((t) => t.id !== id));
  }, []);

  const show = useCallback(
    (kind: ToastKind, message: string) => {
      const id = ++nextID.current;
      setToasts((prev) => [...prev, { id, kind, message }]);
      const timer = setTimeout(() => {
        timers.current.delete(timer);
        dismiss(id);
      }, DISMISS_MS[kind]);
      timers.current.add(timer);
    },
    [dismiss],
  );

  useEffect(() => {
    const pending = timers.current;
    return () => {
      for (const timer of pending) clearTimeout(timer);
      pending.clear();
    };
  }, []);

  const api = useMemo(() => ({ show }), [show]);

  return (
    <ToastContext value={api}>
      {children}
      <div
        style={{ zIndex: Z.toast }}
        className="fixed bottom-4 right-4 flex flex-col gap-2 w-80 max-w-[calc(100vw-2rem)]"
        aria-live="polite"
      >
        {toasts.map((toast) => (
          <div
            key={toast.id}
            role={toast.kind === "error" ? "alert" : "status"}
            data-toast={toast.kind}
            className={`rounded-lg border px-3 py-2.5 text-base shadow-lg backdrop-blur ${
              toast.kind === "error"
                ? "bg-alarm-dim/95 border-alarm/45 text-ink"
                : toast.kind === "success"
                  ? "bg-surface/95 border-line-strong text-ink"
                  : "bg-surface/95 border-line text-ink-muted"
            }`}
          >
            <div className="flex items-start gap-2">
              <span className="flex-1 break-words">{toast.message}</span>
              <button
                onClick={() => dismiss(toast.id)}
                aria-label="Dismiss"
                className="text-ink-faint hover:text-ink leading-none focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-cursor rounded"
              >
                &times;
              </button>
            </div>
          </div>
        ))}
      </div>
    </ToastContext>
  );
}

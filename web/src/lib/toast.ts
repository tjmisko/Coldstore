import { createContext, use } from "react";

export type ToastKind = "error" | "info" | "success";

export interface Toast {
  id: number;
  kind: ToastKind;
  message: string;
}

export interface ToastApi {
  /** show returns nothing on purpose: a toast is a report, never a promise the
   *  caller can await. Actions that need a result use the API directly. */
  show(kind: ToastKind, message: string, durationMs?: number): void;
}

/** The default is a no-op rather than a throw. A component rendered outside the
 *  provider in a test should still be testable; losing a transient message is
 *  not worth an unmountable component tree. */
export const ToastContext = createContext<ToastApi>({ show: () => {} });

export function useToast(): ToastApi {
  return use(ToastContext);
}

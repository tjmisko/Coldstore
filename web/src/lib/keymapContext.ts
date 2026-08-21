import { createContext, use, type RefObject } from "react";
import type { Command, Scope } from "../keymap/types";

/** A layer holds its handler in a ref rather than as a value, so a component
 *  that re-renders every keystroke does not churn the layer stack. */
export interface KeymapLayer {
  scope: Scope;
  handler: RefObject<((command: Command) => boolean) | null>;
}

export interface KeymapApi {
  /** Register a scope layer. Returns the unregister function. */
  register(layer: KeymapLayer): () => void;
  /** Drop a half-typed command. Called when an overlay takes the keyboard. */
  reset(): void;
  /** Run a command as if it had been typed.
   *
   *  The palette, the context menu and the cheatsheet all go through here, so
   *  "archive the selection" has exactly one implementation whether it arrived
   *  as `d`, as `:archive` or as a right-click. */
  dispatch(command: Command): boolean;
}

const NOOP: KeymapApi = {
  register: () => () => {},
  reset: () => {},
  dispatch: () => false,
};

/** The default is inert rather than a throw, for the same reason ToastContext's
 *  is: a component rendered outside the provider in a test should still be
 *  testable. */
export const KeymapContext = createContext<KeymapApi>(NOOP);

export function useKeymap(): KeymapApi {
  return use(KeymapContext);
}

/** How the scope stack is ordered, independent of mount order.
 *
 *  React mounts children before parents, so "whoever registered last is on top"
 *  is true for an overlay opened later and false on a first paint. An explicit
 *  number is the only version of this that does not depend on where a component
 *  happens to sit in the tree. */
export const SCOPE_PRIORITY: Record<Scope, number> = {
  global: 0,
  gallery: 10,
  browser: 10,
  lightbox: 20,
  overlay: 30,
};

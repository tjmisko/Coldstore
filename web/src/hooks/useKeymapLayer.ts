import { useEffect, useRef } from "react";
import { useKeymap } from "../lib/keymapContext";
import type { Command, Scope } from "../keymap/types";

/** useKeymapLayer puts a view on the scope stack for as long as it is mounted.
 *
 *  The handler returns true when it consumed the command and false to let the
 *  layer below have it, which is what lets the lightbox take `q` while `d`
 *  falls through to the same archive path the grid uses. */
export function useKeymapLayer(
  scope: Scope,
  handler: (command: Command) => boolean,
  active = true,
): void {
  const keymap = useKeymap();
  const handlerRef = useRef<((command: Command) => boolean) | null>(null);

  // No dependency array: the ref must hold the CURRENT closure, and this effect
  // is declared first so it has already run by the time the registration
  // effect below fires.
  useEffect(() => {
    handlerRef.current = handler;
  });

  useEffect(() => {
    if (!active) return;
    return keymap.register({ scope, handler: handlerRef });
  }, [keymap, scope, active]);
}

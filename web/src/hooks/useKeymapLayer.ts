import { useEffect, useLayoutEffect, useRef } from "react";
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
  onRawKey?: (key: string) => void,
): void {
  const keymap = useKeymap();
  const handlerRef = useRef<((command: Command) => boolean) | null>(null);
  const rawHandlerRef = useRef<((key: string) => void) | null>(null);

  // Publish the handler in the layout phase. A passive effect leaves a window
  // after a commit where newly visible fetched rows are paired with the
  // previous render's closure, so an immediate key can be handled as though
  // the list were still empty. Writing during render would have the opposite
  // concurrency bug: an abandoned render could leak an uncommitted closure.
  useLayoutEffect(() => {
    handlerRef.current = handler;
    rawHandlerRef.current = onRawKey ?? null;
  });

  useEffect(() => {
    if (!active) return;
    return keymap.register({ scope, handler: handlerRef, rawHandler: rawHandlerRef });
  }, [keymap, scope, active]);
}

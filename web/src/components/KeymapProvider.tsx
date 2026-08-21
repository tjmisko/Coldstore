import { useCallback, useEffect, useMemo, useRef, type ReactNode } from "react";
import { EXCLUSIVE_SCOPES, registry } from "../keymap/bindings";
import { normalizeKey } from "../keymap/keys";
import { initialState, pendingLabel, step, type MachineState } from "../keymap/machine";
import { scopeChain } from "../keymap/registry";
import { isTextEntryTarget } from "../keymap/textEntry";
import type { Command, Scope } from "../keymap/types";
import { KeymapContext, SCOPE_PRIORITY, type KeymapApi, type KeymapLayer } from "../lib/keymapContext";
import { Z } from "../lib/zLayers";

interface Entry extends KeymapLayer {
  id: number;
}

/** The one window keydown listener in the app.
 *
 *  It replaces three that raced each other -- Gallery's, Lightbox's and
 *  ConfirmDialog's -- of which only one had an "am I typing?" guard, and that
 *  one had it inline. Scope decides who gets a key now; document order used to.
 */
export function KeymapProvider({ children }: { children: ReactNode }) {
  const layers = useRef<Entry[]>([]);
  const nextID = useRef(0);
  const machine = useRef<MachineState>(initialState);
  const timer = useRef<ReturnType<typeof setTimeout> | undefined>(undefined);
  const status = useRef<HTMLDivElement | null>(null);

  /** The pending indicator is written straight to the DOM rather than held in
   *  state. A keystroke must not re-render a grid of five hundred tiles to
   *  redraw four characters in a corner, and the machine is not React state --
   *  it is a ref that a listener folds over. */
  const paint = useCallback((state: MachineState) => {
    const el = status.current;
    if (!el) return;
    const label = pendingLabel(state);
    const mode =
      state.mode === "visual" ? "-- VISUAL --" : state.mode === "visual-line" ? "-- VISUAL LINE --" : "";
    el.setAttribute("data-keymap-pending", label);
    el.setAttribute("data-keymap-mode", state.mode);
    el.textContent = [mode, label].filter(Boolean).join("  ");
    el.hidden = label === "" && mode === "";
  }, []);

  const ordered = useCallback((): Entry[] => {
    return [...layers.current].sort(
      (a, b) => SCOPE_PRIORITY[a.scope] - SCOPE_PRIORITY[b.scope] || a.id - b.id,
    );
  }, []);

  const chain = useCallback((): Scope[] => {
    const stack = ordered().map((layer) => layer.scope);
    return scopeChain(stack, EXCLUSIVE_SCOPES);
  }, [ordered]);

  /** Nearest layer first. An overlay only claims `cancel`, so `:archive` typed
   *  into the palette still reaches the grid below it -- which is the whole
   *  reason the palette does not need its own copy of every file action. A
   *  typed key cannot leak past an overlay by this route, because an exclusive
   *  scope resolves against zero bindings and produces no commands. */
  const dispatch = useCallback(
    (command: Command): boolean => {
      const stack = ordered();
      for (let i = stack.length - 1; i >= 0; i--) {
        const handler = stack[i].handler.current;
        if (handler && handler(command)) return true;
      }
      return false;
    },
    [ordered],
  );

  const clearTimer = useCallback(() => {
    if (timer.current !== undefined) {
      clearTimeout(timer.current);
      timer.current = undefined;
    }
  }, []);

  const reset = useCallback(() => {
    clearTimer();
    machine.current = initialState;
    paint(machine.current);
  }, [clearTimer, paint]);

  const register = useCallback(
    (layer: KeymapLayer) => {
      const entry: Entry = { ...layer, id: ++nextID.current };
      layers.current = [...layers.current, entry];
      // A layer appearing or disappearing changes which bindings are in force,
      // so a command half-typed against the old chain is no longer meaningful.
      reset();
      return () => {
        layers.current = layers.current.filter((l) => l.id !== entry.id);
        reset();
      };
    },
    [reset],
  );

  const api = useMemo<KeymapApi>(() => ({ register, reset, dispatch }), [register, reset, dispatch]);

  useEffect(() => {
    const advance = (result: ReturnType<typeof step>) => {
      machine.current = result.state;
      clearTimer();
      for (const command of result.commands) dispatch(command);
      paint(machine.current);
      if (result.awaitTimeout !== null) {
        timer.current = setTimeout(() => {
          timer.current = undefined;
          const bindings = registry.resolve(chain());
          advance(step(machine.current, { type: "timeout" }, bindings));
        }, result.awaitTimeout);
      }
    };

    const onKeyDown = (event: KeyboardEvent) => {
      // The guard comes first, before normalisation and before any dispatch, so
      // a keystroke meant for a text field is never even looked at: `20 GB`
      // typed into a budget editor is `2`, `0`, `G`, `B` to the machine, and
      // `G` is a motion to the end of the folder.
      if (isTextEntryTarget(event.target)) return;
      if (event.isComposing) return;
      const key = normalizeKey(event);
      if (key === null) return;

      const bindings = registry.resolve(chain());
      const result = step(machine.current, { type: "key", key }, bindings);
      advance(result);
      if (result.handled) event.preventDefault();
    };

    window.addEventListener("keydown", onKeyDown);
    return () => {
      window.removeEventListener("keydown", onKeyDown);
      clearTimer();
    };
  }, [chain, clearTimer, dispatch, paint]);

  return (
    <KeymapContext value={api}>
      {children}
      <div
        ref={status}
        hidden
        aria-live="off"
        data-keymap-pending=""
        data-keymap-mode="normal"
        style={{ zIndex: Z.keymapStatus }}
        className="fixed bottom-2 left-2 rounded border border-cursor/30 bg-cursor-dim/95 px-2 py-0.5 font-mono text-meta text-cursor pointer-events-none"
      />
    </KeymapContext>
  );
}

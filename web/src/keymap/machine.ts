import { writeSequence } from "./keys";
import type { ResolvedBindings } from "./registry";
import { NO_COUNT, type Binding, type Command, type Mode, type OperatorId } from "./types";

/** How long an exact match waits for a longer one, in ms. Vim's `timeoutlen`.
 *  It is returned from `step` as a NUMBER rather than scheduled here, so the
 *  machine never reads a clock and the tests never sleep. */
export const TIMEOUT_MS = 800;

/** `999999999j` is a typo, and an uncapped count is an easy way to make the
 *  interpreter allocate a span the size of the catalog. */
export const MAX_COUNT_DIGITS = 5;

export interface MachineState {
  mode: Mode;
  /** Digits typed before an operator, or before a motion in normal/visual. */
  count: string;
  /** Operator-pending is not a mode: it is exactly `operator !== null`. Giving
   *  it its own enum value would let `mode` and `operator` disagree. */
  operator: OperatorId | null;
  /** The tokens that started the operator, so the doubled form (`dd`) can be
   *  recognised without the binding table having to enumerate it. */
  operatorKeys: string[];
  /** Digits typed AFTER the operator. `2d3j` is `d6j`: the counts multiply. */
  operatorCount: string;
  /** A partially matched sequence such as ["g"]. */
  pending: string[];
}

export const initialState: MachineState = {
  mode: "normal",
  count: "",
  operator: null,
  operatorKeys: [],
  operatorCount: "",
  pending: [],
};

export type Input =
  | { type: "key"; key: string }
  | { type: "timeout" }
  | { type: "reset" };

export interface Step {
  state: MachineState;
  commands: Command[];
  /** ms the caller should wait before feeding a `timeout` input back in, or
   *  null. Only ever set when an exact match is being held because a longer
   *  binding also starts with these keys. */
  awaitTimeout: number | null;
  /** True when the engine consumed the key and the caller should
   *  preventDefault. A dead sequence is deliberately NOT handled, so the
   *  browser keeps its own shortcuts. */
  handled: boolean;
}

const DIGIT = /^[0-9]$/;
const CANCEL_KEYS = new Set(["<Esc>", "<C-c>"]);

function countOf(text: string): number {
  return text === "" ? 0 : Number.parseInt(text, 10);
}

/** The effective count is the PRODUCT of the two buffers, because that is what
 *  vim does: `2d3j` operates over six items, not two and not three. */
function effectiveCount(state: MachineState): number {
  const before = countOf(state.count);
  const after = countOf(state.operatorCount);
  if (before === 0 && after === 0) return NO_COUNT;
  return (before || 1) * (after || 1);
}

function idle(mode: Mode): MachineState {
  return { ...initialState, mode };
}

function done(state: MachineState, commands: Command[]): Step {
  return { state, commands, awaitTimeout: null, handled: true };
}

function fire(state: MachineState, binding: Binding, sequence: string[]): Step {
  const count = effectiveCount(state);
  const command = binding.command;

  switch (command.kind) {
    case "motion":
      if (state.operator !== null) {
        return done(idle("normal"), [
          {
            kind: "operator",
            operator: state.operator,
            target: { kind: "motion", motion: command.motion, count },
          },
        ]);
      }
      return done(idle(state.mode), [
        { kind: "motion", motion: command.motion, count, extend: state.mode !== "normal" },
      ]);

    case "operator":
      // In visual an operator executes immediately over the highlighted span.
      // The operator command comes FIRST so the interpreter can read the span
      // before the mode change tears it down.
      if (state.mode !== "normal") {
        return done(idle("normal"), [
          { kind: "operator", operator: command.operator, target: { kind: "selection" } },
          { kind: "mode", mode: "normal", from: state.mode },
        ]);
      }
      return done(
        {
          ...state,
          operator: command.operator,
          operatorKeys: [...sequence],
          operatorCount: "",
          pending: [],
        },
        [],
      );

    case "action":
      return done(idle(state.mode), [{ kind: "action", action: command.action, count }]);

    case "visual": {
      const from = state.mode;
      const to: Mode = state.mode === command.mode ? "normal" : command.mode;
      return done(idle(to), [{ kind: "mode", mode: to, from }]);
    }
  }
}

function onKey(state: MachineState, key: string, bindings: ResolvedBindings): Step {
  // Cancel is checked before everything, in every state. There is no sequence
  // it can be a prefix of, and a cancel that can itself get stuck pending is
  // not a cancel.
  if (CANCEL_KEYS.has(key)) {
    if (state.mode !== "normal") {
      return done(initialState, [{ kind: "mode", mode: "normal", from: state.mode }]);
    }
    const busy = state.count !== "" || state.operator !== null || state.pending.length > 0;
    if (busy) return done(initialState, []);
    // Nothing pending: Escape means what it meant before this engine existed --
    // clear the selection, close the lightbox -- and the active scope decides
    // which.
    return done(initialState, [{ kind: "action", action: "cancel", count: NO_COUNT }]);
  }

  // Digits, but only at the start of a command: once a sequence is pending,
  // a digit may be part of that sequence rather than a count.
  if (state.pending.length === 0 && DIGIT.test(key)) {
    const pendingOperator = state.operator !== null;
    const buffer = pendingOperator ? state.operatorCount : state.count;
    // `0` with an empty buffer is a motion, not a digit. Vim's rule, and the
    // reason `0` cannot simply be a binding.
    if (!(key === "0" && buffer === "")) {
      if (buffer.length >= MAX_COUNT_DIGITS) return done(state, []);
      const grown = buffer + key;
      return done(
        pendingOperator ? { ...state, operatorCount: grown } : { ...state, count: grown },
        [],
      );
    }
  }

  // The doubled operator: `dd`, `3dd`. Recognised here rather than as a binding
  // so that adding an operator does not also mean adding its doubled row.
  if (
    state.operator !== null &&
    state.pending.length === 0 &&
    state.operatorKeys.length === 1 &&
    state.operatorKeys[0] === key
  ) {
    return done(initialState, [
      {
        kind: "operator",
        operator: state.operator,
        target: { kind: "line", count: effectiveCount(state) },
      },
    ]);
  }

  const sequence = [...state.pending, key];
  const options = { motionsOnly: state.operator !== null };
  const exact = bindings.match(sequence, options);
  const longer = bindings.hasPrefix(sequence, options);

  if (exact && !longer) return fire(state, exact, sequence);
  if (exact && longer) {
    // Ambiguous: hold the exact match and wait, exactly as vim does for a
    // binding that is a prefix of another.
    return { state: { ...state, pending: sequence }, commands: [], awaitTimeout: TIMEOUT_MS, handled: true };
  }
  if (longer) return done({ ...state, pending: sequence }, []);

  // Dead sequence. Drop everything and let the browser have the key: an engine
  // that swallows keys it has no binding for is one that breaks Ctrl+R.
  return { state: idle(state.mode), commands: [], awaitTimeout: null, handled: false };
}

function onTimeout(state: MachineState, bindings: ResolvedBindings): Step {
  if (state.pending.length === 0) {
    return { state, commands: [], awaitTimeout: null, handled: false };
  }
  const exact = bindings.match(state.pending, { motionsOnly: state.operator !== null });
  if (!exact) return done(idle(state.mode), []);
  return fire({ ...state, pending: [] }, exact, state.pending);
}

/** step is the whole engine: total, synchronous, and free of the DOM.
 *
 *  Every grammar test drives this directly with an array of tokens, which is
 *  the only reason `2d3j`, `d` plus a timeout, `v3jd` and `ggVG` are cheap
 *  enough to cover exhaustively. */
export function step(state: MachineState, input: Input, bindings: ResolvedBindings): Step {
  switch (input.type) {
    case "reset":
      return done(initialState, []);
    case "timeout":
      return onTimeout(state, bindings);
    case "key":
      return onKey(state, input.key, bindings);
  }
}

/** What to show the owner while a command is half-typed: "2d3", "g".
 *  Empty when the machine is idle. */
export function pendingLabel(state: MachineState): string {
  const parts = [state.count];
  if (state.operator !== null) parts.push(writeSequence(state.operatorKeys), state.operatorCount);
  parts.push(writeSequence(state.pending));
  return parts.join("");
}

export function isIdle(state: MachineState): boolean {
  return (
    state.count === "" &&
    state.operator === null &&
    state.operatorCount === "" &&
    state.pending.length === 0
  );
}

import { describe, expect, it } from "vitest";
import { parseSequence } from "./keys";
import { registry } from "./bindings";
import { initialState, pendingLabel, step, type MachineState } from "./machine";
import { createRegistry, defineBinding, type KeymapRegistry } from "./registry";
import type { Command, Scope } from "./types";

/** Not a real key: written into a run to feed the timeout input in, so the
 *  ambiguity tests never sleep and never read a clock. */
const TIMEOUT = "<timeout>";

/** Drive the machine with a written key sequence, spelled exactly as the
 *  binding table spells one. This is the whole reason the grammar is a pure
 *  function rather than a component. */
function run(
  written: string,
  options: { chain?: Scope[]; from?: MachineState; registry?: KeymapRegistry } = {},
) {
  const table = options.registry ?? registry;
  const bindings = table.resolve(options.chain ?? ["gallery", "global"]);
  let state = options.from ?? initialState;
  const commands: Command[] = [];
  const handled: boolean[] = [];

  for (const token of parseSequence(written)) {
    const result =
      token === TIMEOUT
        ? step(state, { type: "timeout" }, bindings)
        : step(state, { type: "key", key: token }, bindings);
    state = result.state;
    commands.push(...result.commands);
    handled.push(result.handled);
  }
  return { state, commands, handled };
}

describe("counts", () => {
  it("should multiply the two counts when both sides of an operator carry one", () => {
    // 2d3j is d6j. Vim's rule, and the one the owner will assume.
    const { commands } = run("2d3j");
    expect(commands).toEqual([
      { kind: "operator", operator: "archive", target: { kind: "motion", motion: "down", count: 6 } },
    ]);
  });

  it("should accumulate multi-digit counts when digits are typed in a row", () => {
    const { commands } = run("12j");
    expect(commands).toEqual([{ kind: "motion", motion: "down", count: 12, extend: false }]);
  });

  it("should report no count at all when none was typed", () => {
    // 0 is not the same as 1: gg and 1gg agree, but G and 1G do not.
    const { commands } = run("G");
    expect(commands).toEqual([{ kind: "motion", motion: "last", count: 0, extend: false }]);
  });

  it("should treat 0 as a motion when the count buffer is empty", () => {
    const { commands } = run("0");
    expect(commands).toEqual([{ kind: "motion", motion: "row-start", count: 0, extend: false }]);
  });

  it("should treat 0 as a digit when a count is already being typed", () => {
    const { commands } = run("10j");
    expect(commands).toEqual([{ kind: "motion", motion: "down", count: 10, extend: false }]);
  });

  it("should stop accepting digits when the count gets absurd", () => {
    const { state } = run("1234567");
    expect(state.count).toBe("12345");
  });
});

describe("operators", () => {
  it("should archive only the cursor file when the operator is doubled", () => {
    const { commands } = run("dd");
    expect(commands).toEqual([
      { kind: "operator", operator: "archive", target: { kind: "line", count: 0 } },
    ]);
  });

  it("should carry the count into the doubled form when one was typed", () => {
    const { commands } = run("3dd");
    expect(commands).toEqual([
      { kind: "operator", operator: "archive", target: { kind: "line", count: 3 } },
    ]);
  });

  it("should compose every operator with every motion", () => {
    for (const [key, operator] of [
      ["d", "archive"],
      ["r", "restore"],
      ["p", "pin"],
      ["u", "unpin"],
      ["y", "download"],
    ] as const) {
      const { commands } = run(`${key}G`);
      expect(commands).toEqual([
        { kind: "operator", operator, target: { kind: "motion", motion: "last", count: 0 } },
      ]);
    }
  });

  it("should reach a multi-key motion from operator-pending when gg is typed", () => {
    const { commands } = run("dgg");
    expect(commands).toEqual([
      { kind: "operator", operator: "archive", target: { kind: "motion", motion: "first", count: 0 } },
    ]);
  });

  it("should emit nothing and stay pending when only the operator has been typed", () => {
    const { commands, state } = run("d");
    expect(commands).toEqual([]);
    expect(state.operator).toBe("archive");
  });

  it("should cancel the operator when the next key is not a range", () => {
    // dx is not a command. Without this, a fat-finger after d runs whatever the
    // second key happens to mean in normal mode.
    const { commands, state } = run("dx");
    expect(commands).toEqual([]);
    expect(state.operator).toBeNull();
  });

  it("should refuse a leader sequence while an operator is pending", () => {
    const { commands, state } = run("d<Space>a");
    expect(commands).toEqual([]);
    expect(state.operator).toBeNull();
  });
});

describe("cancelling", () => {
  it("should drop a half-typed command when escape is pressed", () => {
    const { commands, state } = run("2d3<Esc>");
    expect(commands).toEqual([]);
    expect(state).toEqual(initialState);
  });

  it("should emit cancel when escape is pressed with nothing pending", () => {
    const { commands } = run("<Esc>");
    expect(commands).toEqual([{ kind: "action", action: "cancel", count: 0 }]);
  });

  it("should cancel on ctrl-c as well as escape", () => {
    const { state } = run("2d3<C-c>");
    expect(state).toEqual(initialState);
  });

  it("should leave visual mode rather than clearing the selection when escape is pressed there", () => {
    const { commands, state } = run("v<Esc>");
    expect(state.mode).toBe("normal");
    expect(commands).toEqual([
      { kind: "mode", mode: "visual", from: "normal" },
      { kind: "mode", mode: "normal", from: "visual" },
    ]);
  });
});

describe("visual mode", () => {
  it("should extend rather than jump when a motion runs in visual mode", () => {
    const { commands } = run("v3j");
    expect(commands).toEqual([
      { kind: "mode", mode: "visual", from: "normal" },
      { kind: "motion", motion: "down", count: 3, extend: true },
    ]);
  });

  it("should run an operator over the selection immediately when in visual mode", () => {
    // The operator comes first so the interpreter can read the highlighted span
    // before the mode change tears it down.
    const { commands } = run("v3jd");
    expect(commands.slice(2)).toEqual([
      { kind: "operator", operator: "archive", target: { kind: "selection" } },
      { kind: "mode", mode: "normal", from: "visual" },
    ]);
  });

  it("should leave visual mode when the same visual key is pressed again", () => {
    const { state } = run("vv");
    expect(state.mode).toBe("normal");
  });

  it("should switch between charwise and linewise when the other visual key is pressed", () => {
    const { state } = run("vV");
    expect(state.mode).toBe("visual-line");
  });

  it("should select the whole folder when ggVG is typed", () => {
    const { commands, state } = run("ggVG");
    expect(commands).toEqual([
      { kind: "motion", motion: "first", count: 0, extend: false },
      { kind: "mode", mode: "visual-line", from: "normal" },
      { kind: "motion", motion: "last", count: 0, extend: true },
    ]);
    expect(state.mode).toBe("visual-line");
  });
});

describe("multi-key sequences", () => {
  it("should hold the keys and emit nothing when a sequence is still ambiguous", () => {
    const { commands, state } = run("<Space>f");
    expect(commands).toEqual([]);
    expect(state.pending).toEqual(["<Space>", "f"]);
  });

  it("should open the finder when the whole leader sequence is typed", () => {
    const { commands } = run("<Space>fe");
    expect(commands).toEqual([{ kind: "action", action: "finder", count: 0 }]);
  });

  it("should drop the sequence and hand the key back when nothing matches", () => {
    const { commands, handled, state } = run("<Space>z");
    expect(commands).toEqual([]);
    expect(state.pending).toEqual([]);
    // Not handled: an engine that swallows keys it has no binding for is one
    // that breaks Ctrl+R.
    expect(handled.at(-1)).toBe(false);
  });

  it("should resolve the shorter binding when the timeout fires on an ambiguous prefix", () => {
    const ambiguous = createRegistry([
      defineBinding({ id: "short", keys: "z", command: { kind: "action", action: "info" }, scopes: ["gallery"], title: "z", group: "t" }),
      defineBinding({ id: "long", keys: "zz", command: { kind: "action", action: "close" }, scopes: ["gallery"], title: "zz", group: "t" }),
    ]);

    const held = run("z", { registry: ambiguous, chain: ["gallery"] });
    expect(held.commands).toEqual([]);

    const timedOut = run(`z${TIMEOUT}`, { registry: ambiguous, chain: ["gallery"] });
    expect(timedOut.commands).toEqual([{ kind: "action", action: "info", count: 0 }]);
  });

  it("should ask the caller to start a timer when an exact match is also a prefix", () => {
    const ambiguous = createRegistry([
      defineBinding({ id: "short", keys: "z", command: { kind: "action", action: "info" }, scopes: ["gallery"], title: "z", group: "t" }),
      defineBinding({ id: "long", keys: "zz", command: { kind: "action", action: "close" }, scopes: ["gallery"], title: "zz", group: "t" }),
    ]);
    const bindings = ambiguous.resolve(["gallery"]);
    const result = step(initialState, { type: "key", key: "z" }, bindings);
    expect(result.awaitTimeout).toBeGreaterThan(0);
  });

  it("should prefer the longer binding when the second key arrives before the timeout", () => {
    const ambiguous = createRegistry([
      defineBinding({ id: "short", keys: "z", command: { kind: "action", action: "info" }, scopes: ["gallery"], title: "z", group: "t" }),
      defineBinding({ id: "long", keys: "zz", command: { kind: "action", action: "close" }, scopes: ["gallery"], title: "zz", group: "t" }),
    ]);
    const { commands } = run("zz", { registry: ambiguous, chain: ["gallery"] });
    expect(commands).toEqual([{ kind: "action", action: "close", count: 0 }]);
  });
});

describe("scopes", () => {
  it("should not reach a gallery binding when only the lightbox chain is active", () => {
    const { commands } = run("x", { chain: ["lightbox", "global"] });
    expect(commands).toEqual([]);
  });

  it("should reach the lightbox's own bindings when the lightbox is on top", () => {
    const { commands } = run("q", { chain: ["lightbox", "global"] });
    expect(commands).toEqual([{ kind: "action", action: "close", count: 0 }]);
  });

  it("should still archive from the lightbox, because the grammar is not per-component", () => {
    const { commands } = run("dd", { chain: ["lightbox", "global"] });
    expect(commands).toEqual([
      { kind: "operator", operator: "archive", target: { kind: "line", count: 0 } },
    ]);
  });

  it("should ignore every binding when an exclusive overlay owns the keyboard", () => {
    const { commands, handled } = run("dd", { chain: ["overlay"] });
    expect(commands).toEqual([]);
    expect(handled).toEqual([false, false]);
  });
});

describe("pendingLabel", () => {
  it("should show what has been typed so far while a command is unfinished", () => {
    expect(pendingLabel(run("2d3").state)).toBe("2d3");
    expect(pendingLabel(run("<Space>f").state)).toBe("<Space>f");
    expect(pendingLabel(initialState)).toBe("");
  });
});

import { describe, expect, it } from "vitest";
import { normalizeKey, parseSequence, writeSequence } from "./keys";
import { isTextEntryTarget } from "./textEntry";

describe("normalizeKey", () => {
  it("should return the bare character when no modifier is held", () => {
    expect(normalizeKey({ key: "j" })).toBe("j");
    expect(normalizeKey({ key: "$" })).toBe("$");
    expect(normalizeKey({ key: ":" })).toBe(":");
    expect(normalizeKey({ key: "?" })).toBe("?");
  });

  it("should keep the shifted character as itself when shift produced it", () => {
    // The browser already reports "V"; spelling it <S-v> as well would give one
    // keystroke two names and let the table disagree with itself.
    expect(normalizeKey({ key: "V", shiftKey: true })).toBe("V");
    expect(normalizeKey({ key: "G", shiftKey: true })).toBe("G");
  });

  it("should spell out control and meta chords in vim's form", () => {
    expect(normalizeKey({ key: "d", ctrlKey: true })).toBe("<C-d>");
    expect(normalizeKey({ key: "a", metaKey: true })).toBe("<D-a>");
    expect(normalizeKey({ key: "d", ctrlKey: true, shiftKey: true })).toBe("<C-S-d>");
  });

  it("should lowercase the letter in a control chord when shift is also held", () => {
    expect(normalizeKey({ key: "A", ctrlKey: true, shiftKey: true })).toBe("<C-S-a>");
  });

  it("should name the special keys", () => {
    expect(normalizeKey({ key: "Escape" })).toBe("<Esc>");
    expect(normalizeKey({ key: "Enter" })).toBe("<CR>");
    expect(normalizeKey({ key: " " })).toBe("<Space>");
    expect(normalizeKey({ key: "ArrowDown" })).toBe("<Down>");
    expect(normalizeKey({ key: "F10" })).toBe("<F10>");
    expect(normalizeKey({ key: "ContextMenu" })).toBe("<Menu>");
    expect(normalizeKey({ key: "Tab", shiftKey: true })).toBe("<S-Tab>");
    expect(normalizeKey({ key: "ArrowDown", ctrlKey: true })).toBe("<C-Down>");
  });

  it("should return null when the key is only a modifier going down", () => {
    // Dispatching on these would fire every <C-...> binding the moment Control
    // was pressed, before the owner had chosen anything.
    for (const key of ["Shift", "Control", "Alt", "Meta", "CapsLock"]) {
      expect(normalizeKey({ key })).toBeNull();
    }
  });

  it("should return null when the key name is not one this table knows", () => {
    expect(normalizeKey({ key: "Unidentified" })).toBeNull();
    expect(normalizeKey({ key: "BrightnessUp" })).toBeNull();
    expect(normalizeKey({ key: "" })).toBeNull();
  });
});

describe("parseSequence", () => {
  it("should split a written binding into tokens", () => {
    expect(parseSequence("gg")).toEqual(["g", "g"]);
    expect(parseSequence("<Space>fe")).toEqual(["<Space>", "f", "e"]);
    expect(parseSequence("<C-d>")).toEqual(["<C-d>"]);
    expect(parseSequence("]f")).toEqual(["]", "f"]);
  });

  it("should round-trip through writeSequence", () => {
    for (const written of ["gg", "<Space>fe", "<C-d>", "cd", "<Esc>"]) {
      expect(writeSequence(parseSequence(written))).toBe(written);
    }
  });

  it("should throw when an angle-bracket token is never closed", () => {
    expect(() => parseSequence("<Space")).toThrow(/unterminated/);
  });
});

describe("isTextEntryTarget", () => {
  function element(html: string): Element {
    const host = document.createElement("div");
    host.innerHTML = html;
    return host.firstElementChild!;
  }

  it("should suspend the engine when focus is in a text field", () => {
    expect(isTextEntryTarget(element("<input type='text'>"))).toBe(true);
    expect(isTextEntryTarget(element("<input>"))).toBe(true);
    expect(isTextEntryTarget(element("<textarea></textarea>"))).toBe(true);
    expect(isTextEntryTarget(element("<select></select>"))).toBe(true);
  });

  it("should NOT suspend the engine when focus is on a checkbox", () => {
    // Focus sits on the grid's own selection checkboxes constantly; suspending
    // there would kill every binding the moment a photo was ticked.
    expect(isTextEntryTarget(element("<input type='checkbox'>"))).toBe(false);
    expect(isTextEntryTarget(element("<input type='radio'>"))).toBe(false);
    expect(isTextEntryTarget(element("<input type='range'>"))).toBe(false);
    expect(isTextEntryTarget(element("<button></button>"))).toBe(false);
    expect(isTextEntryTarget(element("<div tabindex='0'></div>"))).toBe(false);
  });

  it("should suspend the engine when the target is inside a keymap-ignore subtree", () => {
    const host = document.createElement("div");
    host.setAttribute("data-keymap-ignore", "");
    const inner = document.createElement("span");
    host.appendChild(inner);
    document.body.appendChild(host);

    expect(isTextEntryTarget(inner)).toBe(true);
    host.remove();
  });

  it("should return false when the target is not an element at all", () => {
    expect(isTextEntryTarget(null)).toBe(false);
    expect(isTextEntryTarget(window)).toBe(false);
  });
});

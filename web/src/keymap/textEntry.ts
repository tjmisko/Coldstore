/** The one "am I typing?" predicate.
 *
 *  Before this there was exactly one such guard, inline in Gallery.tsx:47-55,
 *  so Escape and Ctrl+A were stolen from the budget editor and from every other
 *  input the app grew after it. With a grammar it stops being a nicety: `20 GB`
 *  typed into a budget field is `2`, `0`, `G`, `B` to the machine, and `G` is a
 *  motion to the end of the folder. */

/** <input> types that are controls, not text. The grid's own selection
 *  checkboxes are among them, which is the case that matters: focus sits on a
 *  checkbox constantly, and suspending the engine there would kill every
 *  binding the moment the owner ticked a photo. */
const CONTROL_INPUT_TYPES = new Set([
  "checkbox",
  "radio",
  "button",
  "submit",
  "reset",
  "file",
  "range",
  "color",
  "image",
]);

interface TargetLike {
  tagName?: unknown;
  isContentEditable?: unknown;
  type?: unknown;
  closest?: unknown;
}

/** isTextEntryTarget is duck-typed rather than `instanceof HTMLElement`.
 *  A keydown target can be the window, a document, or an element from another
 *  realm, and an instanceof that quietly returns false for the last of those
 *  would hand the engine keystrokes that belong to a text field. */
export function isTextEntryTarget(target: EventTarget | null | undefined): boolean {
  if (!target) return false;
  const el = target as TargetLike;
  if (typeof el.tagName !== "string") return false;

  // The explicit escape hatch, checked first: a widget that must own its keys
  // marks its subtree and the engine stays out of all of it.
  if (typeof el.closest === "function") {
    const closest = el.closest as (selector: string) => unknown;
    if (closest.call(el, "[data-keymap-ignore]")) return true;
  }

  const tag = el.tagName.toUpperCase();
  if (tag === "TEXTAREA" || tag === "SELECT") return true;
  if (el.isContentEditable === true) return true;
  if (tag === "INPUT") {
    const type = typeof el.type === "string" ? el.type.toLowerCase() : "text";
    return !CONTROL_INPUT_TYPES.has(type);
  }
  return false;
}

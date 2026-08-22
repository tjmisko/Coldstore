import { useEffect, useRef, type RefObject } from "react";

const FOCUSABLE = [
  "a[href]",
  "button:not([disabled])",
  "input:not([disabled])",
  "select:not([disabled])",
  "textarea:not([disabled])",
  '[tabindex]:not([tabindex="-1"])',
].join(",");

/** useFocusTrap keeps Tab inside an overlay and puts focus back where it was.
 *
 *  The restore half is the one that matters for a keyboard-only control plane:
 *  `:` then Escape has to put the cursor back on the tile it was on, or every
 *  cancelled command costs the owner a `gg` and a count to find their place
 *  again.
 *
 *  Attach the returned ref to the overlay's container. No state is written, so
 *  the whole thing is one effect and `react-hooks/set-state-in-effect` has
 *  nothing to say about it. */
export function useFocusTrap(active: boolean): RefObject<HTMLElement | null> {
  const containerRef = useRef<HTMLElement | null>(null);

  useEffect(() => {
    if (!active) return;
    const container = containerRef.current;
    if (!container) return;

    const previous = document.activeElement as HTMLElement | null;
    // Deliberately NOT filtered on offsetParent: jsdom reports null for every
    // element, so a visibility filter written that way traps focus on nothing
    // in the test tier and passes in the browser.
    const focusables = () =>
      Array.from(container.querySelectorAll<HTMLElement>(FOCUSABLE)).filter(
        (el) => !el.hasAttribute("hidden") && el.getAttribute("aria-hidden") !== "true",
      );

    const first = focusables()[0];
    (first ?? container).focus();

    const onKeyDown = (event: KeyboardEvent) => {
      if (event.key !== "Tab") return;
      const list = focusables();
      if (list.length === 0) {
        event.preventDefault();
        container.focus();
        return;
      }
      const head = list[0];
      const tail = list[list.length - 1];
      const inside = container.contains(document.activeElement);
      if (event.shiftKey && (document.activeElement === head || !inside)) {
        event.preventDefault();
        tail.focus();
      } else if (!event.shiftKey && (document.activeElement === tail || !inside)) {
        event.preventDefault();
        head.focus();
      }
    };

    // Capture, so a component that stops Tab from bubbling cannot escape the
    // trap by accident.
    document.addEventListener("keydown", onKeyDown, true);
    return () => {
      document.removeEventListener("keydown", onKeyDown, true);
      if (previous && typeof previous.focus === "function") previous.focus();
    };
  }, [active]);

  return containerRef;
}

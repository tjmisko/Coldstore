import { createPortal } from "react-dom";
import type { ReactNode } from "react";

/** Everything that has to escape its ancestors renders through here.
 *
 *  There was no portal anywhere in this app: every overlay was an inline
 *  `fixed inset-0` div, which works until it is inside `overflow-hidden`
 *  (`App.tsx` is) or inside an element that has created its own stacking
 *  context (a `transition-transform` sidebar does exactly that). Then a
 *  z-index of 70 loses to a sibling of 40 and there is nothing in the
 *  overlay's own CSS to explain why.
 *
 *  It portals to `document.body` rather than to a host div it creates: a host
 *  needs an effect to append it and a state write to render into it, and
 *  `react-hooks/set-state-in-effect` is not negotiable in this codebase. */
export function Portal({ children }: { children: ReactNode }) {
  return createPortal(children, document.body);
}

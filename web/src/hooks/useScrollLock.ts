import { useEffect } from "react";

/** Nesting counter, module scope on purpose: a dialog opened from the lightbox
 *  must not unlock the page when only the dialog closes. */
let locks = 0;
let restoreTo = "";

/** useScrollLock stops the page behind an overlay from scrolling.
 *
 *  Without it, `<C-d>` typed into the finder scrolls the gallery underneath and
 *  the owner closes the finder somewhere they never navigated to -- which on a
 *  list paged by an IntersectionObserver also fires off a page load. */
export function useScrollLock(active: boolean): void {
  useEffect(() => {
    if (!active) return;
    if (locks === 0) {
      restoreTo = document.body.style.overflow;
      document.body.style.overflow = "hidden";
    }
    locks += 1;
    return () => {
      locks -= 1;
      if (locks === 0) document.body.style.overflow = restoreTo;
    };
  }, [active]);
}

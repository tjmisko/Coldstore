/** The one z-index ladder.
 *
 *  It used to be hand-rolled across six files -- sidebar 40, backdrop 30,
 *  hamburger 50, lightbox 50, toasts 60, dialog 70 -- and the hamburger and the
 *  lightbox were already a tie, decided by document order rather than by
 *  anyone's intent. A ladder can only be read at a glance if it is written down
 *  in one place.
 *
 *  Applied as an inline `style`, not as a Tailwind class: the design refresh
 *  landing in parallel is going to move every colour in the app, and a numeric
 *  constant is one line to change rather than a grep for `z-[70]`.
 */
export const Z = {
  /** The mobile scrim behind the sidebar. */
  sidebarBackdrop: 30,
  sidebar: 40,
  selectionBar: 40,
  /** Below the lightbox, so a full-screen photo is not overlaid by a
   *  hamburger. This was the tie. */
  sidebarToggle: 45,
  lightbox: 50,
  /** Palette, finder, cheatsheet. */
  overlay: 70,
  contextMenu: 75,
  dialog: 80,
  /** Above every overlay: a keyboard action reports its failure as a toast, and
   *  a toast the palette is covering is a failure the owner never sees. */
  toast: 95,
  keymapStatus: 96,
} as const;

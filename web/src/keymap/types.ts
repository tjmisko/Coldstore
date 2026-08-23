/** The vocabulary the whole keymap layer is written in. One module so that
 *  motions.ts, registry.ts, machine.ts and bindings.ts cannot drift and cannot
 *  import each other in a cycle. */

export type MotionId =
  | "left"
  | "right"
  | "up"
  | "down"
  | "row-start"
  | "row-end"
  | "first"
  | "last"
  | "half-page-down"
  | "half-page-up"
  | "page-down"
  | "page-up"
  | "next-file"
  | "previous-file"
  | "capture-day-next"
  | "capture-day-previous"
  | "tag-next"
  | "tag-previous"
  | "untagged-next"
  | "untagged-previous"
  | "pair-next"
  | "pair-previous";

/** Operators take a range and produce ONE request. `d` is archive because
 *  archiving is the delete here -- it removes the file from every device
 *  Syncthing talks to -- and because the muscle memory is real. `u` is unpin,
 *  not undo: there is no undo model in this app, and a `u` that sometimes means
 *  one and sometimes the other is worse than keeping tag undo on `U`. */
export type OperatorId =
  | "archive"
  | "restore"
  | "pin"
  | "unpin"
  | "download"
  | "tag"
  | "untag";

export type ActionId =
  | "open"
  | "toggle-select"
  | "select-all-loaded"
  | "select-all-folder"
  | "clear-selection"
  | "cancel"
  | "palette"
  | "palette-folder"
  | "palette-filter"
  | "cheatsheet"
  | "finder"
  | "context-menu"
  | "view-gallery"
  | "view-files"
  | "view-settings"
  | "next-view"
  | "prev-view"
  | "next-folder"
  | "prev-folder"
  | "close"
  | "info"
  | "invert-pdf"
  | "previous-item"
  | "next-item"
  | "scroll-document-up"
  | "scroll-document-down"
  | "previous-document-page"
  | "next-document-page"
  | "edit-description"
  | "repeat-tags"
  | "tag-undo"
  | "tag-redo"
  | "tag-mode"
  | "tag-current"
  | "untag-current"
  | "toggle-tag-carry"
  | "accept-tag-carry"
  | "quick-tag-1"
  | "quick-tag-2"
  | "quick-tag-3"
  | "quick-tag-4"
  | "quick-tag-5"
  | "quick-tag-6"
  | "quick-tag-7"
  | "quick-tag-8"
  | "quick-tag-9"
  | "rescan"
  | "resolve-device"
  | "resolve-archive";

export type Mode = "normal" | "visual" | "visual-line";

export type Scope =
  | "global"
  | "gallery"
  | "browser"
  | "lightbox"
  | "pdf-reader"
  | "epub-reader"
  | "tagging"
  | "overlay"
  | "description"
  | "tag-prompt";

/** What an operator is waiting for.
 *
 *  "line" is the doubled form (`dd`, `3dd`) and means the cursor ITEM, not the
 *  grid row it happens to be drawn in: which photos share a row depends on the
 *  browser window width, so a linewise `dd` would archive different files at
 *  1440px and at 390px. Visual-line does snap to rows -- there the owner sees
 *  the highlight before anything happens. */
export type OperatorTarget =
  | { kind: "motion"; motion: MotionId; count: number }
  | { kind: "line"; count: number }
  | { kind: "selection" };

/** What the machine emits. Never a callback: a command is data, so a test can
 *  assert on the whole sequence a key run produced. */
export type Command =
  | { kind: "motion"; motion: MotionId; count: number; extend: boolean }
  | { kind: "operator"; operator: OperatorId; target: OperatorTarget }
  | { kind: "action"; action: ActionId; count: number }
  | { kind: "mode"; mode: Mode; from: Mode };

/** A count of 0 means the owner typed no count at all, which is not the same as
 *  1: `gg` goes to the first item and `1gg` goes to item 1 -- the same place --
 *  but `G` goes to the last item while `1G` goes to the first. */
export const NO_COUNT = 0;

export type BindingCommand =
  | { kind: "motion"; motion: MotionId }
  | { kind: "operator"; operator: OperatorId }
  | { kind: "action"; action: ActionId }
  | { kind: "visual"; mode: "visual" | "visual-line" };

export interface Binding {
  /** Stable across renames of the written key form, so tests and the cheatsheet
   *  can refer to a binding without hard-coding its keys. */
  id: string;
  /** The written form: "gg", "<Space>", "<C-d>". */
  keys: string;
  sequence: string[];
  command: BindingCommand;
  scopes: Scope[];
  /** One line, in the imperative, for the `?` overlay. */
  title: string;
  group: string;
  /** Hidden from the cheatsheet: aliases, and keys that exist only so an arrow
   *  key does what the vim key does. */
  alias?: boolean;
}

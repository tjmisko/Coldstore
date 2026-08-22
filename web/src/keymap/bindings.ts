import { createRegistry, defineBinding, type BindingSpec, type KeymapRegistry } from "./registry";
import type { Binding, Scope } from "./types";

/** Scopes that stop the chain. While one of these is on top of the stack, `d`
 *  types a `d` and archives nothing. */
export const EXCLUSIVE_SCOPES: Scope[] = ["overlay", "description", "tag-prompt"];

const GRID: Scope[] = ["gallery", "browser"];
const NAVIGABLE: Scope[] = ["gallery", "browser", "lightbox"];

const GROUP_MOTION = "Motion";
const GROUP_SELECT = "Selection";
const GROUP_FILES = "Files";
const GROUP_NAV = "Navigation";
const GROUP_OVERLAY = "Overlays";
const GROUP_CONFLICT = "Conflicts";
const GROUP_TAGS = "Tags";

const SPECS: BindingSpec[] = [
  // -- motions ------------------------------------------------------------
  { id: "motion.left", keys: "h", command: { kind: "motion", motion: "left" }, scopes: NAVIGABLE, title: "Left", group: GROUP_MOTION },
  { id: "motion.down", keys: "j", command: { kind: "motion", motion: "down" }, scopes: NAVIGABLE, title: "Down", group: GROUP_MOTION },
  { id: "motion.up", keys: "k", command: { kind: "motion", motion: "up" }, scopes: NAVIGABLE, title: "Up", group: GROUP_MOTION },
  { id: "motion.right", keys: "l", command: { kind: "motion", motion: "right" }, scopes: NAVIGABLE, title: "Right", group: GROUP_MOTION },
  { id: "motion.rowStart", keys: "0", command: { kind: "motion", motion: "row-start" }, scopes: NAVIGABLE, title: "First in row", group: GROUP_MOTION },
  { id: "motion.rowEnd", keys: "$", command: { kind: "motion", motion: "row-end" }, scopes: NAVIGABLE, title: "Last in row", group: GROUP_MOTION },
  { id: "motion.first", keys: "gg", command: { kind: "motion", motion: "first" }, scopes: NAVIGABLE, title: "First file (12gg for file 12)", group: GROUP_MOTION },
  { id: "motion.last", keys: "G", command: { kind: "motion", motion: "last" }, scopes: NAVIGABLE, title: "Last file (12G for file 12)", group: GROUP_MOTION },
  { id: "motion.halfDown", keys: "<C-d>", command: { kind: "motion", motion: "half-page-down" }, scopes: NAVIGABLE, title: "Half a screen down", group: GROUP_MOTION },
  { id: "motion.halfUp", keys: "<C-u>", command: { kind: "motion", motion: "half-page-up" }, scopes: NAVIGABLE, title: "Half a screen up", group: GROUP_MOTION },
  { id: "motion.pageDown", keys: "<C-f>", command: { kind: "motion", motion: "page-down" }, scopes: NAVIGABLE, title: "A screen down", group: GROUP_MOTION },
  { id: "motion.pageUp", keys: "<C-b>", command: { kind: "motion", motion: "page-up" }, scopes: NAVIGABLE, title: "A screen up", group: GROUP_MOTION },

  // Arrow keys and the navigation cluster, so the app is usable by someone who
  // has never touched vim. Aliases, so they do not double the cheatsheet.
  { id: "motion.left.arrow", keys: "<Left>", command: { kind: "motion", motion: "left" }, scopes: NAVIGABLE, title: "Left", group: GROUP_MOTION, alias: true },
  { id: "motion.down.arrow", keys: "<Down>", command: { kind: "motion", motion: "down" }, scopes: NAVIGABLE, title: "Down", group: GROUP_MOTION, alias: true },
  { id: "motion.up.arrow", keys: "<Up>", command: { kind: "motion", motion: "up" }, scopes: NAVIGABLE, title: "Up", group: GROUP_MOTION, alias: true },
  { id: "motion.right.arrow", keys: "<Right>", command: { kind: "motion", motion: "right" }, scopes: NAVIGABLE, title: "Right", group: GROUP_MOTION, alias: true },
  { id: "motion.pageDown.key", keys: "<PageDown>", command: { kind: "motion", motion: "page-down" }, scopes: NAVIGABLE, title: "A screen down", group: GROUP_MOTION, alias: true },
  { id: "motion.pageUp.key", keys: "<PageUp>", command: { kind: "motion", motion: "page-up" }, scopes: NAVIGABLE, title: "A screen up", group: GROUP_MOTION, alias: true },
  { id: "motion.first.home", keys: "<Home>", command: { kind: "motion", motion: "first" }, scopes: NAVIGABLE, title: "First file", group: GROUP_MOTION, alias: true },
  { id: "motion.last.end", keys: "<End>", command: { kind: "motion", motion: "last" }, scopes: NAVIGABLE, title: "Last file", group: GROUP_MOTION, alias: true },
  { id: "motion.captureDayPrevious", keys: "{", command: { kind: "motion", motion: "capture-day-previous" }, scopes: NAVIGABLE, title: "Previous capture day", group: GROUP_MOTION },
  { id: "motion.captureDayNext", keys: "}", command: { kind: "motion", motion: "capture-day-next" }, scopes: NAVIGABLE, title: "Next capture day", group: GROUP_MOTION },
  { id: "motion.tagNext", keys: "]t", command: { kind: "motion", motion: "tag-next" }, scopes: NAVIGABLE, title: "Next file sharing a tag", group: GROUP_MOTION },
  { id: "motion.tagPrevious", keys: "[t", command: { kind: "motion", motion: "tag-previous" }, scopes: NAVIGABLE, title: "Previous file sharing a tag", group: GROUP_MOTION },
  { id: "motion.untaggedNext", keys: "]u", command: { kind: "motion", motion: "untagged-next" }, scopes: NAVIGABLE, title: "Next untagged file", group: GROUP_MOTION },
  { id: "motion.untaggedPrevious", keys: "[u", command: { kind: "motion", motion: "untagged-previous" }, scopes: NAVIGABLE, title: "Previous untagged file", group: GROUP_MOTION },
  { id: "motion.pairNext", keys: "]p", command: { kind: "motion", motion: "pair-next" }, scopes: NAVIGABLE, title: "Next file group", group: GROUP_MOTION },
  { id: "motion.pairPrevious", keys: "[p", command: { kind: "motion", motion: "pair-previous" }, scopes: NAVIGABLE, title: "Previous file group", group: GROUP_MOTION },
  { id: "tagMode.next", keys: "j", command: { kind: "motion", motion: "next-file" }, scopes: ["tagging"], title: "Next file", group: GROUP_MOTION, alias: true },
  { id: "tagMode.previous", keys: "k", command: { kind: "motion", motion: "previous-file" }, scopes: ["tagging"], title: "Previous file", group: GROUP_MOTION, alias: true },
  { id: "tagMode.nextUntagged", keys: "J", command: { kind: "motion", motion: "untagged-next" }, scopes: ["tagging"], title: "Next untagged file", group: GROUP_MOTION, alias: true },
  { id: "tagMode.previousUntagged", keys: "K", command: { kind: "motion", motion: "untagged-previous" }, scopes: ["tagging"], title: "Previous untagged file", group: GROUP_MOTION, alias: true },

  // -- operators ----------------------------------------------------------
  { id: "op.archive", keys: "d", command: { kind: "operator", operator: "archive" }, scopes: NAVIGABLE, title: "Archive (dd, d3j, dG) - removes it from every synced device", group: GROUP_FILES },
  { id: "op.restore", keys: "r", command: { kind: "operator", operator: "restore" }, scopes: NAVIGABLE, title: "Restore (rr, r3j, rG)", group: GROUP_FILES },
  { id: "op.pin", keys: "p", command: { kind: "operator", operator: "pin" }, scopes: NAVIGABLE, title: "Pin (pp, p3j) - never archive these", group: GROUP_FILES },
  { id: "op.unpin", keys: "u", command: { kind: "operator", operator: "unpin" }, scopes: NAVIGABLE, title: "Unpin (uu, u3j)", group: GROUP_FILES },
  { id: "op.download", keys: "y", command: { kind: "operator", operator: "download" }, scopes: NAVIGABLE, title: "Download (yy, y3j)", group: GROUP_FILES },
  { id: "op.tag", keys: "t", command: { kind: "operator", operator: "tag" }, scopes: NAVIGABLE, title: "Tag (tt, t3j, tG)", group: GROUP_TAGS },
  { id: "op.untag", keys: "T", command: { kind: "operator", operator: "untag" }, scopes: NAVIGABLE, title: "Remove tags (TT, T3j, TG)", group: GROUP_TAGS },

  // -- tagging ------------------------------------------------------------
  { id: "tagMode.tagCurrent", keys: "t", command: { kind: "action", action: "tag-current" }, scopes: ["tagging"], title: "Tag current file", group: GROUP_TAGS, alias: true },
  { id: "tagMode.untagCurrent", keys: "T", command: { kind: "action", action: "untag-current" }, scopes: ["tagging"], title: "Remove tags from current file", group: GROUP_TAGS, alias: true },
  { id: "tag.repeat", keys: ".", command: { kind: "action", action: "repeat-tags" }, scopes: NAVIGABLE, title: "Repeat the last tag operation", group: GROUP_TAGS },
  { id: "tag.undo", keys: "U", command: { kind: "action", action: "tag-undo" }, scopes: NAVIGABLE, title: "Undo the last tag write", group: GROUP_TAGS },
  { id: "tag.redo", keys: "<C-r>", command: { kind: "action", action: "tag-redo" }, scopes: NAVIGABLE, title: "Redo the last undone tag write", group: GROUP_TAGS },
  { id: "tag.mode", keys: "<Space>e", command: { kind: "action", action: "tag-mode" }, scopes: GRID, title: "Enter Tag mode", group: GROUP_TAGS },
  { id: "tag.carry", keys: ",", command: { kind: "action", action: "toggle-tag-carry" }, scopes: ["tagging"], title: "Toggle tag carry-forward", group: GROUP_TAGS },
  { id: "tag.carryAccept", keys: "<CR>", command: { kind: "action", action: "accept-tag-carry" }, scopes: ["tagging"], title: "Accept carried tags", group: GROUP_TAGS, alias: true },
  { id: "tag.quick1", keys: "<Space>1", command: { kind: "action", action: "quick-tag-1" }, scopes: NAVIGABLE, title: "Apply quick tag 1", group: GROUP_TAGS },
  { id: "tag.quick2", keys: "<Space>2", command: { kind: "action", action: "quick-tag-2" }, scopes: NAVIGABLE, title: "Apply quick tag 2", group: GROUP_TAGS },
  { id: "tag.quick3", keys: "<Space>3", command: { kind: "action", action: "quick-tag-3" }, scopes: NAVIGABLE, title: "Apply quick tag 3", group: GROUP_TAGS },
  { id: "tag.quick4", keys: "<Space>4", command: { kind: "action", action: "quick-tag-4" }, scopes: NAVIGABLE, title: "Apply quick tag 4", group: GROUP_TAGS },
  { id: "tag.quick5", keys: "<Space>5", command: { kind: "action", action: "quick-tag-5" }, scopes: NAVIGABLE, title: "Apply quick tag 5", group: GROUP_TAGS },
  { id: "tag.quick6", keys: "<Space>6", command: { kind: "action", action: "quick-tag-6" }, scopes: NAVIGABLE, title: "Apply quick tag 6", group: GROUP_TAGS },
  { id: "tag.quick7", keys: "<Space>7", command: { kind: "action", action: "quick-tag-7" }, scopes: NAVIGABLE, title: "Apply quick tag 7", group: GROUP_TAGS },
  { id: "tag.quick8", keys: "<Space>8", command: { kind: "action", action: "quick-tag-8" }, scopes: NAVIGABLE, title: "Apply quick tag 8", group: GROUP_TAGS },
  { id: "tag.quick9", keys: "<Space>9", command: { kind: "action", action: "quick-tag-9" }, scopes: NAVIGABLE, title: "Apply quick tag 9", group: GROUP_TAGS },

  // -- selection ----------------------------------------------------------
  { id: "select.toggle", keys: "x", command: { kind: "action", action: "toggle-select" }, scopes: GRID, title: "Select or deselect this file", group: GROUP_SELECT },
  { id: "select.visual", keys: "v", command: { kind: "visual", mode: "visual" }, scopes: GRID, title: "Visual select; motions extend it", group: GROUP_SELECT },
  { id: "select.visualLine", keys: "V", command: { kind: "visual", mode: "visual-line" }, scopes: GRID, title: "Visual select by whole rows (ggVG selects everything)", group: GROUP_SELECT },
  { id: "select.allLoaded", keys: "<C-a>", command: { kind: "action", action: "select-all-loaded" }, scopes: GRID, title: "Select every file on screen", group: GROUP_SELECT },
  { id: "select.allLoaded.cmd", keys: "<D-a>", command: { kind: "action", action: "select-all-loaded" }, scopes: GRID, title: "Select every file on screen", group: GROUP_SELECT, alias: true },
  { id: "select.allFolder", keys: "<Space>a", command: { kind: "action", action: "select-all-folder" }, scopes: GRID, title: "Select every file in the folder that matches the filter", group: GROUP_SELECT },
  { id: "select.clear", keys: "<Space>c", command: { kind: "action", action: "clear-selection" }, scopes: GRID, title: "Clear the selection", group: GROUP_SELECT },

  // -- opening ------------------------------------------------------------
  { id: "file.open", keys: "<CR>", command: { kind: "action", action: "open" }, scopes: GRID, title: "Open in the lightbox", group: GROUP_FILES },
  { id: "file.open.o", keys: "o", command: { kind: "action", action: "open" }, scopes: GRID, title: "Open in the lightbox", group: GROUP_FILES, alias: true },
  { id: "file.menu", keys: "<Menu>", command: { kind: "action", action: "context-menu" }, scopes: GRID, title: "Context menu for this file", group: GROUP_FILES, alias: true },
  { id: "file.menu.leader", keys: "<Space>m", command: { kind: "action", action: "context-menu" }, scopes: GRID, title: "Context menu for this file", group: GROUP_FILES },
  { id: "file.description", keys: "i", command: { kind: "action", action: "edit-description" }, scopes: NAVIGABLE, title: "Edit description", group: GROUP_FILES },

  // -- lightbox -----------------------------------------------------------
  { id: "lightbox.close", keys: "q", command: { kind: "action", action: "close" }, scopes: ["lightbox"], title: "Close the lightbox", group: GROUP_OVERLAY },
  { id: "lightbox.info", keys: "<Space>i", command: { kind: "action", action: "info" }, scopes: ["lightbox"], title: "Toggle the metadata panel", group: GROUP_OVERLAY },

  // -- conflicts ----------------------------------------------------------
  { id: "conflict.device", keys: "cd", command: { kind: "action", action: "resolve-device" }, scopes: NAVIGABLE, title: "Resolve: keep the copy on your device", group: GROUP_CONFLICT },
  { id: "conflict.archive", keys: "ca", command: { kind: "action", action: "resolve-archive" }, scopes: NAVIGABLE, title: "Resolve: keep the archived copy", group: GROUP_CONFLICT },

  // -- navigation ---------------------------------------------------------
  { id: "nav.gallery", keys: "<Space>g", command: { kind: "action", action: "view-gallery" }, scopes: ["global"], title: "Gallery view", group: GROUP_NAV },
  { id: "nav.files", keys: "<Space>b", command: { kind: "action", action: "view-files" }, scopes: ["global"], title: "File browser view", group: GROUP_NAV },
  { id: "nav.settings", keys: "<Space>s", command: { kind: "action", action: "view-settings" }, scopes: ["global"], title: "Settings view", group: GROUP_NAV },
  { id: "nav.nextView", keys: "gt", command: { kind: "action", action: "next-view" }, scopes: ["global"], title: "Next view", group: GROUP_NAV },
  { id: "nav.prevView", keys: "gT", command: { kind: "action", action: "prev-view" }, scopes: ["global"], title: "Previous view", group: GROUP_NAV },
  { id: "nav.nextFolder", keys: "]f", command: { kind: "action", action: "next-folder" }, scopes: ["global"], title: "Next folder", group: GROUP_NAV },
  { id: "nav.prevFolder", keys: "[f", command: { kind: "action", action: "prev-folder" }, scopes: ["global"], title: "Previous folder", group: GROUP_NAV },
  { id: "nav.folderPalette", keys: "<Space>o", command: { kind: "action", action: "palette-folder" }, scopes: ["global"], title: "Switch folder", group: GROUP_NAV },
  { id: "nav.filterPalette", keys: "<Space>t", command: { kind: "action", action: "palette-filter" }, scopes: ["global"], title: "Switch filter", group: GROUP_NAV },
  { id: "nav.rescan", keys: "<Space>R", command: { kind: "action", action: "rescan" }, scopes: ["global"], title: "Rescan this folder", group: GROUP_NAV },

  // -- overlays -----------------------------------------------------------
  { id: "overlay.palette", keys: ":", command: { kind: "action", action: "palette" }, scopes: ["global"], title: "Command palette", group: GROUP_OVERLAY },
  { id: "overlay.finder", keys: "<F10>", command: { kind: "action", action: "finder" }, scopes: ["global"], title: "Fuzzy file finder", group: GROUP_OVERLAY },
  { id: "overlay.finder.leader", keys: "<Space>fe", command: { kind: "action", action: "finder" }, scopes: ["global"], title: "Fuzzy file finder", group: GROUP_OVERLAY },
  { id: "overlay.help", keys: "?", command: { kind: "action", action: "cheatsheet" }, scopes: ["global"], title: "This cheatsheet", group: GROUP_OVERLAY },
  { id: "overlay.help.leader", keys: "<Space>?", command: { kind: "action", action: "cheatsheet" }, scopes: ["global"], title: "This cheatsheet", group: GROUP_OVERLAY, alias: true },

  // Escape is intercepted by the machine before the trie -- a cancel that can
  // itself get stuck pending is not a cancel -- but it lives in the table so
  // the generated cheatsheet documents it.
  { id: "overlay.cancel", keys: "<Esc>", command: { kind: "action", action: "cancel" }, scopes: ["global"], title: "Cancel: drop a half-typed command, clear the selection, close the lightbox", group: GROUP_OVERLAY },
];

export const BINDINGS: Binding[] = SPECS.map(defineBinding);

export const registry: KeymapRegistry = createRegistry(BINDINGS);

/** The cheatsheet's source of truth, grouped, aliases dropped. Generated from
 *  the same array the engine dispatches on, so it cannot describe a binding
 *  that does not exist. */
export function cheatsheetGroups(): { group: string; bindings: Binding[] }[] {
  const order = [GROUP_MOTION, GROUP_SELECT, GROUP_FILES, GROUP_TAGS, GROUP_CONFLICT, GROUP_NAV, GROUP_OVERLAY];
  const byGroup = new Map<string, Binding[]>();
  for (const binding of BINDINGS) {
    if (binding.alias) continue;
    const list = byGroup.get(binding.group) ?? [];
    list.push(binding);
    byGroup.set(binding.group, list);
  }
  return order
    .filter((group) => byGroup.has(group))
    .map((group) => ({ group, bindings: byGroup.get(group) ?? [] }));
}

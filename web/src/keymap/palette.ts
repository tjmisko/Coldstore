import { rankStrings } from "./fuzzy";
import type { OperatorId } from "./types";

/** The ex-line's command table, its parser and its completion. Pure, like the
 *  rest of this directory: the palette component decides what a command does to
 *  the app, and this decides what the owner typed. */

export type ArgumentKind = "folder" | "filter" | "view" | "size" | "minutes" | "text" | "none";

export interface PaletteCommand {
  name: string;
  aliases?: string[];
  summary: string;
  argument: ArgumentKind;
  argumentLabel?: string;
  group: string;
}

const NAV = "Navigate";
const FILES = "Files";
const CONFIG = "Settings";
const META = "Vim";

export const PALETTE_COMMANDS: PaletteCommand[] = [
  { name: "folder", summary: "Switch to a folder", argument: "folder", argumentLabel: "label", group: NAV },
  { name: "filter", summary: "Apply a filter", argument: "filter", argumentLabel: "name", group: NAV },
  { name: "view", summary: "Switch view", argument: "view", argumentLabel: "gallery | files | settings", group: NAV },
  { name: "gallery", summary: "Gallery view", argument: "none", group: NAV },
  { name: "files", summary: "File browser view", argument: "none", group: NAV },
  { name: "settings", summary: "Settings view", argument: "none", group: NAV },
  { name: "find", aliases: ["fe"], summary: "Fuzzy file finder", argument: "text", argumentLabel: "query", group: NAV },

  { name: "archive", summary: "Archive the selection, or the file under the cursor", argument: "none", group: FILES },
  { name: "restore", summary: "Restore the selection, or the file under the cursor", argument: "none", group: FILES },
  { name: "pin", summary: "Pin the selection, or the file under the cursor", argument: "none", group: FILES },
  { name: "unpin", summary: "Unpin the selection, or the file under the cursor", argument: "none", group: FILES },
  { name: "download", summary: "Download the selection, or the file under the cursor", argument: "none", group: FILES },
  { name: "open", summary: "Open the file under the cursor", argument: "none", group: FILES },
  { name: "selectall", summary: "Select every matching file in the folder", argument: "none", group: FILES },
  { name: "clear", aliases: ["noh"], summary: "Clear the selection", argument: "none", group: FILES },

  { name: "budget", summary: "Set this folder's budget", argument: "size", argumentLabel: "20GB", group: CONFIG },
  { name: "grace", summary: "Set this folder's grace period", argument: "minutes", argumentLabel: "minutes", group: CONFIG },
  { name: "enable", summary: "Enable coldstore for this folder", argument: "none", group: CONFIG },
  { name: "disable", summary: "Stop coldstore touching this folder", argument: "none", group: CONFIG },
  { name: "rescan", summary: "Rescan this folder now", argument: "none", group: CONFIG },

  { name: "q", aliases: ["quit", "close"], summary: "Close the lightbox, or this palette", argument: "none", group: META },
  { name: "h", aliases: ["help"], summary: "Open the keymap cheatsheet", argument: "none", group: META },
];

export interface ParsedCommandLine {
  name: string;
  args: string;
  /** True once a space has been typed after the name: the owner is on the
   *  argument now, and completing command names would be wrong. */
  onArgument: boolean;
}

export function parseCommandLine(line: string): ParsedCommandLine {
  const body = line.startsWith(":") ? line.slice(1) : line;
  const match = /^(\S*)(\s+)?([\s\S]*)$/.exec(body);
  if (!match) return { name: "", args: "", onArgument: false };
  return { name: match[1], args: match[3] ?? "", onArgument: match[2] !== undefined };
}

export function findCommand(name: string): PaletteCommand | null {
  const lower = name.toLowerCase();
  return (
    PALETTE_COMMANDS.find((c) => c.name === lower || c.aliases?.includes(lower)) ?? null
  );
}

export interface CompletionContext {
  folders: (string | { label: string; name: string })[];
  filters: { key: string; label: string }[];
  views: string[];
  /** Command names the owner has run before, most recent first. */
  recent: string[];
}

export interface Completion {
  /** The whole line to put in the input if this candidate is taken. */
  line: string;
  label: string;
  hint: string;
  /** True when taking this candidate finishes the command rather than moving
   *  on to its argument. */
  terminal: boolean;
}

/** completions ranks command names by fuzzy match, then argument values.
 *
 *  Recently used commands float to the top of an EMPTY query only. Once
 *  something has been typed, the match is the ranking: a palette that keeps
 *  reordering under the owner's fingers is one they stop trusting. */
export function completions(line: string, context: CompletionContext): Completion[] {
  const parsed = parseCommandLine(line);

  if (!parsed.onArgument) {
    const ranked = rankStrings(parsed.name, PALETTE_COMMANDS, (c) => c.name);
    const ordered =
      parsed.name === ""
        ? [
            ...context.recent
              .map((name) => ranked.find((c) => c.name === name))
              .filter((c): c is PaletteCommand => c !== undefined),
            ...ranked.filter((c) => !context.recent.includes(c.name)),
          ]
        : ranked;

    return ordered.map((command) => ({
      line: command.argument === "none" ? `:${command.name}` : `:${command.name} `,
      label: `:${command.name}`,
      hint: command.summary,
      terminal: command.argument === "none",
    }));
  }

  const command = findCommand(parsed.name);
  if (!command) return [];

  const values = argumentCandidates(command, context);
  if (values.length === 0) return [];

  return rankStrings(parsed.args, values, (v) => v.value).map((candidate) => ({
    line: `:${command.name} ${candidate.value}`,
    label: candidate.value,
    hint: candidate.hint,
    terminal: true,
  }));
}

interface Candidate {
  value: string;
  hint: string;
}

function argumentCandidates(command: PaletteCommand, context: CompletionContext): Candidate[] {
  switch (command.argument) {
    case "folder":
      return context.folders.map((folder) => {
        const choice = typeof folder === "string" ? { label: folder, name: folder } : folder;
        return { value: choice.name, hint: choice.name === choice.label ? "folder" : choice.label };
      });
    case "filter":
      return context.filters.map((f) => ({ value: f.key, hint: f.label }));
    case "view":
      return context.views.map((v) => ({ value: v, hint: "view" }));
    default:
      return [];
  }
}

// ---------------------------------------------------------------------------
// resolution

export type PaletteAction =
  | { kind: "folder"; label: string }
  | { kind: "filter"; key: string }
  | { kind: "view"; view: string }
  | { kind: "operator"; operator: OperatorId }
  | { kind: "open" }
  | { kind: "select-all" }
  | { kind: "clear" }
  | { kind: "budget"; text: string }
  | { kind: "grace"; minutes: number }
  | { kind: "enabled"; enabled: boolean }
  | { kind: "rescan" }
  | { kind: "finder"; query: string }
  | { kind: "quit" }
  | { kind: "help" };

export type PaletteResult =
  | { ok: true; action: PaletteAction }
  | { ok: false; error: string };

/** resolveCommandLine says what the typed line MEANS, or why it does not mean
 *  anything. It never performs anything: a pure resolver is the difference
 *  between testing the grammar and testing the app. */
export function resolveCommandLine(line: string, context: CompletionContext): PaletteResult {
  const parsed = parseCommandLine(line);
  if (parsed.name === "") return { ok: false, error: "type a command, or ? for the cheatsheet" };

  const command = findCommand(parsed.name);
  if (!command) return { ok: false, error: `not a command: :${parsed.name}` };

  const args = parsed.args.trim();
  if (command.argument !== "none" && args === "" && command.argument !== "text") {
    return { ok: false, error: `:${command.name} needs ${command.argumentLabel ?? "an argument"}` };
  }

  switch (command.name) {
    case "folder": {
      const choices = context.folders.map((folder) =>
        typeof folder === "string" ? { label: folder, name: folder } : folder,
      );
      const aliases = choices.flatMap((choice) => [
        { value: choice.name, choice },
        { value: choice.label, choice },
      ]);
      const lower = args.toLowerCase();
      const uniqueChoices = (matched: typeof aliases) => [
        ...new Map(matched.map((candidate) => [candidate.choice.label, candidate.choice])).values(),
      ];
      let found = uniqueChoices(aliases.filter((candidate) => candidate.value.toLowerCase() === lower));
      if (found.length === 0) {
        found = uniqueChoices(
          aliases.filter((candidate) => candidate.value.toLowerCase().startsWith(lower)),
        );
      }
      if (found.length === 0) {
        const ranked = rankStrings(args, aliases, (candidate) => candidate.value);
        found = uniqueChoices(ranked);
      }
      const choice = found.length === 1 ? found[0] : undefined;
      if (!choice) return { ok: false, error: `no folder matching "${args}"` };
      return { ok: true, action: { kind: "folder", label: choice.label } };
    }
    case "filter": {
      const key = matchOne(args, context.filters.map((f) => f.key));
      if (!key) return { ok: false, error: `no filter matching "${args}"` };
      return { ok: true, action: { kind: "filter", key } };
    }
    case "view": {
      const view = matchOne(args, context.views);
      if (!view) return { ok: false, error: `no view matching "${args}"` };
      return { ok: true, action: { kind: "view", view } };
    }
    case "gallery":
      return { ok: true, action: { kind: "view", view: "gallery" } };
    case "files":
      return { ok: true, action: { kind: "view", view: "files" } };
    case "settings":
      return { ok: true, action: { kind: "view", view: "config" } };
    case "find":
      return { ok: true, action: { kind: "finder", query: args } };

    case "archive":
    case "restore":
    case "pin":
    case "unpin":
    case "download":
      return { ok: true, action: { kind: "operator", operator: command.name as OperatorId } };
    case "open":
      return { ok: true, action: { kind: "open" } };
    case "selectall":
      return { ok: true, action: { kind: "select-all" } };
    case "clear":
      return { ok: true, action: { kind: "clear" } };

    case "budget":
      // Parsed by the caller with the SAME parseSize the budget editor uses, so
      // a value this accepts is byte-for-byte one the settings panel would have
      // accepted. Validating it twice, differently, is how they diverge.
      return { ok: true, action: { kind: "budget", text: args } };
    case "grace": {
      const minutes = Number.parseInt(args, 10);
      if (!Number.isFinite(minutes) || minutes < 0) {
        return { ok: false, error: `:grace needs a number of minutes, not "${args}"` };
      }
      return { ok: true, action: { kind: "grace", minutes } };
    }
    case "enable":
      return { ok: true, action: { kind: "enabled", enabled: true } };
    case "disable":
      return { ok: true, action: { kind: "enabled", enabled: false } };
    case "rescan":
      return { ok: true, action: { kind: "rescan" } };

    case "q":
      return { ok: true, action: { kind: "quit" } };
    case "h":
      return { ok: true, action: { kind: "help" } };
  }

  return { ok: false, error: `not a command: :${parsed.name}` };
}

/** An exact match wins; failing that a unique case-insensitive prefix; failing
 *  that the best fuzzy match, but only when it is unambiguous. `:folder Cam`
 *  should work; `:folder a` should not silently pick one of four folders whose
 *  names contain an "a". */
function matchOne(query: string, candidates: string[]): string | null {
  if (query === "") return null;
  const lower = query.toLowerCase();

  const exact = candidates.find((c) => c.toLowerCase() === lower);
  if (exact) return exact;

  const prefixed = candidates.filter((c) => c.toLowerCase().startsWith(lower));
  if (prefixed.length === 1) return prefixed[0];
  if (prefixed.length > 1) return null;

  const ranked = rankStrings(query, candidates, (c) => c);
  return ranked.length === 1 ? ranked[0] : null;
}

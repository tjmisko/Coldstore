import { useMemo, useState } from "react";
import { useTagCompletions } from "../hooks/useTags";
import { formatCount } from "../lib/format";
import { Overlay } from "./Overlay";

export type TagPromptMode = "add" | "remove";

interface TagPromptProps {
  folder: string;
  ids: number[];
  mode: TagPromptMode;
  selectionNote?: string | null;
  onApply(add: string[], remove: string[]): Promise<boolean>;
  onClose(): void;
}

const MAX_FILES = 5000;
const MAX_TAGS = 16;

function cleanToken(value: string): string {
  return value.trim().replace(/\s+/g, " ");
}

function appendUnique(current: string[], values: string[]): string[] {
  const next = [...current];
  const seen = new Set(current.map((tag) => tag.toLocaleLowerCase()));
  for (const value of values) {
    const cleaned = cleanToken(value);
    const fold = cleaned.toLocaleLowerCase();
    if (!cleaned || seen.has(fold)) continue;
    next.push(cleaned);
    seen.add(fold);
  }
  return next;
}

export function TagPrompt({ folder, ids, mode, selectionNote, onApply, onClose }: TagPromptProps) {
  const [tokens, setTokens] = useState<string[]>([]);
  const [input, setInput] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const uniqueIDs = useMemo(() => [...new Set(ids)], [ids]);
  const completions = useTagCompletions(folder, input, true);
  const residual = cleanToken(input);
  const exact = completions.tags.some(
    (tag) => tag.name.toLocaleLowerCase() === residual.toLocaleLowerCase(),
  );
  const overFileLimit = uniqueIDs.length > MAX_FILES;
  const overTagLimit = tokens.length + (residual ? 1 : 0) > MAX_TAGS;

  const addTokens = (values: string[]) => {
    setTokens((current) => appendUnique(current, values).slice(0, MAX_TAGS + 1));
  };

  const changeInput = (value: string) => {
    const parts = value.split(/[,;]/);
    if (parts.length === 1) {
      setInput(value);
      return;
    }
    addTokens(parts.slice(0, -1));
    setInput(parts.at(-1) ?? "");
  };

  const choose = (name: string) => {
    addTokens([name]);
    setInput("");
  };

  const apply = async () => {
    const names = appendUnique(tokens, [input]);
    if (names.length === 0 || names.length > MAX_TAGS || overFileLimit || busy) return;
    setBusy(true);
    setError(null);
    try {
      const ok = await onApply(mode === "add" ? names : [], mode === "remove" ? names : []);
      if (ok) onClose();
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
    } finally {
      setBusy(false);
    }
  };

  const countLabel = `${mode === "add" ? "Tag" : "Remove tags from"} ${formatCount(uniqueIDs.length)} file${uniqueIDs.length === 1 ? "" : "s"}`;

  return (
    <Overlay label={countLabel} onClose={onClose} className="max-w-2xl" testAttribute="tag-prompt">
      <div data-tag-prompt className="rounded-xl border border-line-strong bg-surface p-4 shadow-2xl">
        <div className="flex items-start justify-between gap-3">
          <div>
            <h2 className="text-base font-semibold" data-tag-prompt-count>{countLabel}</h2>
            <p className="mt-1 text-meta text-ink-muted">
              Separate several tags with a comma or semicolon. Nothing is written until Apply.
            </p>
          </div>
        </div>

        {selectionNote && <p className="mt-3 text-meta text-cold">{selectionNote}</p>}
        {overFileLimit && (
          <p className="mt-3 text-meta text-alarm" role="alert">
            This selection has {formatCount(uniqueIDs.length)} files. Bulk tagging supports at most {formatCount(MAX_FILES)}; no files will be tagged.
          </p>
        )}

        {tokens.length > 0 && (
          <div className="mt-3 flex flex-wrap gap-1.5">
            {tokens.map((tag) => (
              <span key={tag} data-tag-chip={tag} className="inline-flex items-center gap-1 rounded-full bg-raised px-2 py-1 text-meta text-ink">
                {tag}
                <button
                  type="button"
                  onClick={() => setTokens((current) => current.filter((value) => value !== tag))}
                  aria-label={`Remove ${tag} from this operation`}
                  className="text-ink-faint hover:text-ink"
                >
                  &times;
                </button>
              </span>
            ))}
          </div>
        )}

        <label className="mt-3 block text-meta text-ink-muted" htmlFor="tag-prompt-input">
          {mode === "add" ? "Tags to add" : "Tags to remove"}
        </label>
        <input
          id="tag-prompt-input"
          autoFocus
          value={input}
          onChange={(event) => changeInput(event.target.value)}
          onKeyDown={(event) => {
            if (event.key === "Enter" && !event.nativeEvent.isComposing) {
              event.preventDefault();
              if (residual) choose(residual);
              else void apply();
            }
          }}
          placeholder="e.g. Trips/Iceland"
          className="mt-1.5 w-full rounded-md border border-line-strong bg-raised px-3 py-2 text-base text-ink outline-none focus:border-cursor focus:ring-2 focus:ring-cursor/40"
        />

        <div className="mt-2 max-h-56 overflow-y-auto rounded-md border border-line bg-ground/40" aria-label="Tag completions">
          {completions.tags.map((tag) => (
            <button
              key={tag.id}
              type="button"
              data-tag-completion={tag.name}
              onClick={() => choose(tag.name)}
              className="flex w-full items-center justify-between gap-3 px-3 py-2 text-left text-meta text-ink-muted hover:bg-raised-hover hover:text-ink focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-inset focus-visible:ring-cursor"
            >
              <span className="truncate">{tag.name}</span>
              <span className="tabular text-ink-faint">
                {formatCount(tag.count)} exact · {formatCount(tag.descendant_count)} in subtree
              </span>
            </button>
          ))}
          {mode === "add" && residual && !exact && !completions.loading && (
            <button
              type="button"
              onClick={() => choose(residual)}
              className="block w-full px-3 py-2 text-left text-meta font-medium text-held hover:bg-raised-hover focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-inset focus-visible:ring-cursor"
            >
              + new tag “{residual}”
            </button>
          )}
          {completions.loading && <p className="px-3 py-2 text-meta text-ink-faint">Finding tags…</p>}
          {completions.error && <p className="px-3 py-2 text-meta text-alarm">{completions.error}</p>}
          {!completions.loading && completions.tags.length === 0 && !(mode === "add" && residual) && (
            <p className="px-3 py-2 text-meta text-ink-faint">No matching tags.</p>
          )}
        </div>

        {overTagLimit && (
          <p className="mt-2 text-meta text-alarm" role="alert">Use at most {MAX_TAGS} tags in one operation.</p>
        )}
        {error && <p className="mt-2 text-meta text-alarm" role="alert">{error}</p>}

        <div className="mt-4 flex justify-end gap-2">
          <button
            type="button"
            onClick={onClose}
            className="rounded-md px-3 py-1.5 text-base text-ink-muted ring-1 ring-line-strong hover:bg-raised-hover hover:text-ink focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-cursor"
          >
            Cancel
          </button>
          <button
            type="button"
            onClick={() => void apply()}
            disabled={busy || overFileLimit || overTagLimit || (!!residual && completions.loading) || (tokens.length === 0 && !residual)}
            className="rounded-md bg-ink px-4 py-1.5 text-base font-medium text-ground hover:brightness-95 disabled:opacity-50 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-cursor focus-visible:ring-offset-2 focus-visible:ring-offset-surface"
          >
            {busy ? "Applying…" : mode === "add" ? "Apply tags" : "Remove tags"}
          </button>
        </div>
      </div>
    </Overlay>
  );
}

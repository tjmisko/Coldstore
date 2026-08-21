import { useEffect, useMemo, useRef, useState } from "react";
import { useTagCompletions, useTagsForFiles } from "../hooks/useTags";
import { formatCount } from "../lib/format";
import { Overlay } from "./Overlay";

export type TagPromptMode = "add" | "remove";

interface TagPromptProps {
  folder: string;
  ids: number[];
  mode: TagPromptMode;
  selectionNote?: string | null;
  initialTags?: string[];
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

export function TagPrompt({ folder, ids, mode, selectionNote, initialTags = [], onApply, onClose }: TagPromptProps) {
  const [tokens, setTokens] = useState<string[]>(() => appendUnique([], initialTags));
  const [input, setInput] = useState("");
  const [seeded, setSeeded] = useState(initialTags.length > 0);
  const [completionIndex, setCompletionIndex] = useState(0);
  const [busy, setBusy] = useState(false);
  const busyRef = useRef(false);
  const [error, setError] = useState<string | null>(null);
  const uniqueIDs = useMemo(() => [...new Set(ids)], [ids]);
  const completions = useTagCompletions(folder, input, mode === "add");
  const present = useTagsForFiles(uniqueIDs, mode === "remove");
  const inputRef = useRef<HTMLInputElement>(null);
  const residual = cleanToken(input);
  const source = mode === "add" ? completions : present;
  const suggestions = source.tags.filter((tag) =>
    !residual || tag.name.toLocaleLowerCase().includes(residual.toLocaleLowerCase()),
  );
  const exact = source.tags.some(
    (tag) => tag.name.toLocaleLowerCase() === residual.toLocaleLowerCase(),
  );
  const removeExact = present.tags.find(
    (tag) => tag.name.toLocaleLowerCase() === residual.toLocaleLowerCase(),
  );
  const invalidRemoveToken = mode === "remove" && tokens.some((token) =>
    !present.tags.some((tag) => tag.name.toLocaleLowerCase() === token.toLocaleLowerCase()),
  );
  const overFileLimit = uniqueIDs.length > MAX_FILES;
  const overTagLimit = tokens.length + (residual ? 1 : 0) > MAX_TAGS;

  useEffect(() => {
    inputRef.current?.focus();
  }, []);

  const changeInput = (value: string) => {
    const parts = value.split(/[,;]/);
    const base = seeded ? [] : tokens;
    if (seeded) {
      setSeeded(false);
      setTokens([]);
    }
    if (parts.length === 1) {
      setInput(value);
      setCompletionIndex(0);
      return;
    }
    setTokens(appendUnique(base, parts.slice(0, -1)).slice(0, MAX_TAGS + 1));
    setInput(parts.at(-1) ?? "");
    setCompletionIndex(0);
  };

  const choose = (name: string) => {
    setTokens((current) => appendUnique(seeded ? [] : current, [name]).slice(0, MAX_TAGS + 1));
    setSeeded(false);
    setInput("");
    setCompletionIndex(0);
  };

  const apply = async () => {
    const residualName = mode === "remove" ? removeExact?.name ?? "" : input;
    const names = appendUnique(tokens, [residualName]);
    if (names.length === 0 || names.length > MAX_TAGS || overFileLimit || invalidRemoveToken || busyRef.current) return;
    busyRef.current = true;
    setBusy(true);
    setError(null);
    try {
      const ok = await onApply(mode === "add" ? names : [], mode === "remove" ? names : []);
      if (ok) onClose();
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
    } finally {
      busyRef.current = false;
      setBusy(false);
    }
  };

  // Once Apply has handed the request to fetch, aborting the browser request
  // cannot prove the server did not commit its transaction. Keep the operation
  // visibly modal until its result is known instead of letting Escape, the
  // backdrop, or a Cancel click promise a cancellation we cannot guarantee.
  const closeUnlessApplying = () => {
    if (!busyRef.current) onClose();
  };

  const countLabel = `${mode === "add" ? "Tag" : "Remove tags from"} ${formatCount(uniqueIDs.length)} file${uniqueIDs.length === 1 ? "" : "s"}`;

  return (
    <Overlay label={countLabel} onClose={closeUnlessApplying} align="bottom" className="max-w-2xl" testAttribute="tag-prompt" scope="tag-prompt">
      <div data-tag-prompt data-keyscope="tag-prompt" className="rounded-xl border border-line-strong bg-surface p-4 shadow-2xl">
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
          ref={inputRef}
          id="tag-prompt-input"
          autoFocus
          value={input}
          onChange={(event) => changeInput(event.target.value)}
          onKeyDown={(event) => {
            if (event.key === "Escape") {
              event.preventDefault();
              closeUnlessApplying();
              return;
            }
            if ((event.key === "Tab" && !event.ctrlKey && !event.metaKey) ||
                ((event.ctrlKey || event.metaKey) && ["n", "p"].includes(event.key.toLowerCase()))) {
              event.preventDefault();
              if (suggestions.length > 0) {
                const backwards = event.shiftKey || event.key.toLowerCase() === "p";
                setCompletionIndex((current) =>
                  (current + (backwards ? suggestions.length - 1 : 1)) % suggestions.length,
                );
              }
              return;
            }
            if ((event.ctrlKey || event.metaKey) && event.key.toLowerCase() === "u") {
              event.preventDefault();
              setSeeded(false);
              setTokens([]);
              setInput("");
              return;
            }
            if ((event.ctrlKey || event.metaKey) && event.key.toLowerCase() === "w") {
              event.preventDefault();
              setSeeded(false);
              if (input) setInput((current) => current.replace(/\s*\S+\s*$/, ""));
              else setTokens((current) => current.slice(0, -1));
              return;
            }
            if (event.key === "Enter" && !event.nativeEvent.isComposing) {
              event.preventDefault();
              if (residual && suggestions[completionIndex]) choose(suggestions[completionIndex].name);
              else if (residual && mode === "add") choose(residual);
              else if (residual && removeExact) choose(removeExact.name);
              else void apply();
            }
          }}
          placeholder="e.g. Trips/Iceland"
          className="mt-1.5 w-full rounded-md border border-line-strong bg-raised px-3 py-2 text-base text-ink outline-none focus:border-cursor focus:ring-2 focus:ring-cursor/40"
        />

        <div className="mt-2 max-h-56 overflow-y-auto rounded-md border border-line bg-ground/40" aria-label="Tag completions">
          {suggestions.map((tag, index) => (
            <button
              key={tag.id}
              type="button"
              data-tag-completion={tag.name}
              onClick={() => choose(tag.name)}
              aria-selected={index === completionIndex}
              className={`flex w-full items-center justify-between gap-3 px-3 py-2 text-left text-meta hover:text-ink focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-inset focus-visible:ring-cursor ${index === completionIndex ? "bg-raised-hover text-ink" : "text-ink-muted"}`}
            >
              <span className="truncate">{tag.name}</span>
              <span className="tabular text-ink-faint">
                {mode === "remove"
                  ? `${formatCount(tag.count)} of ${formatCount(uniqueIDs.length)}`
                  : `${formatCount(tag.count)} exact · ${formatCount(tag.descendant_count)} in subtree`}
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
          {source.loading && <p className="px-3 py-2 text-meta text-ink-faint">Finding tags…</p>}
          {source.error && <p className="px-3 py-2 text-meta text-alarm">{source.error}</p>}
          {!source.loading && suggestions.length === 0 && !(mode === "add" && residual) && (
            <p className="px-3 py-2 text-meta text-ink-faint">No matching tags.</p>
          )}
        </div>

        {overTagLimit && (
          <p className="mt-2 text-meta text-alarm" role="alert">Use at most {MAX_TAGS} tags in one operation.</p>
        )}
        {invalidRemoveToken && !present.loading && (
          <p className="mt-2 text-meta text-alarm" role="alert">Choose only tags present on the covered files.</p>
        )}
        {error && <p className="mt-2 text-meta text-alarm" role="alert">{error}</p>}

        <div className="mt-4 flex justify-end gap-2">
          <button
            type="button"
            onClick={closeUnlessApplying}
            disabled={busy}
            className="rounded-md px-3 py-1.5 text-base text-ink-muted ring-1 ring-line-strong hover:bg-raised-hover hover:text-ink focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-cursor"
          >
            Cancel
          </button>
          <button
            type="button"
            onClick={() => void apply()}
            disabled={busy || overFileLimit || overTagLimit || invalidRemoveToken || (!!residual && (source.loading || (mode === "remove" && !removeExact))) || (tokens.length === 0 && !residual)}
            className="rounded-md bg-ink px-4 py-1.5 text-base font-medium text-ground hover:brightness-95 disabled:opacity-50 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-cursor focus-visible:ring-offset-2 focus-visible:ring-offset-surface"
          >
            {busy ? "Applying…" : mode === "add" ? "Apply tags" : "Remove tags"}
          </button>
        </div>
      </div>
    </Overlay>
  );
}

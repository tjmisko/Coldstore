import { useCallback, useMemo, useRef, useState } from "react";
import {
  completions,
  parseCommandLine,
  resolveCommandLine,
  type CompletionContext,
  type PaletteAction,
} from "../keymap/palette";
import { Overlay } from "./Overlay";

/** Most recently used command names, in module scope.
 *
 *  Not state and not localStorage: it has to survive the palette unmounting
 *  between uses, and it is not worth persisting across a reload -- an ordering
 *  hint that outlives the session would be stale far more often than useful. */
const recent: string[] = [];

function remember(name: string) {
  const at = recent.indexOf(name);
  if (at !== -1) recent.splice(at, 1);
  recent.unshift(name);
  recent.splice(8);
}

interface CommandPaletteProps {
  /** Text the palette opens with, e.g. ":folder " from <leader>o. */
  initial: string;
  folders: string[];
  filters: { key: string; label: string }[];
  onClose(): void;
  /** Returns an error message, or null when the action was taken. */
  onAction(action: PaletteAction): string | null;
}

export function CommandPalette({
  initial,
  folders,
  filters,
  onClose,
  onAction,
}: CommandPaletteProps) {
  const [line, setLine] = useState(initial === "" ? ":" : initial);
  const [highlight, setHighlight] = useState(0);
  const [error, setError] = useState<string | null>(null);
  const inputRef = useRef<HTMLInputElement>(null);

  const context = useMemo<CompletionContext>(
    () => ({ folders, filters, views: ["gallery", "files", "config"], recent: [...recent] }),
    [folders, filters],
  );

  const candidates = useMemo(() => completions(line, context).slice(0, 12), [line, context]);
  const active = candidates[Math.min(highlight, Math.max(0, candidates.length - 1))];

  const submit = useCallback(
    (text: string) => {
      const result = resolveCommandLine(text, context);
      if (!result.ok) {
        setError(result.error);
        return;
      }
      const failure = onAction(result.action);
      if (failure) {
        setError(failure);
        return;
      }
      remember(parseCommandLine(text).name);
      onClose();
    },
    [context, onAction, onClose],
  );

  const take = useCallback(
    (index: number) => {
      const candidate = candidates[index];
      if (!candidate) return;
      if (candidate.terminal) {
        submit(candidate.line);
        return;
      }
      setLine(candidate.line);
      setHighlight(0);
      setError(null);
    },
    [candidates, submit],
  );

  const onKeyDown = useCallback(
    (event: React.KeyboardEvent<HTMLInputElement>) => {
      // The palette's input is a real text field, so the engine has already
      // suspended itself. These are the palette's own keys.
      if (event.key === "Escape") {
        event.preventDefault();
        onClose();
        return;
      }
      if (event.key === "Enter") {
        event.preventDefault();
        // Enter takes the highlighted completion when the owner has moved the
        // highlight, and runs what they typed otherwise.
        if (highlight > 0 && active) take(highlight);
        else submit(line);
        return;
      }
      if (event.key === "Tab") {
        event.preventDefault();
        if (candidates.length > 0) take(Math.min(highlight, candidates.length - 1));
        return;
      }
      const down =
        event.key === "ArrowDown" || (event.ctrlKey && event.key.toLowerCase() === "n");
      const up = event.key === "ArrowUp" || (event.ctrlKey && event.key.toLowerCase() === "p");
      if (down || up) {
        event.preventDefault();
        setHighlight((current) => {
          const size = Math.max(1, candidates.length);
          return (current + (down ? 1 : size - 1)) % size;
        });
      }
    },
    [active, candidates.length, highlight, line, onClose, submit, take],
  );

  return (
    <Overlay label="Command palette" onClose={onClose} align="top" className="max-w-2xl" testAttribute="palette">
      <div className="rounded-xl border border-neutral-700 bg-neutral-900 shadow-2xl overflow-hidden">
        <input
          ref={inputRef}
          autoFocus
          data-command-palette
          aria-label="Command"
          spellCheck={false}
          autoComplete="off"
          value={line}
          onChange={(event) => {
            setLine(event.target.value.startsWith(":") ? event.target.value : `:${event.target.value}`);
            setHighlight(0);
            setError(null);
          }}
          onKeyDown={onKeyDown}
          className="w-full bg-transparent px-4 py-3 font-mono text-sm outline-none border-b border-neutral-800"
        />

        {error && (
          <p className="px-4 py-2 text-xs text-red-400" role="alert" data-palette-error>
            {error}
          </p>
        )}

        <ul className="max-h-[50vh] overflow-y-auto" role="listbox" aria-label="Completions">
          {candidates.map((candidate, index) => (
            <li key={candidate.line}>
              <button
                type="button"
                role="option"
                aria-selected={index === highlight}
                data-palette-option={candidate.label}
                onMouseDown={(event) => {
                  event.preventDefault();
                  take(index);
                }}
                className={`flex w-full items-baseline gap-3 px-4 py-1.5 text-left text-sm ${
                  index === highlight ? "bg-neutral-800" : "hover:bg-neutral-800/60"
                }`}
              >
                <span className="font-mono text-xs text-neutral-200 w-32 shrink-0">
                  {candidate.label}
                </span>
                <span className="text-xs text-neutral-500">{candidate.hint}</span>
              </button>
            </li>
          ))}
        </ul>

        <p className="border-t border-neutral-800 px-4 py-2 text-xs text-neutral-500">
          File commands act on the selection, or on the file under the cursor when nothing is
          selected. Tab completes, Enter runs, Escape closes.
        </p>
      </div>
    </Overlay>
  );
}

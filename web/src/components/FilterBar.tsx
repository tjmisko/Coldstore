import { useState } from "react";
import { FILTER_PRESETS, type FilterKey } from "../hooks/useFileWorkspace";
import { useTagCompletions } from "../hooks/useTags";
import type { NearFilter } from "../lib/api";

interface FilterBarProps {
  active: FilterKey;
  onChange(key: FilterKey): void;
  near?: NearFilter | null;
  onClearNear?(): void;
  folder?: string;
  tags?: string[];
  onTagsChange?(tags: string[]): void;
}

export function FilterBar({
  active,
  onChange,
  near = null,
  onClearNear = () => {},
  folder,
  tags = [],
  onTagsChange = () => {},
}: FilterBarProps) {
  const preset = FILTER_PRESETS.find((p) => p.key === active) ?? FILTER_PRESETS[0];
  const [tagQuery, setTagQuery] = useState("");
  const [tagInputActive, setTagInputActive] = useState(false);
  const completions = useTagCompletions(folder, tagQuery, tagInputActive);

  const addTag = () => {
    const requested = tagQuery.trim();
    if (!requested) return;
    const canonical = completions.tags.find(
      (tag) => tag.name.toLocaleLowerCase() === requested.toLocaleLowerCase(),
    )?.name ?? requested;
    if (!tags.some((tag) => tag.toLocaleLowerCase() === canonical.toLocaleLowerCase())) {
      onTagsChange([...tags, canonical]);
    }
    setTagQuery("");
  };

  return (
    <div className="mb-4">
      <div className="flex flex-wrap gap-1.5" role="tablist" aria-label="File filters">
        {FILTER_PRESETS.map((option) => (
          <button
            key={option.key}
            role="tab"
            aria-selected={option.key === active}
            data-filter={option.key}
            onClick={() => onChange(option.key)}
            className={`px-3 py-1 rounded-full text-meta transition-colors focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-cursor ${
              option.key === active
                ? "bg-ink text-ground font-medium"
                : "bg-raised text-ink-muted hover:bg-raised-hover hover:text-ink"
            }`}
          >
            {option.label}
          </button>
        ))}
        {near && (
          <button
            type="button"
            data-filter="near"
            aria-label={`Clear nearby filter within ${near.radiusKm} kilometres`}
            onClick={onClearNear}
            className="rounded-full bg-held-dim px-3 py-1 text-meta font-medium text-held transition-colors hover:brightness-95 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-cursor"
          >
            Within {near.radiusKm} km of {near.lat.toFixed(4)}, {near.lon.toFixed(4)} &times;
          </button>
        )}
      </div>
      <div className="mt-2 flex flex-wrap items-center gap-2" data-tag-filters>
        <form
          className="flex items-center gap-1.5"
          onSubmit={(event) => {
            event.preventDefault();
            addTag();
          }}
        >
          <label htmlFor="tag-filter" className="text-meta text-ink-faint">
            Tags
          </label>
          <input
            id="tag-filter"
            type="search"
            list="tag-filter-options"
            value={tagQuery}
            onFocus={() => setTagInputActive(true)}
            onBlur={() => setTagInputActive(false)}
            onChange={(event) => setTagQuery(event.target.value)}
            placeholder={folder ? "Filter this folder" : "Filter all folders"}
            aria-label={folder ? `Filter ${folder} by tag` : "Filter all folders by tag"}
            className="w-44 rounded-lg border border-line-strong bg-raised px-2.5 py-1 text-meta text-ink outline-none transition-colors placeholder:text-ink-faint focus:border-cursor focus:ring-2 focus:ring-cursor/35"
          />
          <datalist id="tag-filter-options">
            {completions.tags.map((tag) => (
              <option key={tag.id} value={tag.name}>{tag.descendant_count} files</option>
            ))}
          </datalist>
          <button
            type="submit"
            disabled={!tagQuery.trim()}
            className="rounded-lg bg-raised px-2.5 py-1 text-meta text-ink-muted ring-1 ring-line-strong transition-colors hover:bg-raised-hover hover:text-ink disabled:opacity-40 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-cursor"
          >
            Add
          </button>
        </form>

        {tags.map((tag) => (
          <button
            key={tag}
            type="button"
            data-tag-filter={tag}
            aria-label={`Remove tag filter ${tag}`}
            onClick={() => onTagsChange(tags.filter((candidate) => candidate !== tag))}
            className="rounded-full bg-cursor-dim px-3 py-1 text-meta text-cursor ring-1 ring-cursor/30 transition-colors hover:brightness-125 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-cursor"
          >
            {tag} &times;
          </button>
        ))}
        {tags.length > 1 && <span className="text-micro text-ink-faint">matching every tag</span>}
        {completions.error && <span className="text-meta text-alarm">Tags unavailable: {completions.error}</span>}
      </div>
      {preset.hint && <p className="mt-2 text-meta text-ink-faint">{preset.hint}</p>}
    </div>
  );
}

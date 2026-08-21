import { FILTER_PRESETS, type FilterKey } from "../hooks/useFileWorkspace";
import type { NearFilter } from "../lib/api";

interface FilterBarProps {
  active: FilterKey;
  onChange(key: FilterKey): void;
  near?: NearFilter | null;
  onClearNear?(): void;
}

export function FilterBar({ active, onChange, near = null, onClearNear = () => {} }: FilterBarProps) {
  const preset = FILTER_PRESETS.find((p) => p.key === active) ?? FILTER_PRESETS[0];

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
      {preset.hint && <p className="mt-2 text-meta text-ink-faint">{preset.hint}</p>}
    </div>
  );
}

import { FILTER_PRESETS, type FilterKey } from "../hooks/useFileWorkspace";

interface FilterBarProps {
  active: FilterKey;
  onChange(key: FilterKey): void;
}

export function FilterBar({ active, onChange }: FilterBarProps) {
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
      </div>
      {preset.hint && <p className="mt-2 text-meta text-ink-faint">{preset.hint}</p>}
    </div>
  );
}

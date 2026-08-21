import { FILTER_PRESETS, type FilterKey } from "../hooks/useFileWorkspace";

interface FilterBarProps {
  active: FilterKey;
  onChange(key: FilterKey): void;
}

export function FilterBar({ active, onChange }: FilterBarProps) {
  const preset = FILTER_PRESETS.find((p) => p.key === active) ?? FILTER_PRESETS[0];

  return (
    <div className="mb-3">
      <div className="flex flex-wrap gap-1.5" role="tablist" aria-label="File filters">
        {FILTER_PRESETS.map((option) => (
          <button
            key={option.key}
            role="tab"
            aria-selected={option.key === active}
            data-filter={option.key}
            onClick={() => onChange(option.key)}
            className={`px-2.5 py-1 rounded-full text-xs transition-colors ${
              option.key === active
                ? "bg-neutral-200 text-neutral-900"
                : "bg-neutral-800 text-neutral-400 hover:bg-neutral-700 hover:text-neutral-200"
            }`}
          >
            {option.label}
          </button>
        ))}
      </div>
      {preset.hint && <p className="mt-1.5 text-xs text-neutral-500">{preset.hint}</p>}
    </div>
  );
}

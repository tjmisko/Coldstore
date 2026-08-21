interface TagChipsProps {
  tags: string[];
  /** Tiles and rows default to the compact two-chip contract. */
  max?: number;
  className?: string;
}

export function TagChips({ tags, max = 2, className = "" }: TagChipsProps) {
  if (tags.length === 0) return null;
  const visible = tags.slice(0, max);
  const hidden = tags.length - visible.length;
  return (
    <span className={`inline-flex min-w-0 items-center gap-1 ${className}`} aria-label={`Tags: ${tags.join(", ")}`}>
      {visible.map((tag) => (
        <span
          key={tag}
          data-tag-chip={tag}
          title={tag}
          className="max-w-28 truncate rounded-full border border-line-strong bg-surface/90 px-1.5 py-0.5 text-micro text-ink-muted backdrop-blur-[2px]"
        >
          {tag}
        </span>
      ))}
      {hidden > 0 && (
        <span
          data-tag-overflow={hidden}
          title={tags.slice(max).join(", ")}
          className="rounded-full border border-line-strong bg-surface/90 px-1.5 py-0.5 text-micro text-ink-faint"
        >
          +{hidden}
        </span>
      )}
    </span>
  );
}

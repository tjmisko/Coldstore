import type { FileRecord } from "../types/api";

interface PinToggleProps {
  file: FileRecord;
  onPin(pinned: boolean): void;
  compact?: boolean;
}

/** A pin is visible, never silent (D2). An auto-pin says so in its tooltip:
 *  the owner did not press anything, and a badge they cannot account for is
 *  what makes the eventual wedge inexplicable. */
export function PinToggle({ file, onPin, compact = false }: PinToggleProps) {
  const title = file.pinned
    ? file.pin_source === "restore"
      ? "Auto-pinned when this file was restored. Click to unpin."
      : "Pinned by you. Click to unpin."
    : "Pin: never archive this file";

  return (
    <button
      type="button"
      title={title}
      aria-label={file.pinned ? `Unpin ${file.filename}` : `Pin ${file.filename}`}
      aria-pressed={file.pinned}
      data-pin-toggle={file.id}
      data-pinned={file.pinned}
      data-pin-source={file.pin_source}
      onClick={(e) => {
        e.stopPropagation();
        onPin(!file.pinned);
      }}
      className={`rounded ${compact ? "p-1 bg-black/50" : "p-1.5 hover:bg-neutral-700"} ${
        file.pinned
          ? file.pin_source === "restore"
            ? "text-sky-300"
            : "text-indigo-300"
          : `text-neutral-500 hover:text-neutral-200 ${compact ? "opacity-0 group-hover:opacity-100 focus:opacity-100" : ""}`
      }`}
    >
      <svg
        className="w-4 h-4"
        viewBox="0 0 24 24"
        fill={file.pinned ? "currentColor" : "none"}
        stroke="currentColor"
        strokeWidth={1.8}
        aria-hidden="true"
      >
        <path
          strokeLinecap="round"
          strokeLinejoin="round"
          d="M16 9V4h1a1 1 0 0 0 0-2H7a1 1 0 0 0 0 2h1v5c0 1.66-1.34 3-3 3v2h5.97v7l1 1 1-1v-7H19v-2c-1.66 0-3-1.34-3-3z"
        />
      </svg>
    </button>
  );
}

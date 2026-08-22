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
      className={`rounded-md transition-colors focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-cursor ${
        compact ? "p-1.5 bg-ground/55 backdrop-blur-[2px]" : "p-1.5 hover:bg-raised-hover"
      } ${
        file.pinned
          ? "text-held"
          : // An unpinned pin stays out of the way on a pointer device, but a
            // touch device has no hover to reveal it with -- the same defect
            // that made gallery selection unreachable on a phone. The media
            // query is what keeps the photo clean on a desktop without
            // stranding the control on a phone.
            `text-ink-faint hover:text-ink ${compact ? "opacity-0 group-hover:opacity-100 focus-visible:opacity-100 [@media(hover:none)]:opacity-70" : ""}`
      }`}
    >
      <svg
        className="w-4 h-4"
        viewBox="0 0 24 24"
        // Both kinds of pin wear the held hue, because both mean the same
        // thing: this file is being kept off the array. They differ by fill,
        // not by shade -- a solid pin is one the owner pressed, an outlined one
        // is a pin coldstore added on restore. Opacity alone was not a legible
        // difference, and two separate hues (the old sky/indigo pair) implied
        // two meanings where there is one.
        fill={file.pinned && file.pin_source !== "restore" ? "currentColor" : "none"}
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

import { useEffect, useRef, type ReactNode } from "react";

interface ConfirmDialogProps {
  title: string;
  children: ReactNode;
  confirmLabel: string;
  cancelLabel?: string;
  destructive?: boolean;
  onConfirm(): void;
  onCancel(): void;
}

/** A modal that states the consequence in numbers.
 *
 *  The one it exists for is the restore budget over-run: coldstore's answer to
 *  a folder going over budget is to archive a comparable volume of files the
 *  owner never selected, and every archive is a Syncthing deletion on their
 *  phone. "Are you sure?" is not an honest way to ask that. */
export function ConfirmDialog({
  title,
  children,
  confirmLabel,
  cancelLabel = "Cancel",
  destructive = false,
  onConfirm,
  onCancel,
}: ConfirmDialogProps) {
  const cancelRef = useRef<HTMLButtonElement>(null);

  useEffect(() => {
    cancelRef.current?.focus();
    const onKey = (e: KeyboardEvent) => {
      if (e.key === "Escape") onCancel();
    };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, [onCancel]);

  return (
    <div className="fixed inset-0 z-70 flex items-center justify-center bg-ground/80 backdrop-blur-sm p-4">
      <div
        role="dialog"
        aria-modal="true"
        aria-label={title}
        className="w-full max-w-lg rounded-xl border border-line-strong bg-surface p-5 shadow-2xl"
      >
        <h2 className="text-base font-semibold mb-2">{title}</h2>
        <div className="text-base text-ink-muted space-y-2">{children}</div>
        <div className="mt-5 flex justify-end gap-2">
          <button
            ref={cancelRef}
            onClick={onCancel}
            className="px-3 py-1.5 text-base rounded-md text-ink-muted ring-1 ring-line-strong hover:bg-raised-hover hover:text-ink focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-cursor"
          >
            {cancelLabel}
          </button>
          <button
            onClick={onConfirm}
            className={`px-3 py-1.5 text-base rounded-md font-medium focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-cursor focus-visible:ring-offset-2 focus-visible:ring-offset-surface ${
              destructive
                ? "bg-alarm text-ground hover:brightness-110"
                : "bg-ink text-ground hover:brightness-95"
            }`}
          >
            {confirmLabel}
          </button>
        </div>
      </div>
    </div>
  );
}

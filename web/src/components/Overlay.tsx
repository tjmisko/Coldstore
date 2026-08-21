import { useCallback, type ReactNode } from "react";
import { useFocusTrap } from "../hooks/useFocusTrap";
import { useKeymapLayer } from "../hooks/useKeymapLayer";
import { useScrollLock } from "../hooks/useScrollLock";
import type { Command } from "../keymap/types";
import { Z } from "../lib/zLayers";
import { Portal } from "./Portal";

interface OverlayProps {
  label: string;
  onClose(): void;
  children: ReactNode;
  /** Where the panel sits. "top" is the palette and the finder, which want to
   *  be near the top of the screen the way an ex-line is. */
  align?: "top" | "center";
  zIndex?: number;
  /** Skip the modal role. The context menu is not a dialog. */
  role?: "dialog" | "none";
  className?: string;
  testAttribute?: string;
}

/** One overlay primitive: portal, focus trap, scroll lock, backdrop, z-index,
 *  and a scope on the keymap stack.
 *
 *  Before this each of those was either absent or open-coded. ConfirmDialog had
 *  its own window listener for Escape, which is why Escape used to close the
 *  lightbox and the dialog on top of it in the same press. */
export function Overlay({
  label,
  onClose,
  children,
  align = "center",
  zIndex = Z.overlay,
  role = "dialog",
  className = "",
  testAttribute,
}: OverlayProps) {
  const containerRef = useFocusTrap(true);
  useScrollLock(true);

  // The overlay scope is exclusive, so no TYPED key produces a command here
  // except cancel, which means close. Anything else arriving is a programmatic
  // dispatch -- `:archive` from the palette -- and belongs to the layer below.
  const handle = useCallback(
    (command: Command): boolean => {
      if (command.kind === "action" && command.action === "cancel") {
        onClose();
        return true;
      }
      return false;
    },
    [onClose],
  );
  useKeymapLayer("overlay", handle);

  const dialogProps =
    role === "dialog" ? ({ role: "dialog", "aria-modal": true } as const) : ({} as const);

  return (
    <Portal>
      <div
        style={{ zIndex }}
        className={`fixed inset-0 flex ${
          align === "top" ? "items-start pt-[10vh]" : "items-center"
        } justify-center bg-black/70 p-4`}
        onMouseDown={(event) => {
          if (event.target === event.currentTarget) onClose();
        }}
      >
        <div
          ref={(node) => {
            containerRef.current = node;
          }}
          {...dialogProps}
          aria-label={label}
          tabIndex={-1}
          data-overlay={testAttribute ?? label}
          className={`w-full outline-none ${className}`}
        >
          {children}
        </div>
      </div>
    </Portal>
  );
}

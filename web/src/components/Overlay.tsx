import { useCallback, type ReactNode } from "react";
import { useFocusTrap } from "../hooks/useFocusTrap";
import { useKeymapLayer } from "../hooks/useKeymapLayer";
import { useScrollLock } from "../hooks/useScrollLock";
import type { Command, Scope } from "../keymap/types";
import { Z } from "../lib/zLayers";
import { Portal } from "./Portal";

interface OverlayProps {
  label: string;
  onClose(): void;
  children: ReactNode;
  /** Where the panel sits. "top" is the palette and the finder, which want to
   *  be near the top of the screen the way an ex-line is. */
  align?: "top" | "center" | "bottom";
  zIndex?: number;
  /** Skip the modal role. The context menu is not a dialog. */
  role?: "dialog" | "none";
  className?: string;
  testAttribute?: string;
  /** Most overlays use the generic exclusive scope. The tag picker has its
   * own exclusive scope so browser tests and programmatic dispatch can name it. */
  scope?: Scope;
  /** Command-line surfaces need an exclusive key scope without pretending to
   * be modal: no backdrop, focus trap, scroll lock, or aria-modal contract. */
  modal?: boolean;
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
  scope = "overlay",
  modal = true,
}: OverlayProps) {
  const containerRef = useFocusTrap(modal);
  useScrollLock(modal);

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
  useKeymapLayer(scope, handle);

  const dialogProps = role === "dialog"
    ? ({ role: "dialog", ...(modal ? { "aria-modal": true as const } : {}) } as const)
    : ({} as const);

  return (
    <Portal>
      <div
        style={{ zIndex }}
        className={`fixed inset-0 flex ${modal ? "bg-black/70 p-4" : "pointer-events-none p-2"} ${
          align === "top" ? "items-start pt-[10vh]" : align === "bottom" ? "items-end pb-[3vh]" : "items-center"
        } justify-center`}
        onMouseDown={(event) => {
          if (modal && event.target === event.currentTarget) onClose();
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
          data-keyscope={scope}
          className={`w-full outline-none ${modal ? "" : "pointer-events-auto"} ${className}`}
        >
          {children}
        </div>
      </div>
    </Portal>
  );
}

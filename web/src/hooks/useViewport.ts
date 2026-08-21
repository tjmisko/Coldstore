import { useSyncExternalStore } from "react";

/** The window size, as a value.
 *
 *  `useSyncExternalStore` rather than a resize listener writing state: the
 *  listener version is a setState in an effect, which this codebase does not
 *  allow, and this is what the API is for. */
function subscribe(onChange: () => void): () => void {
  window.addEventListener("resize", onChange);
  return () => window.removeEventListener("resize", onChange);
}

function snapshot(): string {
  return `${window.innerWidth}x${window.innerHeight}`;
}

export interface Viewport {
  width: number;
  height: number;
}

export function useViewport(): Viewport {
  const value = useSyncExternalStore(subscribe, snapshot, () => "1280x800");
  const [width, height] = value.split("x").map(Number);
  return { width, height };
}

/** The gallery's column count, mirroring the Tailwind classes on the grid
 *  (`grid-cols-2 sm:3 md:4 lg:5 xl:6`).
 *
 *  Read off the window width because that is what a media query measures. A
 *  motion engine that thinks the grid is 4 wide when it is 6 sends `j` two
 *  tiles sideways, which reads as the cursor being broken rather than as a
 *  layout mismatch. */
export function galleryColumns(width: number): number {
  if (width >= 1280) return 6;
  if (width >= 1024) return 5;
  if (width >= 768) return 4;
  if (width >= 640) return 3;
  return 2;
}

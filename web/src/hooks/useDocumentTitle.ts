import { useEffect } from "react";

/** The name that owns the tab when nothing more specific is showing. */
export const APP_NAME = "Coldstore";

/** Sets `document.title` to "<title> -- Coldstore", or just "Coldstore" when
 *  `title` is empty. An empty title is the pre-status state (no folder picked
 *  yet), and rendering "  -- Coldstore" there would be worse than the bare name.
 *
 *  The tab is the only place the owner can tell two Coldstore windows apart, so
 *  the specific part comes first: browsers truncate tab labels from the right. */
export function useDocumentTitle(title: string): void {
  useEffect(() => {
    document.title = title ? `${title} — ${APP_NAME}` : APP_NAME;
  }, [title]);
}

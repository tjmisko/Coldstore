import type { FileRecord } from "./api";

/** A request to show a complete record in the Files view.
 *
 * Carrying the record, rather than only its id, lets the browser render and
 * focus a finder match even when it lies beyond the list's first loaded page.
 */
export interface FileReveal {
  file: FileRecord;
  /** Changes for every request so showing the same file twice still focuses it. */
  token: number;
}

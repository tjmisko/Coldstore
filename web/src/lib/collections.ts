import type { StatusResponse } from "../types/api";

/** The first API id of an archive collection entry (contract C8). Every legacy
 *  catalog rowid is below it, and it is below Number.MAX_SAFE_INTEGER. */
export const COLLECTION_ID_BASE = 2 ** 52;

/** The reply to anything a collection cannot do. */
export const NOT_AVAILABLE_FOR_COLLECTION = "not available for a collection";

/** The only place in web/src that compares an id to the collection threshold. */
export function isCollectionFileId(id: number): boolean {
  return id >= COLLECTION_ID_BASE;
}

/** True when `label` names a folder whose status says it is a collection. A
 *  status without `kind` is a sync folder, as is a label the status lacks. */
export function isCollectionFolder(status: StatusResponse | null | undefined, label: string): boolean {
  if (!status || label === "") return false;
  return status.folders.some((folder) => folder.label === label && folder.kind === "collection");
}

/** The filter presets that mean something for a collection: it has no
 *  on-device state, no pins, no conflicts and no capture metadata. */
export const COLLECTION_FILTER_KEYS: readonly string[] = ["all", "archived", "untagged", "trash"];

export function isCollectionFilterKey(key: string): boolean {
  return COLLECTION_FILTER_KEYS.includes(key);
}

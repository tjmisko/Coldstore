export type FileStatus =
  | "synced"
  | "archiving"
  | "archived"
  | "restoring"
  | "missing"
  | "trashing"
  | "trashed"
  | "untrashing";

/** The two pins D2 keeps apart: "restore" is applied automatically by every
 *  restore, "user" is an explicit press and is never downgraded to "restore". */
export type PinSource = "user" | "restore";

/** Sort keys GET /api/v1/files understands. "pinned_at" is ascending -- oldest
 *  pin first -- because the auto-pins an owner has forgotten are the ones the
 *  wedge banner sends them to triage. */
export type FileSort = "mtime" | "size" | "name" | "pinned_at" | "captured";

export type JobAction = "restore" | "archive" | "scan" | "rename" | "trash" | "untrash" | "delete";
export type JobState = "pending" | "running" | "done" | "failed";

export interface ApiError {
  error: string;
}

export interface FileRecord {
  id: number;
  folder_label: string;
  relative_path: string;
  filename: string;
  extension: string;
  mime_type: string;
  size_bytes: number;
  file_mtime: string;
  file_atime: string;
  first_seen: string;
  archived_at?: string;
  trashed_at?: string;
  /** The tree a recoverable trash restore returns to. */
  trashed_from?: "synced" | "archived";
  status: FileStatus;
  thumbnail_path?: string;
  content_hash?: string;
  /** D2: a pinned file is never an offload candidate, whatever its age. */
  pinned: boolean;
  /** Epoch seconds, or null when the file is not pinned. */
  pinned_at: number | null;
  /** "restore" is the automatic pin a restore applies; "user" is explicit and
   *  is never downgraded to "restore". */
  pin_source: "" | PinSource;
  thumb_attempts: number;
  thumb_last_attempt?: number;
  thumb_error?: string;
  /** What the file's leading bytes say it is, as opposed to `mime_type`, which
   *  is only what its extension claims. Absent on a row the scanner has not
   *  sniffed yet. It is the ONLY thing that can classify a file with no
   *  extension -- a bookmark export called "reading" has an empty `extension`
   *  and an empty `mime_type`. */
  detected_type?: string;
  description: string;
  /** Naive camera-local wall clock; empty when the file recorded none. */
  captured_local: string;
  /** Recorded UTC offset in minutes, or null when the file did not carry one. */
  captured_offset_minutes: number | null;
  captured_source: string;
  /** WGS84 coordinates. Zero is valid; null means unknown. */
  gps_lat: number | null;
  gps_lon: number | null;
  gps_altitude_m?: number;
  gps_accuracy_m?: number;
  gps_source: string;
  pair_key: string;
  pair_source: string;
  meta_scanned_at: number | null;
  meta_attempts: number;
  meta_last_attempt?: number;
  meta_error?: string;
  /** Hydrated by the catalog as an empty array until tags are applied. */
  tags: string[];
}

/** PATCH /api/v1/files/{id}; omitted fields are untouched. */
export interface UpdateFileRequest {
  description?: string;
  captured_local?: string;
  gps_lat?: number;
  gps_lon?: number;
}

/** One entry of an EPUB's reading order. `path` is the archive ENTRY NAME,
 *  already resolved against the package document's directory, and is appended
 *  to the resource route verbatim. The server validates it again on the way
 *  back in -- a path component from a URL never gets to skip that. */
export interface EpubSection {
  path: string;
  /** A human label when the book's guide supplies one, else "". */
  label: string;
}

/** GET /api/v1/files/{id}/epub */
export interface EpubSpine {
  title: string;
  sections: EpubSection[];
}

export interface FolderConfig {
  id: number;
  folder_label: string;
  display_name: string;
  syncthing_id: string;
  syncthing_path: string;
  budget_bytes: number;
  grace_minutes: number;
  enabled: boolean;
  present_in_toml: boolean;
  last_enforced_at: number | null;
}

/** PUT /api/v1/folders/{label} accepts only these UI-owned fields, all optional.
 *  syncthing_id and syncthing_path are TOML-owned and are ignored. */
export interface UpdateFolderRequest {
  budget_bytes?: number;
  grace_minutes?: number;
  enabled?: boolean;
  display_name?: string;
}

export interface RenamePathResponse extends JobCreated {
  warnings: string[];
}

export interface PersistentPath {
  id: number;
  folder_label: string;
  pattern: string;
  /** How many of the folder's on-device files this pattern currently
   *  protects. A freshly typed pattern showing 0 is the feedback that tells
   *  the owner it does nothing. */
  match_count: number;
}

export interface FolderCounts {
  total: number;
  synced: number;
  archived: number;
  missing: number;
  trashed: number;
  pinned: number;
}

export interface FolderStatus {
  label: string;
  display_name: string;
  /** On-device footprint: synced + archiving + restoring. pinned_bytes is a
   *  SUBSET of this, never an addend. */
  synced_size_bytes: number;
  budget_bytes: number;
  pinned_bytes: number;
  over_budget: boolean;
  wedged: boolean;
  grace_minutes: number;
  enabled: boolean;
  present_in_toml: boolean;
  last_enforced_at: number | null;
  /** "" when the Syncthing folder type has not been verified. */
  syncthing_type: string;
  counts: FolderCounts;
  /** Absent on a daemon that predates archive collections, which is "sync". */
  kind?: "sync" | "collection";
  /** Bytes held for a collection; absent for a sync folder. */
  stored_bytes?: number;
}

/** POST /api/v1/collections/{name}/trash */
export interface CollectionTrashResponse {
  updated: number;
}

/** StatusResponse.archive; null when the archive core is not open. */
export interface ArchiveStatus {
  blob_count: number;
  blob_bytes: number;
  bad_blob_count: number;
  journal_seq: number;
  scrub_running: boolean;
  scrub_last_finished_at: string | null;
}

/** POST/DELETE /api/v1/files/{id}/pin. `warning` is non-null iff the folder's
 *  pinned bytes exceed its budget AFTER the write -- a state test, not a
 *  transition test -- or the pinned file is in cold storage, where the pin is
 *  latent until a restore. The cold-storage wording distinguishes a file that
 *  was already archived when the request arrived from one the daemon archived
 *  during it; only the latter says the archive happened just now. The pin is
 *  applied either way. */
export interface PinResponse {
  file: FileRecord;
  folder: FolderStatus;
  warning: string | null;
}

/** POST /api/v1/files/pin */
export interface BulkPinRequest {
  ids: number[];
  pinned: boolean;
}

export interface BulkPinResponse {
  updated: number;
  /** One entry per folder the selection touched. */
  folders: FolderStatus[];
  warning: string | null;
}

/** One uniform tag change over a set of files. The server returns grouped
 * changes in TagUndo so heterogeneous starting states can be restored exactly. */
export interface TagFileMutation {
  ids: number[];
  add?: string[];
  remove?: string[];
}

export interface BulkTagUniformRequest extends TagFileMutation {
  changes?: never;
}

export interface BulkTagExactRequest {
  changes: TagFileMutation[];
  ids?: never;
  add?: never;
  remove?: never;
}

export type BulkTagRequest = BulkTagUniformRequest | BulkTagExactRequest;

/** Directly replay this as `api.bulkTags(result.undo)`. */
export interface TagUndo {
  changes: TagFileMutation[];
}

export interface BulkTagResponse {
  updated: number;
  created: string[];
  undo: TagUndo;
}

/** GET /api/v1/tags. `count` is exact assignments; `descendant_count`
 * includes assignments anywhere below this materialised-path node. */
export interface Tag {
  id: number;
  name: string;
  count: number;
  descendant_count: number;
}

export interface TagRenameResponse {
  merged: boolean;
  files_affected: number;
}

export interface TagDeleteResponse {
  deleted: number;
  files_affected: number;
}

export interface JobFailure {
  id: number;
  error: string;
}

export interface Job {
  id: number;
  action: JobAction;
  folder_label: string;
  state: JobState;
  total: number;
  done: number;
  /** The jobs.failed COLUMN, and authoritative. `failed` below is the detail
   *  array, which is truncated. */
  failed_count: number;
  /** The job-level reason a job was abandoned; "" on a job that ended
   *  normally, including one that ended with failed items. */
  error: string;
  created_at: number;
  started_at: number | null;
  finished_at: number | null;
}

/** GET /api/v1/jobs/{id}. `failed` is always an array, never null, and is
 *  truncated to the first 100 entries. */
export interface JobDetail extends Job {
  failed: JobFailure[];
}

/** POST /api/v1/jobs. `ids` is required and non-empty for restore and archive
 *  and ignored for scan; `folder` is only meaningful for scan ("" or absent
 *  scans every folder); `force` is only meaningful for restore and skips the
 *  budget pre-flight. */
export interface JobRequest {
  action: JobAction;
  ids?: number[];
  folder?: string;
  force?: boolean;
}

export interface JobCreated {
  job_id: number;
  action: JobAction;
  state: JobState;
  total: number;
}

/** The 409 body from POST /api/v1/jobs when a restore would push a folder past
 *  its budget. The daemon's answer to that is to evict a comparable volume of
 *  files the owner never selected, so the dialog has to be able to say how
 *  many.
 *
 *  `estimated_evictions` counts only files the offloader would really take:
 *  anything a persistent glob protects is excluded, because it is never
 *  evicted at any budget. It can legitimately be 0 -- the restore still
 *  exceeds the budget, but nothing is left that coldstore is allowed to
 *  archive, so the folder simply goes over and wedges. The dialog has to be
 *  able to say that too, rather than naming a file that cannot move. */
export interface BudgetPreflightError {
  error: string;
  folder: string;
  overage_bytes: number;
  estimated_evictions: number;
}

/** GET /api/v1/files/ids. `ids` is capped at 20000 -- the same cap POST /jobs
 *  enforces -- and `total` is the true match count. */
export interface FileIDsResponse {
  ids: number[];
  total: number;
  truncated: boolean;
}

/** GET /api/v1/events. `timestamp` is an RFC3339 string. */
export interface CatalogEvent {
  id: number;
  timestamp: string;
  event_type: string;
  folder: string;
  path: string;
  details: string;
}

/** POST /api/v1/files/{id}/resolve. "device" deletes the cold-storage copy and
 *  adopts the file on disk; "archive" renames the device file aside as
 *  <name>.coldstore-superseded and leaves the row archived. */
export interface ResolveConflictRequest {
  keep: "device" | "archive";
}

export interface StatusResponse {
  total_files: number;
  synced_files: number;
  archived_files: number;
  missing_files: number;
  trashed_files: number;
  folders: FolderStatus[];
  cold_storage_free_bytes: number;
  archive?: ArchiveStatus | null;
}

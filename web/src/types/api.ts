export type FileStatus =
  | "synced"
  | "archiving"
  | "archived"
  | "restoring"
  | "missing";

export type JobAction = "restore" | "archive" | "scan";
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
  status: FileStatus;
  thumbnail_path?: string;
  content_hash?: string;
  /** D2: a pinned file is never an offload candidate, whatever its age. */
  pinned: boolean;
  /** Epoch seconds, or null when the file is not pinned. */
  pinned_at: number | null;
  /** "restore" is the automatic pin a restore applies; "user" is explicit and
   *  is never downgraded to "restore". */
  pin_source: "" | "user" | "restore";
  thumb_attempts: number;
  thumb_last_attempt?: number;
  thumb_error?: string;
}

export interface FolderConfig {
  id: number;
  folder_label: string;
  syncthing_id: string;
  syncthing_path: string;
  budget_bytes: number;
  grace_minutes: number;
  enabled: boolean;
  present_in_toml: boolean;
  last_enforced_at: number | null;
}

/** PUT /api/v1/folders/{label} accepts only these three fields, all optional.
 *  syncthing_id and syncthing_path are TOML-owned and are ignored. */
export interface UpdateFolderRequest {
  budget_bytes?: number;
  grace_minutes?: number;
  enabled?: boolean;
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
  pinned: number;
}

export interface FolderStatus {
  label: string;
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
  folders: FolderStatus[];
  cold_storage_free_bytes: number;
}

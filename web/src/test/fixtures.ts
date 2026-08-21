import type { FileRecord, FolderConfig, FolderStatus, JobDetail, StatusResponse } from "../types/api";

let nextID = 1;

export function aFile(overrides: Partial<FileRecord> = {}): FileRecord {
  const id = overrides.id ?? nextID++;
  const name = overrides.filename ?? `IMG_${String(id).padStart(4, "0")}.jpg`;
  return {
    id,
    folder_label: "CameraRoll",
    relative_path: name,
    filename: name,
    extension: ".jpg",
    mime_type: "image/jpeg",
    size_bytes: 1_500_000,
    file_mtime: "2026-06-01T10:00:00Z",
    file_atime: "2026-06-01T10:00:00Z",
    first_seen: "2026-06-01T10:00:00Z",
    status: "synced",
    pinned: false,
    pinned_at: null,
    pin_source: "",
    thumb_attempts: 0,
    ...overrides,
  };
}

export function aFolderStatus(overrides: Partial<FolderStatus> = {}): FolderStatus {
  return {
    label: "CameraRoll",
    synced_size_bytes: 10_000_000_000,
    budget_bytes: 20_000_000_000,
    pinned_bytes: 2_000_000_000,
    over_budget: false,
    wedged: false,
    grace_minutes: 30,
    enabled: true,
    present_in_toml: true,
    last_enforced_at: null,
    syncthing_type: "",
    counts: { total: 100, synced: 80, archived: 18, missing: 2, pinned: 10 },
    ...overrides,
  };
}

export function aStatus(overrides: Partial<StatusResponse> = {}): StatusResponse {
  return {
    total_files: 100,
    synced_files: 80,
    archived_files: 18,
    missing_files: 2,
    cold_storage_free_bytes: 500_000_000_000,
    folders: [aFolderStatus()],
    ...overrides,
  };
}

export function aFolderConfig(overrides: Partial<FolderConfig> = {}): FolderConfig {
  return {
    id: 1,
    folder_label: "CameraRoll",
    syncthing_id: "abcde-fghij",
    syncthing_path: "/home/tjmisko/CameraRoll",
    budget_bytes: 20_000_000_000,
    grace_minutes: 30,
    enabled: true,
    present_in_toml: true,
    last_enforced_at: null,
    ...overrides,
  };
}

export function aJob(overrides: Partial<JobDetail> = {}): JobDetail {
  return {
    id: 7,
    action: "restore",
    folder_label: "CameraRoll",
    state: "running",
    total: 3,
    done: 1,
    failed_count: 0,
    error: "",
    created_at: 1_770_000_000,
    started_at: 1_770_000_001,
    finished_at: null,
    failed: [],
    ...overrides,
  };
}

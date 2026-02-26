export type FileStatus = "synced" | "archived" | "restoring" | "missing";

export interface FileRecord {
  id: number;
  folder_label: string;
  relative_path: string;
  filename: string;
  extension: string;
  mime_type: string;
  size_bytes: number;
  file_mtime: string;
  first_seen: string;
  archived_at?: string;
  status: FileStatus;
  thumbnail_path?: string;
  content_hash?: string;
}

export interface FolderConfig {
  id: number;
  folder_label: string;
  syncthing_id: string;
  syncthing_path: string;
  budget_bytes: number;
  grace_minutes: number;
  enabled: boolean;
}

export interface PersistentPath {
  id: number;
  folder_label: string;
  pattern: string;
}

export interface FolderStatus {
  label: string;
  synced_size_bytes: number;
  budget_bytes: number;
}

export interface StatusResponse {
  total_files: number;
  synced_files: number;
  archived_files: number;
  missing_files: number;
  folders: FolderStatus[];
  cold_storage_free_bytes: number;
}

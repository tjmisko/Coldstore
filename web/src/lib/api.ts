import type {
  BulkPinResponse,
  BulkTagRequest,
  BulkTagResponse,
  CatalogEvent,
  EpubSpine,
  FileIDsResponse,
  FileRecord,
  FileSort,
  FileStatus,
  FolderConfig,
  JobCreated,
  JobDetail,
  JobRequest,
  PersistentPath,
  PinResponse,
  PinSource,
  StatusResponse,
  UpdateFolderRequest,
  UpdateFileRequest,
} from "../types/api";

/** ApiRequestError carries the status AND the decoded error body.
 *
 *  The body matters: POST /jobs answers a budget over-run with a 409 whose
 *  payload names the folder, the overage and how many other files coldstore
 *  would evict to make room. A dialog that cannot see those numbers can only
 *  ask "are you sure?", which is the question that gets clicked through. */
export class ApiRequestError extends Error {
  // Plain fields, not constructor parameter properties: tsconfig sets
  // erasableSyntaxOnly, so the type system may not emit runtime code.
  readonly status: number;
  readonly body: unknown;

  constructor(status: number, message: string, body?: unknown) {
    super(message);
    this.name = "ApiRequestError";
    this.status = status;
    this.body = body;
  }
}

/** apiFetch is the only place in the app that calls fetch.
 *
 *  The guarded JSON parse is not defensive habit. routes.go answers an
 *  unmatched METHOD with Go's plain-text 405 and, before the /api/ catch-all
 *  existed, answered an unmatched PATH with index.html. A bare res.json() on
 *  either throws a SyntaxError about markup, which is how a missing endpoint
 *  used to present as a blank page with a parser error in the console. */
export async function apiFetch<T>(url: string, init?: RequestInit): Promise<T> {
  const res = await fetch(url, init);

  if (!res.ok) {
    let message = `${res.status} ${res.statusText}`;
    let body: unknown;
    try {
      body = await res.json();
      const withError = body as { error?: unknown };
      if (typeof withError?.error === "string" && withError.error !== "") {
        message = withError.error;
      }
    } catch {
      /* not JSON: keep the status line */
    }
    throw new ApiRequestError(res.status, message, body);
  }

  if (res.status === 204) return undefined as T;
  return (await res.json()) as T;
}

/** errorMessage turns anything a rejected promise carries into a line a person
 *  can read. An AbortError is never one of those -- it means the caller moved
 *  on -- so callers check isAbortError first. */
export function errorMessage(err: unknown): string {
  if (err instanceof Error) return err.message;
  return String(err);
}

export function isAbortError(err: unknown): boolean {
  return err instanceof DOMException && err.name === "AbortError";
}

// ---------------------------------------------------------------------------
// query building

export interface FileFilters {
  folder?: string;
  status?: FileStatus;
  extension?: string;
  search?: string;
  pinned?: boolean;
  pinSource?: PinSource;
  /** Unresolved section 4.1b conflicts only. */
  conflict?: boolean;
  capturedFrom?: string;
  capturedTo?: string;
  /** Metadata was scanned successfully but contained no capture date. */
  undated?: boolean;
  near?: NearFilter;
}

export interface NearFilter {
  lat: number;
  lon: number;
  radiusKm: number;
}

/** fileFilterParams is shared by GET /files and GET /files/ids, which is the
 *  point: "select all in folder" must resolve the SAME set the grid is
 *  showing, or the job acts on files the owner never saw. */
export function fileFilterParams(filters: FileFilters): URLSearchParams {
  const params = new URLSearchParams();
  if (filters.folder) params.set("folder", filters.folder);
  if (filters.status) params.set("status", filters.status);
  if (filters.extension) params.set("ext", filters.extension);
  if (filters.search) params.set("q", filters.search);
  if (filters.pinned !== undefined) params.set("pinned", filters.pinned ? "1" : "0");
  if (filters.pinSource) params.set("pin_source", filters.pinSource);
  if (filters.conflict) params.set("conflict", "1");
  if (filters.capturedFrom) params.set("captured_from", filters.capturedFrom);
  if (filters.capturedTo) params.set("captured_to", filters.capturedTo);
  if (filters.undated) params.set("undated", "1");
  if (filters.near) {
    params.set("near", `${filters.near.lat},${filters.near.lon},${filters.near.radiusKm}`);
  }
  return params;
}

export function filesUrl(
  filters: FileFilters,
  page: { offset: number; limit: number; sort?: FileSort },
): string {
  const params = fileFilterParams(filters);
  if (page.sort) params.set("sort", page.sort);
  params.set("offset", String(page.offset));
  params.set("limit", String(page.limit));
  return `/api/v1/files?${params}`;
}

// ---------------------------------------------------------------------------
// endpoints

export const api = {
  status: () => apiFetch<StatusResponse>("/api/v1/status"),

  files: (filters: FileFilters, page: { offset: number; limit: number; sort?: FileSort },
    init?: RequestInit) => apiFetch<FileRecord[]>(filesUrl(filters, page), init),

  /** filesByIDs backs post-job reconciliation. The server caps `ids` at 500,
   *  so callers chunk; it ignores every other filter, and returns exactly the
   *  ids that still exist. */
  filesByIDs: (ids: number[]) =>
    apiFetch<FileRecord[]>(`/api/v1/files?ids=${ids.join(",")}`),

  fileIDs: (filters: FileFilters) =>
    apiFetch<FileIDsResponse>(`/api/v1/files/ids?${fileFilterParams(filters)}`),

  downloadUrl: (id: number) => `/api/v1/files/${id}/download`,

  /** The INLINE route. downloadUrl sets Content-Disposition: attachment
   *  unconditionally, so pointing a reader or an iframe at it makes the
   *  browser save the file instead of showing it. This one states an explicit
   *  Content-Type and sends nosniff. Archived files stream straight out of
   *  cold storage; previewing one does not restore it. */
  previewUrl: (id: number) => `/api/v1/files/${id}/preview`,

  thumbnailUrl: (thumbnailPath: string) => `/api/v1/thumbnails/${thumbnailPath}`,

  /** The reading order of an EPUB, extracted server-side by the same parser
   *  that finds its cover. Nothing is unzipped in the browser. */
  epubSpine: (id: number, init?: RequestInit) =>
    apiFetch<EpubSpine>(`/api/v1/files/${id}/epub`, init),

  /** One resource out of an EPUB. `resource` is an archive entry name, encoded
   *  segment by segment so a "/" inside it stays a path separator and
   *  everything else -- spaces, "#", "?" -- does not change what is asked
   *  for. The server validates the decoded name against the archive root. */
  epubResourceUrl: (id: number, resource: string) =>
    `/api/v1/files/${id}/epub/${resource.split("/").map(encodeURIComponent).join("/")}`,

  pin: (id: number, pinned: boolean) =>
    apiFetch<PinResponse>(`/api/v1/files/${id}/pin`, { method: pinned ? "POST" : "DELETE" }),

  bulkPin: (ids: number[], pinned: boolean) =>
    apiFetch<BulkPinResponse>("/api/v1/files/pin", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ ids, pinned }),
    }),

  bulkTags: (request: BulkTagRequest) =>
    apiFetch<BulkTagResponse>("/api/v1/files/tags", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(request),
    }),

  restore: (id: number) =>
    apiFetch<FileRecord>(`/api/v1/files/${id}/restore`, { method: "POST" }),

  archive: (id: number) =>
    apiFetch<FileRecord>(`/api/v1/files/${id}/archive`, { method: "POST" }),

  updateFile: (id: number, patch: UpdateFileRequest) =>
    apiFetch<FileRecord>(`/api/v1/files/${id}`, {
      method: "PATCH",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(patch),
    }),

  resolve: (id: number, keep: "device" | "archive") =>
    apiFetch<FileRecord>(`/api/v1/files/${id}/resolve`, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ keep }),
    }),

  folders: () => apiFetch<FolderConfig[]>("/api/v1/folders"),

  updateFolder: (label: string, patch: UpdateFolderRequest) =>
    apiFetch<FolderConfig>(`/api/v1/folders/${encodeURIComponent(label)}`, {
      method: "PUT",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(patch),
    }),

  persistentPaths: (label: string) =>
    apiFetch<PersistentPath[]>(`/api/v1/folders/${encodeURIComponent(label)}/persistent`),

  addPersistentPath: (label: string, pattern: string) =>
    apiFetch<{ status: string }>(`/api/v1/folders/${encodeURIComponent(label)}/persistent`, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ pattern }),
    }),

  deletePersistentPath: (label: string, id: number) =>
    apiFetch<{ status: string }>(
      `/api/v1/folders/${encodeURIComponent(label)}/persistent/${id}`,
      { method: "DELETE" },
    ),

  createJob: (req: JobRequest) =>
    apiFetch<JobCreated>("/api/v1/jobs", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(req),
    }),

  job: (id: number) => apiFetch<JobDetail>(`/api/v1/jobs/${id}`),

  events: (params: { folder?: string; type?: string; warningsOnly?: boolean; limit?: number }) => {
    const search = new URLSearchParams();
    if (params.folder) search.set("folder", params.folder);
    if (params.type) search.set("type", params.type);
    if (params.warningsOnly) search.set("class", "warning");
    if (params.limit) search.set("limit", String(params.limit));
    return apiFetch<CatalogEvent[]>(`/api/v1/events?${search}`);
  },
};

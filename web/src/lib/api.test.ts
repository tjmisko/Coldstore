import { afterEach, describe, expect, it } from "vitest";
import { ApiRequestError, api, apiFetch, fileFilterParams, filesUrl } from "./api";
import { stubFetch } from "../test/harness";

let stub: ReturnType<typeof stubFetch> | null = null;

afterEach(() => {
  stub?.restore();
  stub = null;
});

describe("apiFetch", () => {
  it("should surface the daemon's own message when it answers with a JSON error", async () => {
    stub = stubFetch([{ url: "/api/v1/files", status: 400, body: { error: "invalid limit" } }]);
    await expect(apiFetch("/api/v1/files?limit=0")).rejects.toThrow("invalid limit");
  });

  it("should surface the status line when the body is not JSON", async () => {
    // An unmatched METHOD gets Go's plain-text 405. A bare res.json() on that
    // throws a SyntaxError about markup, which is the blank-page failure mode.
    stub = stubFetch([]);
    stub.route({
      url: "/api/v1/files",
      status: 405,
      get body(): never {
        throw new SyntaxError("Unexpected token M");
      },
    });
    await expect(apiFetch("/api/v1/files")).rejects.toThrow("405");
  });

  it("should carry the 409 body so the confirm dialog can name the numbers", async () => {
    stub = stubFetch([
      {
        method: "POST",
        url: "/api/v1/jobs",
        status: 409,
        body: {
          error: "restoring these files would exceed the folder budget",
          folder: "CameraRoll",
          overage_bytes: 15_000_000_000,
          estimated_evictions: 2500,
        },
      },
    ]);

    const err = await apiFetch("/api/v1/jobs", { method: "POST" }).catch((e: unknown) => e);
    expect(err).toBeInstanceOf(ApiRequestError);
    const apiErr = err as ApiRequestError;
    expect(apiErr.status).toBe(409);
    expect(apiErr.body).toMatchObject({ estimated_evictions: 2500, folder: "CameraRoll" });
  });
});

describe("file query building", () => {
  it("should send a filter only when it is set", () => {
    expect(fileFilterParams({ folder: "CameraRoll" }).toString()).toBe("folder=CameraRoll");
  });

  it("should translate the pin and conflict filters into the parameters the API defines", () => {
    const params = fileFilterParams({
      folder: "CameraRoll",
      status: "archived",
      pinned: true,
      pinSource: "restore",
      conflict: true,
    });
    expect(params.get("pinned")).toBe("1");
    expect(params.get("pin_source")).toBe("restore");
    expect(params.get("conflict")).toBe("1");
    expect(params.get("status")).toBe("archived");
  });

  it("should share capture bounds and undated with listing and id selection", () => {
    const filters = {
      folder: "CameraRoll",
      capturedFrom: "2024-01-02",
      capturedTo: "2024-03-04",
      undated: true,
    };
    const params = fileFilterParams(filters);
    expect(params.get("captured_from")).toBe("2024-01-02");
    expect(params.get("captured_to")).toBe("2024-03-04");
    expect(params.get("undated")).toBe("1");
    expect(filesUrl(filters, { offset: 0, limit: 60, sort: "captured" }))
      .toContain("sort=captured");
  });

  it("should distinguish pinned=0 from no pin filter at all", () => {
    expect(fileFilterParams({ pinned: false }).get("pinned")).toBe("0");
    expect(fileFilterParams({}).has("pinned")).toBe(false);
  });

  it("should carry sort and pagination on the listing endpoint", () => {
    const url = filesUrl({ folder: "CameraRoll", pinned: true, pinSource: "restore" }, {
      offset: 60,
      limit: 60,
      sort: "pinned_at",
    });
    expect(url).toContain("sort=pinned_at");
    expect(url).toContain("offset=60");
    expect(url).toContain("limit=60");
  });
});

describe("file metadata", () => {
  it("should PATCH only the owner-authored fields supplied", async () => {
    stub = stubFetch([{ method: "PATCH", url: "/api/v1/files/7", body: {} }]);
    await api.updateFile(7, { description: "A caption" });
    expect(stub.calls).toEqual([
      { url: "/api/v1/files/7", method: "PATCH", body: { description: "A caption" } },
    ]);
  });

  it("should send a manual capture date through the file PATCH endpoint", async () => {
    stub = stubFetch([{ method: "PATCH", url: "/api/v1/files/7", body: {} }]);
    await api.updateFile(7, { captured_local: "2024-02-29 03:04:05.006" });
    expect(stub.calls[0]).toEqual({
      url: "/api/v1/files/7",
      method: "PATCH",
      body: { captured_local: "2024-02-29 03:04:05.006" },
    });
  });
});

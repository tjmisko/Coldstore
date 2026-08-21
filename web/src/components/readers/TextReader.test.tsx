import { render, screen, waitFor } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import TextReader from "./TextReader";

const original = globalThis.fetch;

afterEach(() => {
  globalThis.fetch = original;
});

/** A fetch stub that answers with a body and headers, so the truncation
 *  header the daemon sets can actually be exercised. */
function stubText(body: string, headers: Record<string, string> = {}) {
  const calls: string[] = [];
  globalThis.fetch = (async (input: RequestInfo | URL) => {
    calls.push(String(input));
    return {
      ok: true,
      status: 200,
      headers: new Headers(headers),
      text: async () => body,
    } as Response;
  }) as typeof fetch;
  return calls;
}

describe("TextReader", () => {
  it("should render the file's text when the preview loads", async () => {
    stubText("first line\nsecond line\n");

    render(<TextReader src="/api/v1/files/9/preview" filename="bookmarks" />);

    await waitFor(() => {
      expect(document.querySelector("[data-text-body]")).not.toBeNull();
    });
    expect(document.querySelector("[data-text-body]")!.textContent).toContain("second line");
  });

  it("should read from the inline preview route, not the download route", async () => {
    // The download route sets Content-Disposition: attachment.
    const calls = stubText("body");

    render(<TextReader src="/api/v1/files/9/preview" filename="notes.txt" />);

    await waitFor(() => expect(calls.length).toBe(1));
    expect(calls[0]).toBe("/api/v1/files/9/preview");
  });

  it("should say the preview is partial when the server truncated it", async () => {
    // Otherwise the owner reads the first 512 kB of a log as the whole file.
    stubText("a".repeat(100), { "X-Coldstore-Truncated": "true" });

    render(<TextReader src="/api/v1/files/9/preview" filename="huge.log" />);

    await waitFor(() => {
      expect(document.querySelector("[data-text-truncated]")).not.toBeNull();
    });
  });

  it("should not claim truncation when the whole file arrived", async () => {
    stubText("short", { "X-Coldstore-Truncated": "false" });

    render(<TextReader src="/api/v1/files/9/preview" filename="notes.txt" />);

    await waitFor(() => {
      expect(document.querySelector("[data-text-body]")).not.toBeNull();
    });
    expect(document.querySelector("[data-text-truncated]")).toBeNull();
  });

  it("should report the failure when the preview cannot be loaded", async () => {
    globalThis.fetch = (async () =>
      ({ ok: false, status: 415, headers: new Headers(), text: async () => "" }) as Response) as typeof fetch;

    render(<TextReader src="/api/v1/files/9/preview" filename="firmware.bin" />);

    await waitFor(() => {
      expect(document.querySelector("[data-text-error]")).not.toBeNull();
    });
    expect(screen.getByText("firmware.bin")).toBeInTheDocument();
  });

  it("should abort the request when the file changes before it lands", async () => {
    // Arrowing through a folder must not leave one in-flight fetch per file.
    const abort = vi.fn();
    const originalAbortController = globalThis.AbortController;
    class SpyController extends originalAbortController {
      abort(reason?: unknown) {
        abort();
        super.abort(reason);
      }
    }
    globalThis.AbortController = SpyController as typeof AbortController;

    try {
      stubText("body");
      const view = render(<TextReader src="/api/v1/files/1/preview" filename="a.txt" />);
      view.rerender(<TextReader src="/api/v1/files/2/preview" filename="b.txt" />);
      expect(abort).toHaveBeenCalled();
    } finally {
      globalThis.AbortController = originalAbortController;
    }
  });
});

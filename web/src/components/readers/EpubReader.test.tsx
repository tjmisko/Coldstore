import { render, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it } from "vitest";
import { stubFetch } from "../../test/harness";
import EpubReader from "./EpubReader";

let stub: ReturnType<typeof stubFetch> | null = null;

afterEach(() => {
  stub?.restore();
  stub = null;
});

const SPINE = {
  title: "The Pocket Guide",
  sections: [
    { path: "OEBPS/text/ch1.xhtml", label: "Chapter One" },
    { path: "OEBPS/text/ch2.xhtml", label: "" },
  ],
};

function renderReader(body: unknown = SPINE, status = 200) {
  stub = stubFetch([{ url: "/api/v1/files/9/epub", body, status }]);
  return render(<EpubReader fileId={9} filename="guide.epub" />);
}

describe("EpubReader", () => {
  it("should render the first section in an iframe when the spine loads", async () => {
    renderReader();

    await waitFor(() => {
      expect(document.querySelector("[data-epub-frame]")).not.toBeNull();
    });
    const frame = document.querySelector("[data-epub-frame]") as HTMLIFrameElement;
    expect(frame.getAttribute("src")).toBe("/api/v1/files/9/epub/OEBPS/text/ch1.xhtml");
  });

  it("should sandbox the iframe to an opaque origin when rendering book markup", async () => {
    // The markup inside arrived over Syncthing. An empty sandbox attribute
    // means no scripts and no same-origin access, so it cannot reach this
    // page, its storage or its cookies.
    renderReader();

    await waitFor(() => {
      expect(document.querySelector("[data-epub-frame]")).not.toBeNull();
    });
    const frame = document.querySelector("[data-epub-frame]") as HTMLIFrameElement;
    expect(frame.getAttribute("sandbox")).toBe("");
  });

  it("should move to the next section when the reader asks for it", async () => {
    renderReader();
    await waitFor(() => {
      expect(document.querySelector("[data-epub-frame]")).not.toBeNull();
    });

    await userEvent.click(document.querySelector("[data-epub-next]") as HTMLElement);

    const frame = document.querySelector("[data-epub-frame]") as HTMLIFrameElement;
    expect(frame.getAttribute("src")).toBe("/api/v1/files/9/epub/OEBPS/text/ch2.xhtml");
  });

  it("should label a section with the book's own title for it when one exists", async () => {
    renderReader();

    await waitFor(() => {
      expect(document.querySelector("[data-epub-section-indicator]")).not.toBeNull();
    });
    expect(document.querySelector("[data-epub-section-indicator]")!.textContent).toContain(
      "Chapter One",
    );
  });

  it("should encode a section path when it contains characters a URL would eat", async () => {
    // Book entry names come from whatever produced the archive, spaces and
    // hashes included, and the path is a URL component.
    stub = stubFetch([
      {
        url: "/api/v1/files/9/epub",
        body: { title: "T", sections: [{ path: "OEBPS/My Text/a b#1.xhtml", label: "" }] },
      },
    ]);
    render(<EpubReader fileId={9} filename="guide.epub" />);

    await waitFor(() => {
      expect(document.querySelector("[data-epub-frame]")).not.toBeNull();
    });
    const src = document.querySelector("[data-epub-frame]")!.getAttribute("src")!;
    expect(src).toBe("/api/v1/files/9/epub/OEBPS/My%20Text/a%20b%231.xhtml");
    // The slashes must survive as separators; everything else must not.
    expect(src).not.toContain("#1.xhtml");
  });

  it("should say the book has no readable sections when the spine is empty", async () => {
    renderReader({ title: "Empty", sections: [] });

    await waitFor(() => {
      expect(document.querySelector("[data-epub-empty]")).not.toBeNull();
    });
  });

  it("should report the failure when the book cannot be opened", async () => {
    renderReader({ error: "this epub could not be read" }, 422);

    await waitFor(() => {
      expect(document.querySelector("[data-epub-error]")).not.toBeNull();
    });
  });
});

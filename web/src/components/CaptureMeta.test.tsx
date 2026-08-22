import { render, screen } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import { aFile } from "../test/fixtures";
import { CaptureMeta } from "./CaptureMeta";

describe("CaptureMeta", () => {
  it("should distinguish an unknown offset from the real zero offset", () => {
    const { rerender } = render(
      <CaptureMeta file={aFile({ captured_local: "2024-01-02 03:04:05", captured_offset_minutes: null })} />,
    );
    expect(screen.getByText("Timezone not recorded")).toBeInTheDocument();

    rerender(
      <CaptureMeta file={aFile({ captured_local: "2024-01-02 03:04:05", captured_offset_minutes: 0 })} />,
    );
    expect(screen.getByText("UTC+00:00")).toBeInTheDocument();
    expect(screen.queryByText("Timezone not recorded")).toBeNull();
  });

  it("should distinguish unscanned metadata from scanned metadata with no date", () => {
    const { rerender } = render(<CaptureMeta file={aFile({ meta_scanned_at: null })} />);
    expect(screen.getByText("Metadata not scanned yet")).toHaveAttribute("data-capture-date", "");

    rerender(<CaptureMeta file={aFile({ meta_scanned_at: 123 })} />);
    expect(screen.getByText("No capture date recorded")).toBeInTheDocument();
  });
});

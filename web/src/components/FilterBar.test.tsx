import { screen } from "@testing-library/react";
import { afterEach, describe, expect, it } from "vitest";
import { renderWithToasts, stubFetch } from "../test/harness";
import { FilterBar } from "./FilterBar";

let stub: ReturnType<typeof stubFetch> | null = null;

afterEach(() => {
  stub?.restore();
  stub = null;
});

describe("FilterBar", () => {
  it("should hide the capture and pinned filters when a collection is selected", () => {
    stub = stubFetch([]);
    renderWithToasts(<FilterBar active="all" onChange={() => {}} folder="Papers" collection />);

    for (const key of ["pinned-user", "pinned-auto", "undated", "conflicts", "synced"]) {
      expect(document.querySelector(`[data-filter="${key}"]`)).toBeNull();
    }
    for (const key of ["all", "archived", "untagged", "trash"]) {
      expect(document.querySelector(`[data-filter="${key}"]`)).not.toBeNull();
    }
    expect(screen.getByRole("tab", { name: "Trash" })).toBeInTheDocument();
  });

  it("should offer every filter when the folder is not a collection", () => {
    stub = stubFetch([]);
    renderWithToasts(<FilterBar active="all" onChange={() => {}} folder="CameraRoll" />);

    expect(document.querySelectorAll("[data-filter]").length).toBeGreaterThanOrEqual(9);
    expect(document.querySelector('[data-filter="undated"]')).not.toBeNull();
  });
});

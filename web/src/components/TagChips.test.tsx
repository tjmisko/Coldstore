import { render, screen } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import { TagChips } from "./TagChips";

describe("TagChips", () => {
  it("shows two tile chips and an honest overflow count", () => {
    render(<TagChips tags={["Trips", "Trips/Iceland", "Favorites", "2024"]} />);

    expect(document.querySelector('[data-tag-chip="Trips"]')).not.toBeNull();
    expect(document.querySelector('[data-tag-chip="Trips/Iceland"]')).not.toBeNull();
    expect(document.querySelector('[data-tag-chip="Favorites"]')).toBeNull();
    expect(screen.getByText("+2")).toHaveAttribute("title", "Favorites, 2024");
  });
});

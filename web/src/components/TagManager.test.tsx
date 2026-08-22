import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it } from "vitest";
import { renderWithToasts, stubFetch } from "../test/harness";
import { TagManager } from "./TagManager";

let stub: ReturnType<typeof stubFetch> | null = null;

afterEach(() => {
  stub?.restore();
  stub = null;
  localStorage.clear();
});

const TAGS = [
  { id: 1, name: "Trips", count: 0, descendant_count: 4 },
  { id: 2, name: "Trips/Icland", count: 1, descendant_count: 1 },
  { id: 3, name: "Trips/Iceland", count: 3, descendant_count: 3 },
];

function row(name: string): HTMLElement {
  return document.querySelector(`[data-managed-tag="${name}"]`) as HTMLElement;
}

describe("TagManager", () => {
  it("does not expose the removed Space-leader quick slots", async () => {
    stub = stubFetch([{ url: "/api/v1/tags", body: TAGS }]);
    renderWithToasts(<TagManager folder="CameraRoll" />);

    expect(await screen.findByText("Iceland")).toBeInTheDocument();
    expect(screen.queryByLabelText("Quick tag 1")).toBeNull();
  });

  it("makes an existing-name rename an explicit merge and reports affected files", async () => {
    localStorage.setItem("coldstore.quick-tags.CameraRoll", JSON.stringify(["Trips/Icland", "", "", "", "", "", "", "", ""]));
    localStorage.setItem("coldstore.quick-tags.Books", JSON.stringify(["Trips/Icland/Edited", "", "", "", "", "", "", "", ""]));
    stub = stubFetch([
      { url: "/api/v1/tags", body: TAGS },
      { method: "PUT", url: "/api/v1/tags/Trips%2FIcland", body: { merged: true, files_affected: 1 } },
    ]);
    const { toasts } = renderWithToasts(<TagManager folder="CameraRoll" />);
    await screen.findByText("Icland");

    await userEvent.click(within(row("Trips/Icland")).getByRole("button", { name: "Rename" }));
    const input = screen.getByLabelText("Rename or merge Trips/Icland");
    await userEvent.clear(input);
    await userEvent.type(input, "Trips/Iceland");
    expect(screen.getByText(/will merge into existing tag Trips\/Iceland/)).toBeInTheDocument();
    await userEvent.click(screen.getByRole("button", { name: "Merge" }));

    await waitFor(() => expect(stub!.calls.find((call) => call.method === "PUT")?.body)
      .toEqual({ name: "Trips/Iceland" }));
    expect(toasts.messages().join(" ")).toContain("Merged Trips/Icland; 1 file kept its tag assignments");
    expect(JSON.parse(localStorage.getItem("coldstore.quick-tags.CameraRoll") ?? "[]")[0]).toBe("Trips/Iceland");
    expect(JSON.parse(localStorage.getItem("coldstore.quick-tags.Books") ?? "[]")[0]).toBe("Trips/Iceland/Edited");
  });

  it("names cascade consequences before deleting a whole subtree", async () => {
    localStorage.setItem("coldstore.quick-tags.CameraRoll", JSON.stringify(["Trips", "Trips/Iceland", "Keep", "", "", "", "", "", ""]));
    localStorage.setItem("coldstore.quick-tags.Books", JSON.stringify(["Trips/Icland", "", "", "", "", "", "", "", ""]));
    stub = stubFetch([
      { url: "/api/v1/tags", body: TAGS },
      { method: "DELETE", url: "/api/v1/tags/Trips?cascade=1", body: { deleted: 3, files_affected: 4 } },
    ]);
    renderWithToasts(<TagManager folder="CameraRoll" />);
    await screen.findByText("Trips");

    await userEvent.click(within(row("Trips")).getByRole("button", { name: "Delete" }));
    expect(screen.getByRole("heading", { name: "Delete Trips and its descendants?" })).toBeInTheDocument();
    expect(screen.getByText(/every descendant and removes assignments from 4 distinct files/)).toBeInTheDocument();
    await userEvent.click(screen.getByRole("button", { name: "Delete entire subtree" }));

    await waitFor(() => expect(stub!.calls.some(
      (call) => call.method === "DELETE" && call.url === "/api/v1/tags/Trips?cascade=1",
    )).toBe(true));
    expect(JSON.parse(localStorage.getItem("coldstore.quick-tags.CameraRoll") ?? "[]").slice(0, 3)).toEqual(["", "", "Keep"]);
    expect(JSON.parse(localStorage.getItem("coldstore.quick-tags.Books") ?? "[]")[0]).toBe("");
  });
});

import { screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it, vi } from "vitest";
import type { FileActions } from "../hooks/useFileWorkspace";
import { aFile } from "../test/fixtures";
import { renderWithToasts } from "../test/harness";
import { KeymapProvider } from "./KeymapProvider";
import { LocationEditor } from "./LocationEditor";

function locationActions(): FileActions {
  return {
    setPinned: vi.fn(async () => {}),
    restore: vi.fn(async () => {}),
    archive: vi.fn(async () => {}),
    resolve: vi.fn(async () => {}),
    download: vi.fn(),
    setDescription: vi.fn(async () => true),
    setCaptureDate: vi.fn(async () => true),
    setLocation: vi.fn(async () => true),
  };
}

function renderEditor(file = aFile({ gps_lat: 0, gps_lon: 0 }), actions = locationActions()) {
  const onFindNearby = vi.fn();
  const onClose = vi.fn();
  const view = renderWithToasts(
    <KeymapProvider>
      <LocationEditor
        file={file}
        actions={actions}
        onFindNearby={onFindNearby}
        onClose={onClose}
      />
    </KeymapProvider>,
  );
  return { ...view, file, actions, onFindNearby, onClose };
}

describe("LocationEditor", () => {
  it("should preserve and save the valid 0,0 coordinate with mouse controls", async () => {
    const { actions, file, onClose } = renderEditor();
    expect(screen.getByRole("textbox", { name: "Latitude" })).toHaveValue("0");
    expect(screen.getByRole("textbox", { name: "Longitude" })).toHaveValue("0");

    await userEvent.click(screen.getByRole("button", { name: "Save coordinates" }));

    expect(actions.setLocation).toHaveBeenCalledWith(file, 0, 0);
    expect(onClose).toHaveBeenCalledOnce();
  });

  it("should create a nearby filter without contacting a map service", async () => {
    const { onFindNearby } = renderEditor();
    const radius = screen.getByRole("textbox", { name: "Nearby radius in kilometres" });
    await userEvent.clear(radius);
    await userEvent.type(radius, "10");
    await userEvent.click(screen.getByRole("button", { name: "Show nearby" }));

    expect(onFindNearby).toHaveBeenCalledWith({ lat: 0, lon: 0, radiusKm: 10 });
  });

  it("should explain invalid finite and range values instead of saving", async () => {
    const { actions } = renderEditor(aFile({ gps_lat: null, gps_lon: null }));
    await userEvent.type(screen.getByRole("textbox", { name: "Latitude" }), "91");
    await userEvent.type(screen.getByRole("textbox", { name: "Longitude" }), "0");
    await userEvent.click(screen.getByRole("button", { name: "Save coordinates" }));

    expect(screen.getByRole("alert")).toHaveTextContent("Latitude must be between -90 and 90");
    expect(actions.setLocation).not.toHaveBeenCalled();
  });
});

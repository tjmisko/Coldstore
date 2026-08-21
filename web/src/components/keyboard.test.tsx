import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, beforeEach, describe, expect, it } from "vitest";
import App from "../App";
import { parseSequence } from "../keymap/keys";
import { aFile, aJob, aStatus } from "../test/fixtures";
import { renderWithToasts, stubFetch } from "../test/harness";
import { FileBrowser } from "./FileBrowser";
import { Gallery } from "./Gallery";
import { KeymapProvider } from "./KeymapProvider";

let stub: ReturnType<typeof stubFetch> | null = null;

afterEach(() => {
  stub?.restore();
  stub = null;
});

beforeEach(() => {
  // The gallery derives its column count from the window width the way its
  // Tailwind classes do, and a motion engine that thinks the grid is 4 wide
  // when it is 6 sends `j` two tiles sideways. Pin it so the arithmetic in
  // these tests is the arithmetic in the app.
  Object.defineProperty(window, "innerWidth", { value: 1024, configurable: true });
  Object.defineProperty(window, "innerHeight", { value: 768, configurable: true });
});

const NAMED: Record<string, string> = {
  Esc: "Escape",
  CR: "Enter",
  Space: " ",
  Up: "ArrowUp",
  Down: "ArrowDown",
  Left: "ArrowLeft",
  Right: "ArrowRight",
  Menu: "ContextMenu",
};

/** Type a vim sequence at the window, spelled the way the binding table spells
 *  one: type("d3j"), type("ggVG"), type("<C-d>"). */
function type(written: string, target: Window | Element = window) {
  for (const token of parseSequence(written)) {
    fireEvent.keyDown(target, keyInit(token));
  }
}

function keyInit(token: string): KeyboardEventInit {
  if (!token.startsWith("<")) return { key: token, shiftKey: token !== token.toLowerCase() };
  const parts = token.slice(1, -1).split("-");
  const base = parts.pop() ?? "";
  const init: KeyboardEventInit = { key: NAMED[base] ?? base };
  for (const modifier of parts) {
    if (modifier === "C") init.ctrlKey = true;
    if (modifier === "D") init.metaKey = true;
    if (modifier === "A") init.altKey = true;
    if (modifier === "S") init.shiftKey = true;
  }
  return init;
}

const FILES = Array.from({ length: 12 }, (_, i) =>
  aFile({ id: i + 1, filename: `file_${String(i + 1).padStart(2, "0")}.jpg` }),
);

function row(id: number): HTMLElement {
  return document.querySelector(`tr[data-file-id="${id}"]`) as HTMLElement;
}

/** The fixture's relative_path is its filename, so findByText matches the Name
 *  cell and the Path cell both. Wait on the row instead. */
async function browserReady() {
  await waitFor(() => expect(row(1)).not.toBeNull());
}

function tile(id: number): HTMLElement {
  return document.querySelector(`[data-cursor-scope="gallery"][data-file-id="${id}"]`) as HTMLElement;
}

function renderBrowser(files = FILES) {
  stub = stubFetch([{ url: "/api/v1/files", body: files }]);
  return renderWithToasts(
    <KeymapProvider>
      <FileBrowser
        folder="CameraRoll"
        filterKey="all"
        onFilterChange={() => {}}
        onFolderChanged={() => {}}
      />
    </KeymapProvider>,
  );
}

function renderGallery(files = FILES) {
  stub = stubFetch([{ url: "/api/v1/files", body: files }]);
  return renderWithToasts(
    <KeymapProvider>
      <Gallery
        folder="CameraRoll"
        filterKey="all"
        onFilterChange={() => {}}
        onFolderChanged={() => {}}
      />
    </KeymapProvider>,
  );
}

function stubArchiveJob() {
  stub!.route({
    method: "POST",
    url: "/api/v1/jobs",
    status: 202,
    body: { job_id: 9, action: "archive", state: "pending", total: 1 },
  });
  stub!.route({ url: "/api/v1/jobs/9", body: aJob({ id: 9, action: "archive", state: "done" }) });
}

function postedJob() {
  return stub!.calls.find((call) => call.method === "POST" && call.url === "/api/v1/jobs")?.body as
    | { action: string; ids: number[]; force: boolean }
    | undefined;
}

describe("motions", () => {
  it("should handle a key sent as soon as the first fetched rows commit", async () => {
    renderBrowser();

    const body = screen.getByRole("table").querySelector("tbody");
    expect(body).not.toBeNull();

    // MutationObserver runs after React commits the rows but before passive
    // effects. This is the browser-visible interval in which the old keymap
    // lifecycle still exposed the handler from the empty-list render.
    await new Promise<void>((resolve) => {
      const observer = new MutationObserver(() => {
        if (row(1) === null) return;
        observer.disconnect();
        type("j");
        resolve();
      });
      observer.observe(body!, { childList: true });
    });

    expect(row(2).dataset.cursor).toBe("true");
    expect(row(1).dataset.cursor).toBe("false");
  });

  it("should move the cursor one row down when j is pressed in the table", async () => {
    renderBrowser();
    await browserReady();

    type("j");

    expect(row(2).dataset.cursor).toBe("true");
    expect(row(1).dataset.cursor).toBe("false");
  });

  it("should move the cursor by the count when 3j is pressed", async () => {
    renderBrowser();
    await browserReady();

    type("3j");

    expect(row(4).dataset.cursor).toBe("true");
  });

  it("should move the cursor a whole grid row when j is pressed in the gallery", async () => {
    // Five columns at 1024px, so `j` is five tiles, not one. A table is the
    // same engine with columns=1, which is why both work from one motion
    // module.
    renderGallery();
    await screen.findByLabelText("file_01.jpg");

    type("j");

    expect(tile(6).dataset.cursor).toBe("true");
  });

  it("should stay put rather than jumping sideways when j runs off the bottom", async () => {
    renderGallery();
    await screen.findByLabelText("file_01.jpg");

    type("jjjj");

    // 12 files, 5 wide: rows are 1-5, 6-10, 11-12. From tile 1, one j reaches
    // 6, a second reaches 11, and the rest have nowhere in column 0 to go.
    expect(tile(11).dataset.cursor).toBe("true");
  });

  it("should go to the last file on G and back to the first on gg", async () => {
    renderBrowser();
    await browserReady();

    type("G");
    expect(row(12).dataset.cursor).toBe("true");

    type("gg");
    expect(row(1).dataset.cursor).toBe("true");
  });

  it("should go to the numbered file when G carries a count", async () => {
    renderBrowser();
    await browserReady();

    type("7G");

    expect(row(7).dataset.cursor).toBe("true");
  });
});

describe("operators", () => {
  it("should archive only the file under the cursor when dd is typed", async () => {
    renderBrowser();
    await browserReady();
    stubArchiveJob();

    type("jdd");

    await waitFor(() => expect(postedJob()).toBeDefined());
    expect(postedJob()).toMatchObject({ action: "archive", ids: [2] });
  });

  it("should archive the cursor file and the three below it when d3j is typed", async () => {
    // Inclusive at both ends: an exclusive range archives three of the four
    // files the eye counted.
    renderBrowser();
    await browserReady();
    stubArchiveJob();

    type("d3j");

    await waitFor(() => expect(postedJob()).toBeDefined());
    expect(postedJob()).toMatchObject({ action: "archive", ids: [1, 2, 3, 4] });
  });

  it("should multiply the counts when 2d3j is typed", async () => {
    renderBrowser();
    await browserReady();
    stubArchiveJob();

    type("2d3j");

    await waitFor(() => expect(postedJob()).toBeDefined());
    expect(postedJob()?.ids).toEqual([1, 2, 3, 4, 5, 6, 7]);
  });

  it("should restore rather than archive when the operator is r", async () => {
    renderBrowser();
    await browserReady();
    stub!.route({
      method: "POST",
      url: "/api/v1/jobs",
      status: 202,
      body: { job_id: 9, action: "restore", state: "pending", total: 1 },
    });
    stub!.route({ url: "/api/v1/jobs/9", body: aJob({ id: 9, state: "done" }) });

    type("rr");

    await waitFor(() => expect(postedJob()).toBeDefined());
    expect(postedJob()).toMatchObject({ action: "restore", ids: [1] });
  });

  it("should pin a whole selection in ONE request when p is typed", async () => {
    renderBrowser();
    await browserReady();
    stub!.route({
      method: "POST",
      url: "/api/v1/files/pin",
      body: { updated: 2, folders: [], warning: null },
    });

    type("xjx");
    expect(screen.getByText("2 selected")).toBeInTheDocument();

    type("pp");

    await waitFor(() =>
      expect(stub!.calls.some((c) => c.url === "/api/v1/files/pin")).toBe(true),
    );
    expect(stub!.calls.find((c) => c.url === "/api/v1/files/pin")?.body).toEqual({
      ids: [1, 2],
      pinned: true,
    });
  });

  it("should ask before archiving when the range is more than ten files", async () => {
    renderBrowser();
    await browserReady();
    stubArchiveJob();

    type("dG");

    // dG names no number and is one keystroke from dg. The bar's buttons sit
    // next to a visible count and do not need asking; this does.
    const dialog = await screen.findByRole("dialog");
    expect(dialog).toHaveTextContent("Archive 12 files?");
    expect(dialog).toHaveTextContent(/Syncthing will delete them/);
    expect(postedJob()).toBeUndefined();

    await userEvent.click(screen.getByRole("button", { name: "Yes, do it" }));

    await waitFor(() => expect(postedJob()).toBeDefined());
    expect(postedJob()?.ids).toHaveLength(12);
  });

  it("should cancel the whole operator when the key after it is not a range", async () => {
    renderBrowser();
    await browserReady();
    stubArchiveJob();

    type("dz");
    type("j");

    expect(postedJob()).toBeUndefined();
    // The j after the dead sequence is an ordinary motion again.
    expect(row(2).dataset.cursor).toBe("true");
  });
});

describe("visual mode", () => {
  it("should extend the selection over the motion and keep it after Escape", async () => {
    // In vim nothing stays selected after Escape. Here the point of a visual
    // range is to end up with a selection, and <Space>c is how one goes away.
    renderBrowser();
    await browserReady();

    type("v3j");
    expect(screen.getByText("4 selected")).toBeInTheDocument();

    type("<Esc>");
    expect(screen.getByText("4 selected")).toBeInTheDocument();
    for (const id of [1, 2, 3, 4]) expect(row(id).dataset.selected).toBe("true");
    expect(row(5).dataset.selected).toBe("false");
  });

  it("should shrink the selection again when the motion reverses", async () => {
    renderBrowser();
    await browserReady();

    type("v3j");
    type("k");

    expect(screen.getByText("3 selected")).toBeInTheDocument();
    expect(row(4).dataset.selected).toBe("false");
  });

  it("should select every file when ggVG is typed", async () => {
    renderGallery();
    await screen.findByLabelText("file_01.jpg");

    type("GggVG");

    expect(screen.getByText("12 selected")).toBeInTheDocument();
  });

  it("should archive the highlighted span when an operator runs in visual mode", async () => {
    renderBrowser();
    await browserReady();
    stubArchiveJob();

    type("v2jd");

    await waitFor(() => expect(postedJob()).toBeDefined());
    expect(postedJob()?.ids).toEqual([1, 2, 3]);
  });

  it("should clear the selection on leader-c", async () => {
    renderBrowser();
    await browserReady();

    type("v3j");
    type("<Space>c");

    await waitFor(() => expect(screen.queryByText("4 selected")).toBeNull());
  });
});

describe("the text-entry guard", () => {
  it("should leave every key alone when focus is in a text field", async () => {
    renderBrowser();
    await browserReady();
    stubArchiveJob();

    const input = document.createElement("input");
    document.body.appendChild(input);
    input.focus();
    type("dd", input);
    type("3j", input);

    // "20 GB" typed into a budget editor is 2, 0, G, B to the machine, and G is
    // a motion to the end of the folder.
    expect(postedJob()).toBeUndefined();
    expect(row(1).dataset.cursor).toBe("true");
    input.remove();
  });

  it("should NOT suspend on a checkbox, which is where focus sits constantly", async () => {
    renderBrowser();
    await browserReady();

    const checkbox = row(1).querySelector("input[type=checkbox]") as HTMLInputElement;
    checkbox.focus();
    type("j", checkbox);

    expect(row(2).dataset.cursor).toBe("true");
  });
});

describe("the context menu", () => {
  it("should act on the whole selection when it is opened inside one", async () => {
    renderBrowser();
    await browserReady();
    stub!.route({
      method: "POST",
      url: "/api/v1/files/pin",
      body: { updated: 3, folders: [], warning: null },
    });

    type("v2j<Esc>");
    fireEvent.contextMenu(row(2));

    const menu = await screen.findByRole("menu");
    expect(menu).toHaveTextContent("3 files selected");

    await userEvent.click(screen.getByRole("menuitem", { name: "Pin (3 files)" }));

    await waitFor(() =>
      expect(stub!.calls.find((c) => c.url === "/api/v1/files/pin")?.body).toEqual({
        ids: [1, 2, 3],
        pinned: true,
      }),
    );
  });

  it("should act on only the file clicked when it is opened outside the selection", async () => {
    renderBrowser();
    await browserReady();

    type("x");
    fireEvent.contextMenu(row(7));

    const menu = await screen.findByRole("menu");
    expect(menu).toHaveTextContent("file_07.jpg");
    expect(menu).not.toHaveTextContent("files selected");
  });

  it("should be reachable from the keyboard", async () => {
    renderBrowser();
    await browserReady();

    type("<Space>m");

    expect(await screen.findByRole("menu")).toHaveTextContent("file_01.jpg");
  });
});

describe("overlays", () => {
  function renderApp() {
    stub = stubFetch([
      { url: "/api/v1/status", body: aStatus() },
      { url: "/api/v1/folders", body: [] },
      { url: "/api/v1/files", body: FILES },
      { url: "/api/v1/events", body: [] },
    ]);
    return render(<App />);
  }

  it("should open the cheatsheet on ? and generate it from the real bindings", async () => {
    renderApp();
    await screen.findByRole("heading", { name: "CameraRoll", level: 2 });

    type("?");

    const sheet = await screen.findByRole("dialog", { name: "Keyboard shortcuts" });
    // Generated from the binding table, so it cannot describe a binding that
    // does not exist.
    expect(sheet.querySelector('[data-cheatsheet-binding="op.archive"]')).not.toBeNull();
    expect(sheet.querySelector('[data-cheatsheet-binding="motion.first"]')).not.toBeNull();
  });

  it("should close the cheatsheet on Escape and put focus back", async () => {
    renderApp();
    await screen.findByRole("heading", { name: "CameraRoll", level: 2 });

    type("?");
    await screen.findByRole("dialog", { name: "Keyboard shortcuts" });

    type("<Esc>");

    await waitFor(() =>
      expect(screen.queryByRole("dialog", { name: "Keyboard shortcuts" })).toBeNull(),
    );
  });

  it("should open the command palette on colon and complete a folder name", async () => {
    renderApp();
    await screen.findByRole("heading", { name: "CameraRoll", level: 2 });

    type(":");

    const input = await screen.findByLabelText("Command");
    await userEvent.type(input, "folder Cam");

    expect(
      await screen.findByRole("option", { name: /CameraRoll/ }),
    ).toHaveAttribute("data-palette-option", "CameraRoll");
  });

  it("should refuse a command that is not one, rather than closing silently", async () => {
    renderApp();
    await screen.findByRole("heading", { name: "CameraRoll", level: 2 });

    type(":");
    const input = await screen.findByLabelText("Command");
    await userEvent.type(input, "wq{Enter}");

    expect(await screen.findByText("not a command: :wq")).toBeInTheDocument();
  });

  it("should run an operator on the selection from the palette", async () => {
    renderApp();
    await screen.findByRole("heading", { name: "CameraRoll", level: 2 });
    stub!.route({
      method: "POST",
      url: "/api/v1/files/pin",
      body: { updated: 2, folders: [], warning: null },
    });

    await waitFor(() => expect(document.querySelector('[data-file-id="1"]')).not.toBeNull());
    type("xjx");
    expect(await screen.findByText("2 selected")).toBeInTheDocument();

    type(":");
    const input = await screen.findByLabelText("Command");
    await userEvent.type(input, "pin{Enter}");

    // The palette dispatches into the same layer the keyboard does, so there is
    // exactly one implementation of "pin the selection". ids are 1 and 6
    // because this is the gallery, where j is a whole row of five.
    await waitFor(() =>
      expect(stub!.calls.find((c) => c.url === "/api/v1/files/pin")?.body).toEqual({
        ids: [1, 6],
        pinned: true,
      }),
    );
  });

  it("should switch view from the palette", async () => {
    renderApp();
    await screen.findByRole("heading", { name: "CameraRoll", level: 2 });

    type(":");
    const input = await screen.findByLabelText("Command");
    await userEvent.type(input, "files{Enter}");

    await waitFor(() => expect(document.querySelector("table")).not.toBeNull());
  });

  it("should not let a key typed into the palette reach the grid behind it", async () => {
    renderApp();
    await screen.findByRole("heading", { name: "CameraRoll", level: 2 });
    stubArchiveJob();

    type(":");
    const input = await screen.findByLabelText("Command");
    await userEvent.type(input, "dd");

    expect(postedJob()).toBeUndefined();
    expect(input).toHaveValue(":dd");
  });
});

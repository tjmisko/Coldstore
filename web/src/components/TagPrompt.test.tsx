import { act, fireEvent, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";
import { renderWithToasts, stubFetch } from "../test/harness";
import { aFile } from "../test/fixtures";
import { TagPrompt } from "./TagPrompt";

let stub: ReturnType<typeof stubFetch> | null = null;

afterEach(() => {
  stub?.restore();
  stub = null;
  vi.unstubAllGlobals();
});

describe("TagPrompt", () => {
  it("commits a pre-seeded repeat with Enter and replaces the seed on typing", async () => {
    stub = stubFetch([{ url: "/api/v1/tags", body: [] }]);
    const repeat = vi.fn(async () => true);
    const first = renderWithToasts(
      <TagPrompt folder="CameraRoll" ids={[1]} mode="add" initialTags={["Trips/Iceland"]} onApply={repeat} onClose={() => {}} />,
    );

    fireEvent.keyDown(screen.getByLabelText("Tags to add"), { key: "Enter" });
    await waitFor(() => expect(repeat).toHaveBeenCalledWith(["Trips/Iceland"], []));
    first.unmount();

    const replaced = vi.fn(async () => true);
    renderWithToasts(
      <TagPrompt folder="CameraRoll" ids={[2]} mode="add" initialTags={["Trips/Iceland"]} onApply={replaced} onClose={() => {}} />,
    );
    await userEvent.type(screen.getByLabelText("Tags to add"), "Favorites");
    expect(screen.queryByText("Trips/Iceland")).toBeNull();
    await waitFor(() => expect(screen.getByRole("button", { name: "Apply tags" })).toBeEnabled());
    await userEvent.click(screen.getByRole("button", { name: "Apply tags" }));
    await waitFor(() => expect(replaced).toHaveBeenCalledWith(["Favorites"], []));
  });

  it("tokenizes with commas and semicolons and writes once only after pointer apply", async () => {
    stub = stubFetch([{ url: "/api/v1/tags", body: [] }]);
    const apply = vi.fn(async () => true);
    const close = vi.fn();
    renderWithToasts(
      <TagPrompt folder="CameraRoll" ids={[1, 2]} mode="add" onApply={apply} onClose={close} />,
    );

    await userEvent.type(screen.getByLabelText("Tags to add"), "Trips, Iceland;2024");
    expect(apply).not.toHaveBeenCalled();
    expect(await screen.findByRole("button", { name: /new tag “2024”/i })).toBeInTheDocument();
    await userEvent.click(screen.getByRole("button", { name: "Apply tags" }));

    await waitFor(() => expect(apply).toHaveBeenCalledWith(["Trips", "Iceland", "2024"], []));
    expect(close).toHaveBeenCalledTimes(1);
  });

  it("commits the residual and selected completion with one Enter", async () => {
    stub = stubFetch([{ url: "/api/v1/tags", body: [] }]);
    const apply = vi.fn(async () => true);
    const close = vi.fn();
    renderWithToasts(
      <TagPrompt folder="CameraRoll" ids={[1]} mode="add" onApply={apply} onClose={close} />,
    );

    const input = screen.getByLabelText("Tags to add");
    await userEvent.type(input, "Trips,2024");
    await screen.findByRole("button", { name: /new tag “2024”/i });
    fireEvent.keyDown(input, { key: "Enter" });

    await waitFor(() => expect(apply).toHaveBeenCalledWith(["Trips", "2024"], []));
    expect(apply).toHaveBeenCalledTimes(1);
    expect(close).toHaveBeenCalledOnce();
  });

  it("is a bottom command line with an exclusive scope, not a modal overlay", () => {
    stub = stubFetch([{ url: "/api/v1/tags", body: [] }]);
    renderWithToasts(
      <TagPrompt folder="CameraRoll" ids={[1]} mode="add" onApply={async () => true} onClose={() => {}} />,
    );

    const prompt = document.querySelector('[data-overlay="tag-prompt"]');
    expect(prompt).not.toBeNull();
    expect(prompt).not.toHaveAttribute("role");
    expect(prompt).toHaveAttribute("data-keyscope", "tag-prompt");
    expect(prompt?.parentElement).toHaveClass("pointer-events-none");
    expect(prompt?.parentElement).not.toHaveClass("bg-black/70");
    expect(document.body.style.overflow).toBe("");
  });

  it("cancels the entire locally-tokenized operation without a write", async () => {
    stub = stubFetch([{ url: "/api/v1/files?ids=7", body: [aFile({ id: 7, tags: ["Trips", "Favorites"] })] }]);
    const apply = vi.fn(async () => true);
    const close = vi.fn();
    renderWithToasts(
      <TagPrompt folder="CameraRoll" ids={[7]} mode="remove" onApply={apply} onClose={close} />,
    );

    await userEvent.type(screen.getByLabelText("Tags to remove"), "Trips;Favorites");
    await userEvent.click(screen.getByRole("button", { name: "Cancel" }));

    expect(apply).not.toHaveBeenCalled();
    expect(close).toHaveBeenCalledTimes(1);
  });

  it("cannot be dismissed after Apply starts until the write result is known", async () => {
    stub = stubFetch([{ url: "/api/v1/tags", body: [] }]);
    let finish: ((ok: boolean) => void) | undefined;
    const apply = vi.fn(() => new Promise<boolean>((resolve) => { finish = resolve; }));
    const close = vi.fn();
    renderWithToasts(
      <TagPrompt folder="CameraRoll" ids={[7]} mode="add" onApply={apply} onClose={close} />,
    );

    await userEvent.type(screen.getByLabelText("Tags to add"), "Reviewed");
    const applyButton = screen.getByRole("button", { name: "Apply tags" });
    await waitFor(() => expect(applyButton).toBeEnabled());
    await userEvent.click(applyButton);
    expect(screen.getByRole("button", { name: "Cancel" })).toBeDisabled();

    await userEvent.keyboard("{Escape}");
    const panel = document.querySelector('[data-overlay="tag-prompt"]');
    const backdrop = panel?.parentElement;
    if (!backdrop) throw new Error("tag prompt backdrop not found");
    fireEvent.mouseDown(backdrop, { target: backdrop });
    expect(close).not.toHaveBeenCalled();

    await act(async () => {
      finish?.(true);
      await Promise.resolve();
    });
    await waitFor(() => expect(close).toHaveBeenCalledTimes(1));
  });

  it("applies a removal with the pointer through the remove side of the bulk contract", async () => {
    stub = stubFetch([{
      url: "/api/v1/files?ids=7,8",
      body: [
        aFile({ id: 7, tags: ["Trips/Iceland"] }),
        aFile({ id: 8, tags: ["Trips/Iceland"] }),
      ],
    }]);
    const apply = vi.fn(async () => true);
    renderWithToasts(
      <TagPrompt folder="CameraRoll" ids={[7, 8]} mode="remove" onApply={apply} onClose={() => {}} />,
    );

    await userEvent.type(screen.getByLabelText("Tags to remove"), "Trips/Iceland");
    await waitFor(() => expect(screen.getByRole("button", { name: "Remove tags" })).toBeEnabled());
    await userEvent.click(screen.getByRole("button", { name: "Remove tags" }));

    await waitFor(() => expect(apply).toHaveBeenCalledWith([], ["Trips/Iceland"]));
  });

  it("does not ignore an unmatched remove residual when Enter is pressed", async () => {
    stub = stubFetch([{
      url: "/api/v1/files?ids=7",
      body: [aFile({ id: 7, tags: ["Trips"] })],
    }]);
    const apply = vi.fn(async () => true);
    renderWithToasts(
      <TagPrompt folder="CameraRoll" ids={[7]} mode="remove" onApply={apply} onClose={() => {}} />,
    );
    const input = screen.getByLabelText("Tags to remove");
    await userEvent.type(input, "Missing");
    await screen.findByText("No matching tags.");
    fireEvent.keyDown(input, { key: "Enter" });
    expect(apply).not.toHaveBeenCalled();
  });

  it("aborts superseded completion requests and ignores a stale response", async () => {
    let resolveOld: ((value: Response) => void) | undefined;
    const signals: AbortSignal[] = [];
    const fetchMock = vi.fn((input: RequestInfo | URL, init?: RequestInit) => {
      const url = String(input);
      if (init?.signal) signals.push(init.signal);
      if (url.includes("prefix=a&")) {
        return new Promise<Response>((resolve) => { resolveOld = resolve; });
      }
      if (url.includes("prefix=ab&")) {
        return Promise.resolve({
          ok: true, status: 200, json: async () => [{ id: 2, name: "AB New", count: 3, descendant_count: 3 }],
        } as Response);
      }
      return Promise.resolve({ ok: true, status: 200, json: async () => [] } as Response);
    });
    vi.stubGlobal("fetch", fetchMock);

    renderWithToasts(
      <TagPrompt folder="CameraRoll" ids={[1]} mode="add" onApply={async () => true} onClose={() => {}} />,
    );
    const input = screen.getByLabelText("Tags to add");
    await userEvent.type(input, "a");
    await waitFor(() => expect(fetchMock).toHaveBeenCalledTimes(1), { timeout: 1000 });
    await userEvent.type(input, "b");
    await waitFor(() => expect(fetchMock).toHaveBeenCalledTimes(2), { timeout: 1000 });

    expect(signals[0].aborted).toBe(true);
    expect(await screen.findByRole("button", { name: /AB New/ })).toBeInTheDocument();
    await act(async () => {
      resolveOld?.({
        ok: true, status: 200, json: async () => [{ id: 1, name: "A stale", count: 9, descendant_count: 9 }],
      } as Response);
      await Promise.resolve();
    });
    expect(screen.queryByText("A stale")).toBeNull();
  });

  it("cycles completions with Tab and Ctrl-N/P and preserves command-line editing keys", async () => {
    stub = stubFetch([{
      url: "/api/v1/tags",
      body: [
        { id: 1, name: "Trips/Iceland", count: 4, descendant_count: 4 },
        { id: 2, name: "Trips/Italy", count: 2, descendant_count: 2 },
      ],
    }]);
    renderWithToasts(
      <TagPrompt folder="CameraRoll" ids={[1]} mode="add" onApply={async () => true} onClose={() => {}} />,
    );
    const input = screen.getByLabelText("Tags to add");
    await userEvent.type(input, "Trips/I");
    const first = await screen.findByRole("button", { name: /Trips\/Iceland/ });
    const second = screen.getByRole("button", { name: /Trips\/Italy/ });
    expect(first).toHaveAttribute("aria-selected", "true");

    fireEvent.keyDown(input, { key: "Tab" });
    expect(second).toHaveAttribute("aria-selected", "true");
    fireEvent.keyDown(input, { key: "p", ctrlKey: true });
    expect(first).toHaveAttribute("aria-selected", "true");
    fireEvent.keyDown(input, { key: "u", ctrlKey: true });
    expect(input).toHaveValue("");
  });

  it("states the full count and refuses to truncate an over-cap selection", async () => {
    stub = stubFetch([{ url: "/api/v1/tags", body: [] }]);
    const apply = vi.fn(async () => true);
    renderWithToasts(
      <TagPrompt
        folder="CameraRoll"
        ids={Array.from({ length: 5001 }, (_, index) => index + 1)}
        mode="add"
        selectionNote="Selected the first 5,001 of 8,000 matching files."
        onApply={apply}
        onClose={() => {}}
      />,
    );

    expect(screen.getByText("Tag 5,001 files")).toHaveAttribute("data-tag-prompt-count");
    expect(screen.getByText(/Selected the first 5,001 of 8,000/)).toBeInTheDocument();
    expect(screen.getByText(/supports at most 5,000; no files will be tagged/)).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Apply tags" })).toBeDisabled();
    expect(apply).not.toHaveBeenCalled();
  });
});

import { screen } from "@testing-library/react";
import { render } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it } from "vitest";
import App from "./App";
import { aStatus } from "./test/fixtures";
import { stubFetch } from "./test/harness";

let stub: ReturnType<typeof stubFetch> | null = null;

afterEach(() => {
  stub?.restore();
  stub = null;
});

describe("App", () => {
  it("should explain a rejected credential instead of rendering an empty shell", async () => {
    stub = stubFetch([{ url: "/api/v1/status", status: 401, body: { error: "unauthorized" } }]);

    render(<App />);

    // Any API failure used to produce an empty sidebar, a null StatusBar and
    // "Select a folder from the sidebar" -- with no message anywhere.
    expect(await screen.findByText("Not authorised")).toBeInTheDocument();
    expect(
      screen.getByText(/username and password in coldstore.toml/),
    ).toBeInTheDocument();
  });

  it("should say the daemon is unreachable when the request never lands", async () => {
    stub = stubFetch([]);
    stub.route({
      url: "/api/v1/status",
      get body(): never {
        throw new TypeError("Failed to fetch");
      },
    });

    render(<App />);

    expect(await screen.findByText("Cannot reach coldstore")).toBeInTheDocument();
    expect(screen.getByText(/daemon may not be running/)).toBeInTheDocument();
  });

  it("should select the first folder as soon as status arrives", async () => {
    stub = stubFetch([
      { url: "/api/v1/status", body: aStatus() },
      { url: "/api/v1/files", body: [] },
      { url: "/api/v1/events", body: [] },
    ]);

    render(<App />);

    expect(await screen.findByRole("heading", { name: "CameraRoll", level: 2 })).toBeInTheDocument();
  });

  it("should name the tab after the selected folder instead of Vite's scaffold title", async () => {
    stub = stubFetch([
      { url: "/api/v1/status", body: aStatus() },
      { url: "/api/v1/files", body: [] },
      { url: "/api/v1/events", body: [] },
    ]);

    render(<App />);

    // The shipped build said "web" in every tab, because index.html still
    // carried the `npm create vite` default.
    await screen.findByRole("heading", { name: "CameraRoll", level: 2 });
    expect(document.title).toBe("CameraRoll — Coldstore");
  });

  it("should name the tab Settings when the config view is showing", async () => {
    stub = stubFetch([
      { url: "/api/v1/status", body: aStatus() },
      { url: "/api/v1/files", body: [] },
      { url: "/api/v1/events", body: [] },
    ]);

    render(<App />);

    await userEvent.click(await screen.findByRole("button", { name: /Settings/ }));

    expect(document.title).toBe("Settings — Coldstore");
  });
});

import { describe, expect, it } from "vitest";
import {
  completions,
  findCommand,
  parseCommandLine,
  resolveCommandLine,
  type CompletionContext,
} from "./palette";

const CONTEXT: CompletionContext = {
  folders: ["CameraRoll", "Backups", "Documents", "OldPhone"],
  filters: [
    { key: "all", label: "All" },
    { key: "synced", label: "Synced" },
    { key: "archived", label: "Archived" },
    { key: "pinned-user", label: "Pinned by me" },
    { key: "pinned-auto", label: "Auto-pinned on restore" },
    { key: "conflicts", label: "Conflicts" },
  ],
  views: ["gallery", "files", "config"],
  recent: [],
};

describe("parseCommandLine", () => {
  it("should split the name from the argument when a space has been typed", () => {
    expect(parseCommandLine(":folder Camera")).toEqual({
      name: "folder",
      args: "Camera",
      onArgument: true,
    });
  });

  it("should report that the owner is still typing the name when there is no space yet", () => {
    expect(parseCommandLine(":fol")).toEqual({ name: "fol", args: "", onArgument: false });
  });

  it("should report the argument as empty but started when the line ends in a space", () => {
    // This is the difference between completing command names and completing
    // folder labels, and it is exactly one space wide.
    expect(parseCommandLine(":folder ")).toEqual({ name: "folder", args: "", onArgument: true });
  });

  it("should work with or without the leading colon", () => {
    expect(parseCommandLine("filter archived").name).toBe("filter");
  });

  it("should keep a multi-word argument whole", () => {
    expect(parseCommandLine(":budget 20 GB").args).toBe("20 GB");
  });
});

describe("findCommand", () => {
  it("should find a command by its alias", () => {
    expect(findCommand("quit")?.name).toBe("q");
    expect(findCommand("help")?.name).toBe("h");
  });

  it("should return null when the name is not a command", () => {
    expect(findCommand("wq")).toBeNull();
  });
});

describe("completions", () => {
  it("should complete command names while the name is still being typed", () => {
    // ":fil" is a prefix of two commands, and the shorter one wins on a tie --
    // so the assertion is that both are offered, not which of them is first.
    const found = completions(":fil", CONTEXT).map((c) => c.label);
    expect(found.slice(0, 2)).toEqual(expect.arrayContaining([":files", ":filter"]));
    expect(completions(":filt", CONTEXT)[0].label).toBe(":filter");
  });

  it("should leave a trailing space when the command takes an argument", () => {
    // Otherwise taking the completion puts the cursor where the owner has to
    // press space themselves before they can type the folder.
    expect(completions(":folder", CONTEXT)[0].line).toBe(":folder ");
    expect(completions(":rescan", CONTEXT)[0].line).toBe(":rescan");
  });

  it("should complete folder labels once the argument has been started", () => {
    const found = completions(":folder Cam", CONTEXT);
    expect(found[0].line).toBe(":folder CameraRoll");
    expect(found.every((c) => c.terminal)).toBe(true);
  });

  it("should complete the filter keys, with their labels as the hint", () => {
    const found = completions(":filter pinned-a", CONTEXT);
    expect(found[0].label).toBe("pinned-auto");
    expect(found[0].hint).toBe("Auto-pinned on restore");
  });

  it("should float recently used commands to the top of an empty palette", () => {
    const found = completions(":", { ...CONTEXT, recent: ["restore", "budget"] });
    expect(found.slice(0, 2).map((c) => c.label)).toEqual([":restore", ":budget"]);
  });

  it("should rank by the match rather than by recency once something is typed", () => {
    // A palette that keeps reordering under the owner's fingers is one they
    // stop trusting.
    const found = completions(":arch", { ...CONTEXT, recent: ["restore"] });
    expect(found[0].label).toBe(":archive");
  });

  it("should offer nothing when the command takes no argument", () => {
    expect(completions(":rescan ", CONTEXT)).toEqual([]);
  });
});

describe("resolveCommandLine", () => {
  it("should resolve a folder from a unique prefix", () => {
    expect(resolveCommandLine(":folder Cam", CONTEXT)).toEqual({
      ok: true,
      action: { kind: "folder", label: "CameraRoll" },
    });
  });

  it("should refuse to guess when a folder prefix is ambiguous", () => {
    const context = { ...CONTEXT, folders: ["Camera", "Cameras"] };
    const result = resolveCommandLine(":folder Camera", context);
    // Exact wins over the ambiguity, which is the point of checking exact first.
    expect(result).toEqual({ ok: true, action: { kind: "folder", label: "Camera" } });
    expect(resolveCommandLine(":folder Cam", context).ok).toBe(false);
  });

  it("should report an unknown folder rather than silently doing nothing", () => {
    const result = resolveCommandLine(":folder Nope", CONTEXT);
    expect(result).toEqual({ ok: false, error: 'no folder matching "Nope"' });
  });

  it("should resolve every filter preset", () => {
    for (const filter of CONTEXT.filters) {
      expect(resolveCommandLine(`:filter ${filter.key}`, CONTEXT)).toEqual({
        ok: true,
        action: { kind: "filter", key: filter.key },
      });
    }
  });

  it("should turn the file verbs into operators", () => {
    for (const verb of ["archive", "restore", "pin", "unpin", "download"] as const) {
      expect(resolveCommandLine(`:${verb}`, CONTEXT)).toEqual({
        ok: true,
        action: { kind: "operator", operator: verb },
      });
    }
  });

  it("should hand the budget text on untouched, for parseSize to judge", () => {
    // Validated by the same parseSize the settings panel uses. Two validators
    // is how the palette starts accepting a budget the panel would refuse.
    expect(resolveCommandLine(":budget 20GB", CONTEXT)).toEqual({
      ok: true,
      action: { kind: "budget", text: "20GB" },
    });
  });

  it("should refuse a grace period that is not a number of minutes", () => {
    expect(resolveCommandLine(":grace soon", CONTEXT).ok).toBe(false);
    expect(resolveCommandLine(":grace 30", CONTEXT)).toEqual({
      ok: true,
      action: { kind: "grace", minutes: 30 },
    });
  });

  it("should support the vim meta commands a vim user types in the first minute", () => {
    expect(resolveCommandLine(":q", CONTEXT)).toEqual({ ok: true, action: { kind: "quit" } });
    expect(resolveCommandLine(":quit", CONTEXT)).toEqual({ ok: true, action: { kind: "quit" } });
    expect(resolveCommandLine(":h", CONTEXT)).toEqual({ ok: true, action: { kind: "help" } });
  });

  it("should name the missing argument when a command needs one", () => {
    expect(resolveCommandLine(":folder", CONTEXT)).toEqual({
      ok: false,
      error: ":folder needs label",
    });
  });

  it("should say so when the command does not exist", () => {
    expect(resolveCommandLine(":wq", CONTEXT)).toEqual({ ok: false, error: "not a command: :wq" });
  });

  it("should open the finder with whatever was typed after :find", () => {
    expect(resolveCommandLine(":find holiday", CONTEXT)).toEqual({
      ok: true,
      action: { kind: "finder", query: "holiday" },
    });
    expect(resolveCommandLine(":find", CONTEXT)).toEqual({
      ok: true,
      action: { kind: "finder", query: "" },
    });
  });
});

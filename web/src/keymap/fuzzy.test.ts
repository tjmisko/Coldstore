import { describe, expect, it } from "vitest";
import { fuzzyMatch, rank, rankStrings, scoreFields, type FileFields } from "./fuzzy";

function fields(overrides: Partial<FileFields> = {}): FileFields {
  return {
    filename: "IMG_20260112_091015.jpg",
    path: "CameraRoll/2026/IMG_20260112_091015.jpg",
    folder: "CameraRoll",
    extension: ".jpg",
    status: "synced",
    ...overrides,
  };
}

describe("fuzzyMatch", () => {
  it("should match a subsequence rather than only a substring", () => {
    expect(fuzzyMatch("img", "IMG_2026.jpg")).not.toBeNull();
    expect(fuzzyMatch("i2j", "IMG_2026.jpg")).not.toBeNull();
  });

  it("should return null when a query character is missing", () => {
    expect(fuzzyMatch("imgz", "IMG_2026.jpg")).toBeNull();
  });

  it("should report where it matched so the result can be highlighted", () => {
    expect(fuzzyMatch("img", "IMG_2026.jpg")?.positions).toEqual([0, 1, 2]);
  });

  it("should match everything when the query is empty", () => {
    expect(fuzzyMatch("", "anything")).toEqual({ score: 0, positions: [] });
  });

  it("should score a contiguous run above a scattering of the same letters", () => {
    const contiguous = fuzzyMatch("img", "img.txt")!.score;
    const scattered = fuzzyMatch("img", "i-m-g.txt")!.score;
    expect(contiguous).toBeGreaterThan(scattered);
  });

  it("should score a match at the start above one in the middle", () => {
    const atStart = fuzzyMatch("cam", "camera_roll.jpg")!.score;
    const inMiddle = fuzzyMatch("cam", "my_old_camera.jpg")!.score;
    expect(atStart).toBeGreaterThan(inMiddle);
  });

  it("should score a match after a separator above one mid-word", () => {
    const afterBreak = fuzzyMatch("roll", "camera_roll.jpg")!.score;
    const midWord = fuzzyMatch("roll", "controlled.jpg")!.score;
    expect(afterBreak).toBeGreaterThan(midWord);
  });

  it("should prefer the shorter of two otherwise identical matches", () => {
    const short = fuzzyMatch("img", "img.jpg")!.score;
    const long = fuzzyMatch("img", "img_but_a_very_long_name_indeed.jpg")!.score;
    expect(short).toBeGreaterThan(long);
  });
});

describe("scoreFields", () => {
  it("should rank a filename hit above a folder hit for the same query", () => {
    const inName = scoreFields("camera", fields({ filename: "camera.jpg" }))!;
    const inFolder = scoreFields("camera", fields({ filename: "IMG_0001.jpg" }))!;
    // Otherwise every file in CameraRoll ranks identically for "camera", which
    // is the same as having no ranking at all.
    expect(inName.field).toBe("filename");
    expect(inName.score).toBeGreaterThan(inFolder.score);
  });

  it("should find a file by its extension when that is what was typed", () => {
    expect(scoreFields("pdf", fields({ filename: "report", extension: ".pdf" }))?.field).toBe(
      "extension",
    );
  });

  it("should find a file by its status when that is what was typed", () => {
    expect(
      scoreFields("archived", fields({ filename: "x.bin", extension: ".bin", path: "x.bin", folder: "B", status: "archived" }))?.field,
    ).toBe("status");
  });

  it("should return null when nothing in the record matches", () => {
    expect(scoreFields("zzzz", fields())).toBeNull();
  });
});

describe("rank", () => {
  const files = [
    fields({ filename: "notes.txt", path: "Docs/notes.txt", extension: ".txt" }),
    fields({ filename: "IMG_2026.jpg", path: "CameraRoll/IMG_2026.jpg" }),
    fields({ filename: "a.txt", path: "i/m/g/a.txt", extension: ".txt" }),
  ];

  it("should put the file the query names first", () => {
    // "img" as a contiguous filename hit must beat "i/m/g" spelled out by a
    // path, which is the case a naive subsequence matcher gets backwards.
    const ranked = rank("img", files, (f) => f);
    expect(ranked[0].item.filename).toBe("IMG_2026.jpg");
  });

  it("should drop everything that does not match at all", () => {
    expect(rank("zzz", files, (f) => f)).toHaveLength(0);
  });

  it("should keep the caller's order when nothing distinguishes two files", () => {
    // A query that matches nothing in particular must not reshuffle the list on
    // every keystroke.
    const ranked = rank("", files, (f) => f);
    expect(ranked.map((r) => r.item.filename)).toEqual(["notes.txt", "IMG_2026.jpg", "a.txt"]);
  });
});

describe("rankStrings", () => {
  it("should rank a command whose name starts with the query above one that merely contains the letters", () => {
    const names = ["archive", "restore", "filter"];
    expect(rankStrings("re", names, (n) => n)[0]).toBe("restore");
  });

  it("should return every candidate in order when the query is empty", () => {
    const names = ["archive", "restore"];
    expect(rankStrings("", names, (n) => n)).toEqual(names);
  });
});

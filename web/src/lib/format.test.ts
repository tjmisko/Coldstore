import { describe, expect, it } from "vitest";
import { formatAgo, formatSize, parseSize } from "./format";

describe("formatSize", () => {
  it("should render the same SI units the daemon writes when given a TOML budget", () => {
    // catalog.HumanSI produces exactly these strings, and the wedge banner
    // quotes both: a UI on 1024-based units renders a 20GB budget as 18.6 GB.
    expect(formatSize(20_000_000_000)).toBe("20 GB");
    expect(formatSize(200_000)).toBe("200 kB");
    expect(formatSize(999)).toBe("999 B");
    expect(formatSize(1_500)).toBe("1.5 kB");
    expect(formatSize(0)).toBe("0 B");
  });

  it("should not render a trailing .0 when the value is whole", () => {
    expect(formatSize(2_000_000)).toBe("2 MB");
  });
});

describe("parseSize", () => {
  it("should round-trip a budget the owner typed in coldstore.toml", () => {
    expect(parseSize("20 GB")).toBe(20_000_000_000);
    expect(parseSize("20GB")).toBe(20_000_000_000);
    expect(parseSize("200kB")).toBe(200_000);
    expect(parseSize("1.5 MB")).toBe(1_500_000);
    expect(parseSize(" 500 ")).toBe(500);
  });

  it("should round-trip whatever formatSize rendered", () => {
    for (const bytes of [999, 1_500, 200_000, 20_000_000_000]) {
      expect(parseSize(formatSize(bytes))).toBe(bytes);
    }
  });

  it("should return NaN for input that is not a size, so the editor can refuse it inline", () => {
    for (const bad of ["", "abc", "20 gigs", "GB", "-5 GB"]) {
      expect(Number.isNaN(parseSize(bad))).toBe(true);
    }
  });
});

describe("formatAgo", () => {
  const now = Date.UTC(2026, 7, 20, 12, 0, 0);

  it("should say never when a folder has never been enforced", () => {
    expect(formatAgo(null, now)).toBe("never");
  });

  it("should count in the unit that reads naturally at that distance", () => {
    expect(formatAgo(now / 1000 - 30, now)).toBe("just now");
    expect(formatAgo(now / 1000 - 300, now)).toBe("5 min ago");
    expect(formatAgo(now / 1000 - 7200, now)).toBe("2 h ago");
    expect(formatAgo(now / 1000 - 86400 * 5, now)).toBe("5 days ago");
  });
});

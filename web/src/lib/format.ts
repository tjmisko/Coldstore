// One formatter per quantity, for the whole app.
//
// There used to be four copies of formatSize with two different rounding
// schemes, all of them binary (1024-based). internal/config.ParseBudget is SI,
// so a TOML budget of "20GB" rendered as "18.6 GB" -- a number the owner had
// never typed, next to a bar that claimed to measure it.

const SIZE_UNITS = ["kB", "MB", "GB", "TB", "PB"] as const;

/** formatSize mirrors catalog.HumanSI exactly, so a budget rendered here and a
 *  wedge warning written by the daemon are worded identically. */
export function formatSize(bytes: number): string {
  if (!Number.isFinite(bytes)) return "-";
  if (bytes < 0) return "-" + formatSize(-bytes);
  if (bytes < 1000) return `${Math.round(bytes)} B`;

  let value = bytes;
  let suffix: string = SIZE_UNITS[0];
  for (const unit of SIZE_UNITS) {
    value /= 1000;
    suffix = unit;
    if (value < 1000) break;
  }
  const rounded = Math.round(value * 10) / 10;
  return Number.isInteger(rounded)
    ? `${rounded.toFixed(0)} ${suffix}`
    : `${rounded.toFixed(1)} ${suffix}`;
}

const SIZE_MULTIPLIERS: Record<string, number> = {
  B: 1,
  KB: 1_000,
  MB: 1_000_000,
  GB: 1_000_000_000,
  TB: 1_000_000_000_000,
};

/** parseSize is the inverse of formatSize for the budget editor, and it accepts
 *  what coldstore.toml accepts (config.ParseBudget): a number with an optional
 *  SI unit, case-insensitive. A bare number is bytes. Returns NaN when the
 *  input is not a size, so callers can reject it inline rather than PUTting a
 *  budget the server has to guess at. */
export function parseSize(input: string): number {
  const text = input.trim().toUpperCase().replace(/I?B$/, "B");
  if (text === "") return NaN;

  const match = /^([0-9]*\.?[0-9]+)\s*(B|KB|MB|GB|TB)?$/.exec(text);
  if (!match) return NaN;

  const value = Number.parseFloat(match[1]);
  if (!Number.isFinite(value)) return NaN;
  return Math.round(value * SIZE_MULTIPLIERS[match[2] ?? "B"]);
}

export function formatCount(n: number): string {
  return n.toLocaleString();
}

export function formatDate(iso: string): string {
  const date = new Date(iso);
  if (Number.isNaN(date.getTime())) return "-";
  return date.toLocaleDateString(undefined, { year: "numeric", month: "short", day: "numeric" });
}

export function formatDateTime(iso: string): string {
  const date = new Date(iso);
  if (Number.isNaN(date.getTime())) return "-";
  return date.toLocaleString(undefined, {
    year: "numeric",
    month: "short",
    day: "numeric",
    hour: "2-digit",
    minute: "2-digit",
  });
}

/** formatAgo turns an epoch-SECONDS timestamp -- which is what last_enforced_at
 *  and pinned_at are -- into "3 min ago". `now` is injectable so tests do not
 *  have to mock the clock. */
export function formatAgo(epochSeconds: number | null | undefined, now = Date.now()): string {
  if (epochSeconds === null || epochSeconds === undefined) return "never";
  const seconds = Math.max(0, Math.round(now / 1000 - epochSeconds));
  if (seconds < 60) return "just now";
  const minutes = Math.round(seconds / 60);
  if (minutes < 60) return `${minutes} min ago`;
  const hours = Math.round(minutes / 60);
  if (hours < 48) return `${hours} h ago`;
  return `${Math.round(hours / 24)} days ago`;
}

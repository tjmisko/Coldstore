import type { FileRecord } from "../types/api";

const manualCapture = /^(\d{4})-(\d{2})-(\d{2}) (\d{2}):(\d{2}):(\d{2})(?:\.(\d{3}))?$/;

export function isValidManualCapture(value: string): boolean {
  if (value === "") return true;
  const match = manualCapture.exec(value);
  if (!match) return false;
  const [, yearText, monthText, dayText, hourText, minuteText, secondText] = match;
  const year = Number(yearText);
  const month = Number(monthText);
  const day = Number(dayText);
  const hour = Number(hourText);
  const minute = Number(minuteText);
  const second = Number(secondText);
  if (month < 1 || month > 12 || hour > 23 || minute > 59 || second > 59) return false;
  const leap = year % 4 === 0 && (year % 100 !== 0 || year % 400 === 0);
  const days = [31, leap ? 29 : 28, 31, 30, 31, 30, 31, 31, 30, 31, 30, 31];
  return day >= 1 && day <= days[month - 1];
}

export function captureDay(file: FileRecord): string {
  return file.captured_local.length >= 10 ? file.captured_local.slice(0, 10) : "";
}

export interface CaptureDayGroup {
  day: string;
  start: number;
  files: FileRecord[];
}

/** Group contiguous rows from sort=captured without re-sorting a paged result. */
export function captureDayGroups(files: FileRecord[]): CaptureDayGroup[] {
  const groups: CaptureDayGroup[] = [];
  for (const file of files) {
    const day = captureDay(file);
    const current = groups[groups.length - 1];
    if (!current || current.day !== day) {
      groups.push({ day, start: groups.reduce((sum, group) => sum + group.files.length, 0), files: [file] });
    } else {
      current.files.push(file);
    }
  }
  return groups;
}

export interface CaptureDayJump {
  index: number;
  /** False only when a forward jump needs rows beyond the loaded page. */
  complete: boolean;
}

export function resolveCaptureDayJump(
  files: FileRecord[],
  index: number,
  direction: -1 | 1,
  count: number,
): CaptureDayJump {
  const groups = captureDayGroups(files);
  if (groups.length === 0) return { index: 0, complete: true };
  const current = groups.findIndex((group, at) => {
    const end = groups[at + 1]?.start ?? files.length;
    return index >= group.start && index < end;
  });
  const distance = count > 0 ? count : 1;
  const wanted = current + direction * distance;
  const target = Math.max(0, Math.min(wanted, groups.length - 1));
  return {
    index: groups[target].start,
    // Pages append in captured-descending order, so only a forward jump past
    // the last loaded group can become satisfiable by loading another page.
    complete: direction < 0 || wanted < groups.length,
  };
}

export function captureDayTarget(
  files: FileRecord[],
  index: number,
  direction: -1 | 1,
  count: number,
): number {
  return resolveCaptureDayJump(files, index, direction, count).index;
}

export function formatCaptureDay(day: string): string {
  if (day === "") return "No capture date";
  const [year, month, date] = day.split("-").map(Number);
  return new Intl.DateTimeFormat("en-US", {
    year: "numeric",
    month: "long",
    day: "numeric",
    timeZone: "UTC",
  }).format(new Date(Date.UTC(year, month - 1, date)));
}

export function formatCaptureOffset(offset: number | null): string {
  if (offset === null) return "Timezone not recorded";
  const sign = offset < 0 ? "-" : "+";
  const absolute = Math.abs(offset);
  const hours = String(Math.floor(absolute / 60)).padStart(2, "0");
  const minutes = String(absolute % 60).padStart(2, "0");
  return `UTC${sign}${hours}:${minutes}`;
}

import type { MotionId } from "../keymap/types";
import type { FileRecord } from "../types/api";
import { resolveCaptureDayJump, type CaptureDayJump } from "./capture";

export interface ContextMotionResult {
  index: number;
  /** False when another appended page could still satisfy the full count. */
  complete: boolean;
}

export function isContextMotion(motion: MotionId): boolean {
  return [
    "capture-day-next", "capture-day-previous", "tag-next", "tag-previous",
    "untagged-next", "untagged-previous", "pair-next", "pair-previous",
  ].includes(motion);
}

function directionOf(motion: MotionId): -1 | 1 {
  return motion.endsWith("previous") ? -1 : 1;
}

/** Resolve metadata-aware motions identically in the grid and lightbox.
 * Forward exhaustion remains incomplete while another page exists; callers can
 * therefore fetch and replay the original command without losing its count. */
export function resolveContextMotion(
  files: FileRecord[],
  at: number,
  motion: MotionId,
  count: number,
  hasMore: boolean,
  resolveCaptureDay?: (index: number, direction: -1 | 1, count: number) => CaptureDayJump,
): ContextMotionResult {
  const direction = directionOf(motion);
  if (motion === "capture-day-next" || motion === "capture-day-previous") {
    const result = resolveCaptureDay?.(at, direction, count) ?? resolveCaptureDayJump(files, at, direction, count);
    if (direction > 0 && !result.complete && !hasMore) {
      return { index: Math.max(at, files.length - 1), complete: true };
    }
    return { ...result, complete: result.complete || direction < 0 || !hasMore };
  }

  const distance = count > 0 ? count : 1;
  let current = at;
  for (let step = 0; step < distance; step += 1) {
    const source = files[current];
    if (!source) return { index: at, complete: true };
    let found = -1;
    if (motion === "tag-next" || motion === "tag-previous") {
      if (source.tags.length === 0) return { index: current, complete: true };
      const sourceTags = new Set(source.tags.map((tag) => tag.toLocaleLowerCase()));
      for (let index = current + direction; index >= 0 && index < files.length; index += direction) {
        if (files[index].tags.some((tag) => sourceTags.has(tag.toLocaleLowerCase()))) {
          found = index;
          break;
        }
      }
    } else if (motion === "untagged-next" || motion === "untagged-previous") {
      for (let index = current + direction; index >= 0 && index < files.length; index += direction) {
        if (files[index].tags.length === 0) {
          found = index;
          break;
        }
      }
    } else {
      const group = source.pair_key ? `pair:${source.pair_key}` : `file:${source.id}`;
      for (let index = current + direction; index >= 0 && index < files.length; index += direction) {
        const candidate = files[index];
        const candidateGroup = candidate.pair_key ? `pair:${candidate.pair_key}` : `file:${candidate.id}`;
        if (candidateGroup !== group) {
          found = index;
          break;
        }
      }
    }
    if (found < 0) return { index: current, complete: direction < 0 || !hasMore };
    current = found;
  }
  return { index: current, complete: true };
}

/** A next-day motion lands on the first row of the next day, while an operator
 * over that motion ends on the final row of the current/last covered day. */
export function contextOperatorEndpoint(motion: MotionId, from: number, to: number): number {
  return motion === "capture-day-next" && to > from ? to - 1 : to;
}

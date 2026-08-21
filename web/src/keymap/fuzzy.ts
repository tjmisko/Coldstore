/** The finder's scorer.
 *
 *  Server-side `q` is `filename LIKE '%q%'` (internal/catalog/catalog.go:230):
 *  filename only, no path, no ranking, no FTS. So ranking is the client's job,
 *  and this is where the fiddly off-by-ones live. It is pure, and its tests
 *  assert on ORDER rather than on exact scores -- freezing the numbers would
 *  make every future tuning change a test rewrite. */

const WORD_BREAK = /[\s_\-./\\]/;

/** Bonus for a match at the start of the string: the thing the owner is most
 *  likely typing the beginning of. */
const BONUS_FIRST = 12;
/** Bonus for a match right after a separator, or at a camelCase hump. */
const BONUS_BOUNDARY = 7;
/** Bonus per character in a run. Runs are what make "img20" beat a scattering
 *  of the same five letters. */
const BONUS_CONSECUTIVE = 8;
/** Bonus when the query appears verbatim. */
const BONUS_CONTIGUOUS = 18;
/** Charged per character skipped between matches, capped so a long path does
 *  not go arbitrarily negative. */
const PENALTY_GAP = 1;
const MAX_GAP_PENALTY = 12;
/** A gentle preference for the shorter of two otherwise equal matches. */
const PENALTY_LENGTH = 0.05;

export interface FuzzyMatch {
  score: number;
  /** Indices in `text` that matched, for highlighting. */
  positions: number[];
}

function isBoundary(text: string, index: number): boolean {
  if (index === 0) return true;
  const previous = text[index - 1];
  if (WORD_BREAK.test(previous)) return true;
  // camelCase: a capital preceded by a lowercase.
  return previous === previous.toLowerCase() && text[index] !== text[index].toLowerCase();
}

/** The occurrence of a verbatim query worth scoring: at the start if there is
 *  one, otherwise at a word boundary, otherwise the earliest.
 *
 *  Picking one matters more than it looks. "roll" in "camera_roll.jpg" is
 *  contiguous at index 7, but a greedy character walk takes the `r` of "camera"
 *  at index 4 and then scores the result as four scattered characters -- which
 *  is how "controlled.jpg" used to outrank it. */
function bestContiguousIndex(haystack: string, needle: string, text: string): number {
  let earliest = -1;
  let boundary = -1;
  let at = haystack.indexOf(needle);
  while (at !== -1) {
    if (at === 0) return 0;
    if (earliest === -1) earliest = at;
    if (boundary === -1 && isBoundary(text, at)) boundary = at;
    at = haystack.indexOf(needle, at + 1);
  }
  return boundary !== -1 ? boundary : earliest;
}

/** fuzzyMatch is a subsequence match, scored.
 *
 *  The character walk is greedy rather than optimal: it takes the first
 *  occurrence of each query character. An optimal aligner is O(n*m) per
 *  candidate and this runs over two thousand files on every keystroke, on a
 *  box whose CPU is a 1.5 GHz core from 2011. The verbatim case is handled
 *  separately above, which is where greediness would otherwise cost the most. */
export function fuzzyMatch(query: string, text: string): FuzzyMatch | null {
  if (query === "") return { score: 0, positions: [] };
  if (text === "") return null;

  const haystack = text.toLowerCase();
  const needle = query.toLowerCase();

  let positions: number[];
  const verbatim = bestContiguousIndex(haystack, needle, text);
  if (verbatim !== -1) {
    positions = Array.from({ length: needle.length }, (_, i) => verbatim + i);
  } else {
    positions = [];
    let cursor = 0;
    for (const char of needle) {
      const found = haystack.indexOf(char, cursor);
      if (found === -1) return null;
      positions.push(found);
      cursor = found + 1;
    }
  }

  let score = verbatim !== -1 ? BONUS_CONTIGUOUS : 0;
  let previous = -2;
  for (let i = 0; i < positions.length; i++) {
    const at = positions[i];
    if (at === 0) score += BONUS_FIRST;
    else if (isBoundary(text, at)) score += BONUS_BOUNDARY;
    if (at === previous + 1) score += BONUS_CONSECUTIVE;
    else if (i > 0) score -= Math.min(MAX_GAP_PENALTY, (at - previous - 1) * PENALTY_GAP);
    if (text[at] === query[i]) score += 1; // exact case
    previous = at;
  }

  score -= text.length * PENALTY_LENGTH;
  return { score, positions };
}

/** The fields a file is searched over, and how much each is worth.
 *
 *  Filename first: "which photo" is a filename question. Path, folder,
 *  extension and status are there because the owner asked for them -- ".pdf"
 *  and "archived" are real queries -- but a filename hit has to win over a
 *  folder hit or every file in CameraRoll ranks equally for "camera". */
export interface FileFields {
  filename: string;
  path: string;
  folder: string;
  extension: string;
  status: string;
}

const FIELD_WEIGHTS: { field: keyof FileFields; weight: number }[] = [
  { field: "filename", weight: 1 },
  { field: "path", weight: 0.72 },
  { field: "extension", weight: 0.6 },
  { field: "folder", weight: 0.45 },
  { field: "status", weight: 0.4 },
];

export interface FieldMatch {
  score: number;
  field: keyof FileFields;
  positions: number[];
}

/** scoreFields returns the best-scoring field, or null when the query matches
 *  nothing. Best, not the sum: summing lets a file whose path repeats its own
 *  name outrank a file that simply IS what was typed. */
export function scoreFields(query: string, fields: FileFields): FieldMatch | null {
  if (query === "") return { score: 0, field: "filename", positions: [] };

  let best: FieldMatch | null = null;
  for (const { field, weight } of FIELD_WEIGHTS) {
    const value = fields[field];
    if (!value) continue;
    const match = fuzzyMatch(query, value);
    if (!match) continue;
    const scaled = match.score * weight;
    if (!best || scaled > best.score) {
      best = { score: scaled, field, positions: match.positions };
    }
  }
  return best;
}

export interface Ranked<T> {
  item: T;
  match: FieldMatch;
}

/** rank is a stable sort: equal scores keep the order the caller supplied,
 *  which for files is the folder's own sort. Without that, a query that
 *  matches nothing in particular reshuffles the list on every keystroke. */
export function rank<T>(
  query: string,
  items: readonly T[],
  fieldsOf: (item: T) => FileFields,
): Ranked<T>[] {
  const scored: { entry: Ranked<T>; order: number }[] = [];
  items.forEach((item, order) => {
    const match = scoreFields(query, fieldsOf(item));
    if (match) scored.push({ entry: { item, match }, order });
  });
  scored.sort((a, b) => b.entry.match.score - a.entry.match.score || a.order - b.order);
  return scored.map((s) => s.entry);
}

/** rankStrings is the same thing for command names in the palette. */
export function rankStrings<T>(
  query: string,
  items: readonly T[],
  textOf: (item: T) => string,
): T[] {
  if (query === "") return [...items];
  const scored: { item: T; score: number; order: number }[] = [];
  items.forEach((item, order) => {
    const match = fuzzyMatch(query, textOf(item));
    if (match) scored.push({ item, score: match.score, order });
  });
  scored.sort((a, b) => b.score - a.score || a.order - b.order);
  return scored.map((s) => s.item);
}

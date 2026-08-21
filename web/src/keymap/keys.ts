/** Key normalisation: one KeyboardEvent in, one canonical token out.
 *
 *  Every layer above this one -- the trie, the machine, the binding table, the
 *  cheatsheet -- speaks tokens, never `event.key` plus four booleans. A binding
 *  table written against the raw event is a table where `<C-d>` has to be
 *  spelled three different ways depending on who wrote the row. */

/** The shape of a KeyboardEvent this module actually reads. Taking a structural
 *  type rather than KeyboardEvent is what lets the tests -- and the machine's
 *  tests, which are the ones that matter -- work in plain objects. */
export interface KeyEventLike {
  key: string;
  ctrlKey?: boolean;
  metaKey?: boolean;
  altKey?: boolean;
  shiftKey?: boolean;
}

/** `event.key` values that ARE a modifier. A keydown for one of these is not a
 *  keystroke yet; dispatching on it would fire `<C-...>` bindings the moment
 *  Control went down, before the owner had chosen anything. */
const MODIFIER_KEYS = new Set([
  "Shift",
  "Control",
  "Alt",
  "Meta",
  "AltGraph",
  "CapsLock",
  "NumLock",
  "ScrollLock",
  "Fn",
  "FnLock",
  "Hyper",
  "Super",
  "Symbol",
  "SymbolLock",
  "Dead",
  "Process",
  "Unidentified",
]);

/** Named keys, in vim's spelling. `<CR>` rather than `<Enter>` and `<BS>`
 *  rather than `<Backspace>` because the audience for this table already has
 *  those names in their fingers. */
const NAMED_KEYS: Record<string, string> = {
  Escape: "Esc",
  Enter: "CR",
  " ": "Space",
  Tab: "Tab",
  Backspace: "BS",
  Delete: "Del",
  Insert: "Insert",
  ArrowUp: "Up",
  ArrowDown: "Down",
  ArrowLeft: "Left",
  ArrowRight: "Right",
  Home: "Home",
  End: "End",
  PageUp: "PageUp",
  PageDown: "PageDown",
  ContextMenu: "Menu",
};

const FUNCTION_KEY = /^F([1-9]|1[0-9]|2[0-4])$/;

/** normalizeKey returns null for anything that is not a keystroke: a bare
 *  modifier, an IME composition, a key name this table does not know. Null
 *  means "not ours" -- the caller must not preventDefault it. */
export function normalizeKey(event: KeyEventLike): string | null {
  const { key } = event;
  if (key === undefined || key === "" || MODIFIER_KEYS.has(key)) return null;

  let base: string;
  let named: boolean;

  if (NAMED_KEYS[key] !== undefined) {
    base = NAMED_KEYS[key];
    named = true;
  } else if (FUNCTION_KEY.test(key)) {
    base = key;
    named = true;
  } else if ([...key].length === 1) {
    base = key;
    named = false;
  } else {
    return null;
  }

  const parts: string[] = [];
  if (event.ctrlKey) parts.push("C");
  if (event.metaKey) parts.push("D");
  if (event.altKey) parts.push("A");

  // Shift is only ever spelled out when it cannot already be read off the
  // character. `V` and `$` carry their own shift; `<C-a>` and `<C-S-a>` do not,
  // because a control chord reports the unshifted letter.
  let shifted = false;
  if (named) {
    shifted = event.shiftKey === true;
  } else if (parts.length > 0 && /^[A-Za-z]$/.test(base)) {
    shifted = event.shiftKey === true;
    base = base.toLowerCase();
  }
  if (shifted) parts.push("S");

  if (parts.length === 0 && !named) return base;
  return `<${[...parts, base].join("-")}>`;
}

/** parseSequence turns the written form of a binding into its tokens.
 *
 *  "gg" -> ["g","g"], "<Space>fe" -> ["<Space>","f","e"]. Written as a parser
 *  rather than as an array literal per binding because the binding table is
 *  read far more often than it is written, and `["<Space>","f","e"]` is not
 *  something anyone can scan a hundred rows of. */
export function parseSequence(written: string): string[] {
  const tokens: string[] = [];
  let i = 0;
  while (i < written.length) {
    if (written[i] === "<") {
      const close = written.indexOf(">", i);
      if (close === -1) throw new Error(`unterminated key token in "${written}"`);
      tokens.push(written.slice(i, close + 1));
      i = close + 1;
      continue;
    }
    // Iterate by code point so an emoji or an accented character is one token.
    const codePoint = written.codePointAt(i);
    const char = String.fromCodePoint(codePoint ?? written.charCodeAt(i));
    tokens.push(char);
    i += char.length;
  }
  return tokens;
}

/** The written form of a token sequence, for the cheatsheet and the pending
 *  indicator. The inverse of parseSequence. */
export function writeSequence(tokens: readonly string[]): string {
  return tokens.join("");
}

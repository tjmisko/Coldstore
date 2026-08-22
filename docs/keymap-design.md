# Keymap Design

The control plane has to be drivable with no mouse at all: archive, restore, pin,
download, resolve a conflict, switch folder, switch filter, open the lightbox,
find a file. The owner's request was not a shortcut table but the grammar they
already have in their fingers -- counts, motions, and operators that compose, so
`3j`, `d3j`, `v` plus a motion and `ggVG` all mean what they mean in vim.

That is an architectural decision, not a feature. A flat `Record<string, () => void>`
cannot express `d3j`, because by the time `d` is dispatched the handler has no way
to say "I am not finished, and the next few keys belong to me". Everything below
follows from choosing a machine over a table.

## 0. What the app had before this

Three `window` keydown listeners, no registry, no shared state:

| Where | Keys |
|---|---|
| `Gallery.tsx:44-69` | Ctrl/Cmd+A, Escape, Enter |
| `Lightbox.tsx:67-105` | arrows, PageUp/Down, Home/End, Space, Escape |
| `ConfirmDialog.tsx:30-37` | Escape |

Only the first had an "am I typing?" guard, and it was local. The FileBrowser
table had no bindings at all and its rows were not focusable; the only `tabIndex`
in the app was on the gallery tile. Three listeners on one window also means three
components racing for the same Escape, which is why the lightbox and the dialog
both closed on one press.

## 1. Architecture

Nine modules, none of which import React. That is deliberate: the grammar is
where the bugs are, and a grammar tested as a pure function costs a millisecond
per case instead of a jsdom render.

```
web/src/keymap/
  keys.ts       KeyboardEvent -> a canonical token string        ("j", "<C-d>", "<Esc>")
  motions.ts    (motion, count, geometry) -> an index, or a span (pure arithmetic)
  registry.ts   the binding trie: sequence -> binding, per scope
  machine.ts    the reducer: counts, operators, pending sequences, visual mode
  bindings.ts   the binding TABLE -- data, not code
  fuzzy.ts      the finder's scorer
  palette.ts    the ex-command table, its argument parser and its completion
  textEntry.ts  the shared text-entry suspension predicate
  types.ts      the exhaustive motion/operator/action/scope vocabulary
```

and one thin React layer:

```
web/src/lib/keymapContext.ts     the context object (see "why lib" below)
web/src/components/KeymapProvider.tsx  one window listener, the scope stack
web/src/hooks/useGridCursor.ts   cursor index <-> DOM focus
```

`lib/keymapContext.ts` rather than the provider file because
`react-refresh/only-export-components` fails any module that exports both a
component and a hook -- the same reason `lib/toast.ts` is split from
`components/ToastProvider.tsx`.

### 1.1 The pipeline

```
keydown
  |
  |-- suspended?  (focus is in a text field)            -> return, untouched
  |
  |-- normalize   keys.ts        KeyboardEvent -> "3" | "d" | "<C-d>" | "<Space>"
  |
  |-- resolve     registry.ts    active scope chain -> the bindings in force
  |
  |-- step        machine.ts     (state, token, bindings) -> (state', Command[])
  |
  +-- interpret   KeymapProvider -> the active view's CommandTarget
```

`step` is `(state, input, bindings) => { state, commands, awaitTimeout, handled }`.
It is total, synchronous and allocation-light, and every test in
`machine.test.ts` drives it directly with an array of tokens.

### 1.2 Why a reducer and not a state class

The machine has to be re-entrant (a keydown can arrive while an async action from
the last one is still running) and it has to be replayable in a test as a fold
over a list of tokens:

```ts
const { commands } = run(["2", "d", "3", "j"]);
expect(commands).toEqual([{ kind: "operator", operator: "archive",
                            target: { kind: "motion", motion: "down", count: 6 } }]);
```

A mutable class would make that test order-dependent on construction, and would
tempt the DOM work into the same object.

## 2. State

```ts
interface MachineState {
  mode: "normal" | "visual" | "visual-line";
  count: string;           // digits typed before an operator or a motion
  operator: OperatorId | null;
  operatorCount: string;   // digits typed after the operator
  pending: string[];       // a partially matched sequence: ["g"], ["<Space>","f"]
}
```

Operator-pending is not a fourth mode: it is `operator !== null`. Vim's
operator-pending state is exactly "an operator is waiting for a range", and
giving it its own enum value would let `mode` and `operator` disagree.

Visual **is** a mode, because in visual an operator executes immediately over the
highlighted span rather than waiting for a motion, and motions extend instead of
jumping.

### 2.1 Counts

Digits accumulate into `count` in normal and visual, and into `operatorCount`
once an operator is pending. `0` is a motion (start of row) when the relevant
count buffer is empty and a digit when it is not -- vim's rule, and the reason
`0` cannot simply be a binding.

The effective count is the **product**: `2d3j` is `d6j`, six items, because that
is what vim does and the owner will expect it. `count("") === 1`.

A count is capped at 5 digits. `999999999j` is a typo, and an uncapped count is
an easy way to make the interpreter allocate a span the size of the catalog.

### 2.2 Pending sequences and the timeout

The registry is a trie over token arrays, so `gg`, `<Space>fe` and `zz` cost
nothing special. On each token:

- **no binding starts with `pending + token`** -> the sequence is dead. Reset to
  normal, drop the count, report the whole thing as unhandled (so the browser
  keeps its own shortcuts) and swallow nothing.
- **an exact match and no longer binding exists** -> fire immediately.
- **an exact match and a longer binding also exists** (`d` alone vs `dd`) -> hold,
  and return `awaitTimeout: 800`. The React layer schedules one timer and feeds
  `{type:"timeout"}` back in; the machine then fires the exact match. This is
  vim's `timeoutlen` and it is fed back as an *input*, not read from a clock, so
  tests advance it explicitly and never sleep.
- **no exact match but a longer one exists** -> hold, no timeout needed (nothing
  to fall back to).

`<Esc>` and `<C-c>` are handled before the trie in every state: they clear the
count, the operator, the pending sequence, and leave visual mode. There is no
sequence they can be a prefix of, and a cancel that can itself get stuck pending
is not a cancel.

## 3. Motions, and what a "line" is in a grid

`motions.ts` knows nothing about React or the DOM. It takes a geometry:

```ts
interface Geometry { index: number; total: number; columns: number; rows: number; }
```

`columns` is 1 for the file table and the lightbox and comes from the responsive
grid in the gallery; `rows` is how many rows fit on screen, for `<C-d>`/`<C-f>`.
The table and the grid therefore share every motion: a table is a grid one column
wide, and that is the whole reason FileBrowser gets the same bindings for free.

| Motion | Keys | Behaviour |
|---|---|---|
| `left` / `right` | `h` `l` | within the row; does not wrap |
| `up` / `down` | `k` `j` | +/- `columns`, keeping the column |
| `row-start` / `row-end` | `0` `$` | first / last item of the visual row |
| `first` | `gg` | item 1, or item N with a count |
| `last` | `G` | last item, or item N with a count |
| `half-page-down` / `-up` | `<C-d>` `<C-u>` | `floor(rows/2)` rows |
| `page-down` / `-up` | `<C-f>` `<C-b>` | `rows` rows |
| `capture-day-next` / `-previous` | `}` `{` | next / previous capture-day boundary |
| `tag-next` / `-previous` | `]t` `[t` | next / previous file sharing a tag |
| `untagged-next` / `-previous` | `]u` `[u` | next / previous untagged file |
| `pair-next` / `-previous` | `]p` `[p` | next / previous distinct pair group |

The final four motion families require file metadata and are resolved by the active file
layer; `motions.ts` remains pure geometry and returns the current index for those ids.

`down` from the last row clamps by whole rows rather than to `total-1`, so `j` on
the bottom row stays in its column instead of teleporting to the last tile. `k`
does the same at the top. Every motion clamps rather than erroring; a motion that
cannot move returns the same index and the interpreter treats it as a no-op.

**A doubled operator acts on the cursor item, not on the grid row.** `dd`
archives one file; `3dd` archives three. It is tempting to make `dd` linewise in
the grid too, but which files share a row depends on the browser window width, so
`dd` at 1440px and `dd` at 390px would archive different sets of the owner's
photos. Visual-line (`V`) does snap to whole rows -- there the owner sees the
highlight before anything happens.

## 4. Operators

An operator takes a range and produces one bulk request.

| Operator | Key | On N files |
|---|---|---|
| archive | `d` | one `POST /jobs {action:"archive"}` |
| restore | `r` | one `POST /jobs {action:"restore"}`, budget pre-flight included |
| pin | `p` | one `POST /files/pin` |
| unpin | `u` | one `POST /files/pin {pinned:false}` |
| download | `y` | `window.open` per file, capped |
| tag | `t` | one bulk tag request after the range-aware picker |
| untag | `T` | one bulk tag request limited to tags present on the range |

`d` for archive because the muscle memory is real and because archiving *is* the
delete here -- it removes the file from every device Syncthing talks to. `u` stays
unpin: byte and pin operations have no generic undo model. Tag writes expose their exact
one-level inverse on `U`, with `<C-r>` redo, so their reversible contract does not overload
the existing operator.

Operators never loop per file. They resolve their range to a list of ids and go
through the same job runner and the same `bulkPin` the selection bar uses, which
is where the budget pre-flight dialog, the progress readout and the post-job
repaint already live. A keyboard path that reimplemented any of that is how one
of them ends up without the `res.ok` check.

A range wider than `CONFIRM_ABOVE = 10` files routes through `ConfirmDialog`
first, naming the count and the action. `d}` in a text editor loses a paragraph;
`dG` here can be four thousand Syncthing deletions.

### 4.1 Operator targets

```ts
type OperatorTarget =
  | { kind: "motion"; motion: MotionId; count: number }  // d3j
  | { kind: "line"; count: number }                      // dd, 3dd
  | { kind: "selection" };                               // visual mode, or a live selection
```

Resolution to a span is `motions.resolveRange`, also pure. Motion ranges are
**inclusive** at both ends -- there is no charwise/linewise distinction worth
having when the unit is a file, and an exclusive `d3j` that archives three of the
four files the eye counted is a bug report waiting to happen.

When nothing is selected, a bare operator with no motion is invalid and the
machine cancels. When a selection exists, `:archive`, `<leader>` bindings and the
context menu all target `{kind:"selection"}`.

## 5. Visual mode

`v` enters charwise visual, `V` linewise (whole grid rows). The anchor is the
cursor index at entry, held by the React layer, not the machine -- the machine
only knows that it is in visual mode.

Two departures from vim, both deliberate:

- **Entering visual snapshots the existing selection**, and the highlighted span
  is unioned onto that snapshot on every motion. Multi-select here is persistent
  state the owner has been building, not a transient region; discarding it on `v`
  would make visual mode useless for "these fifty and also that one".
- **Leaving visual commits the span.** In vim, `<Esc>` from visual leaves nothing
  selected. Here the point of `ggVG` is to end up with everything selected, so
  `<Esc>` keeps what was highlighted. `<leader>c` and `:clear` are how a selection
  goes away.

## 6. Scopes

Bindings declare which scopes they live in. The provider keeps a **scope stack**;
the top of the stack plus every non-exclusive scope beneath it is the chain that
`registry.resolve` searches, nearest first.

| Scope | Pushed by | Exclusive |
|---|---|---|
| `global` | always at the bottom | -- |
| `gallery` | the gallery view | no |
| `browser` | the file table | no |
| `lightbox` | the lightbox | no |
| `tagging` | Tag mode | no |
| `overlay` | palette, finder, cheatsheet, context menu, confirm dialog | **yes** |
| `description` | description editor | **yes** |
| `tag-prompt` | tag/untag command line | **yes** |

Exclusive means the chain stops there: while the command palette is open, `d`
types a `d` and does not archive anything. `gallery` and `browser` are mutually
exclusive in practice because only one view renders at a time, but nothing
enforces that -- the stack does not care.

The lightbox is *not* exclusive: `p`, `d`, `r`, `y` and the motions all still
mean what they mean, which is the point of having a grammar rather than a
per-component switch statement.

## 7. Text entry suspends everything

One predicate, `isTextEntryTarget`, in `keymap/textEntry.ts`, used by the single
listener:

- `<textarea>`, `<select>`, any element with `contentEditable`
- `<input>` **except** `checkbox`, `radio`, `button`, `submit`, `reset`, `file`,
  `range` and `color` -- those are controls, and the grid's own checkboxes are
  among them
- anything inside `[data-keymap-ignore]`, the escape hatch for a widget that must
  own its keys

While suspended the engine returns before normalizing, so it neither dispatches
nor calls `preventDefault`, and Ctrl+A in the budget editor selects the text in
the budget editor. Components that own text entry own their own Escape, and
Escape in a text field blurs it rather than reaching the machine. That is the
rule that keeps "type `20 GB` into a budget" from being interpreted as `2`, `0`,
`G`, `B`.

The command palette and the finder are real `<input>` elements, so they are
suspended by this rule automatically; their key handling is local to the
component, and `overlay` scope exists for the parts of them that are not the
input (the result list).

## 8. Keyboard focus and the cursor

The visible cursor is one index per view, owned by `useGridCursor`. On every
change it calls `.focus()` on the element carrying the matching `data-file-id`
and stamps `data-cursor="true"` on it. So DOM focus and the drawn cursor cannot
disagree: there is exactly one source of truth and the DOM is written from it,
never read back into it.

FileBrowser rows become `tabIndex={-1}` so they can receive that focus.
`tabIndex={-1}` rather than `0` because a table of 500 rows in the tab order is
its own accessibility failure; the grid and the table are entered with `j`/`k`
or by clicking, and Tab still walks the controls.

`scrollIntoView({block:"nearest"})` follows the focus, which is what makes
`<C-d>` and `G` usable at all.

## 9. Overlays

The palette, the finder, the cheatsheet and the context menu need what the app
did not have: a portal, a focus trap, a scroll lock and one z-index scale. They
are built once, in `components/Portal.tsx`, `hooks/useFocusTrap.ts`,
`hooks/useScrollLock.ts` and `lib/zLayers.ts`, and `ConfirmDialog` and `Lightbox`
are migrated onto them rather than left as a second implementation.

`lib/zLayers.ts` is the single source of truth for every application layer.
Components apply its `Z` values as inline `style={{ zIndex: ... }}`, not Tailwind
classes: Tailwind has no z-index theme namespace, and duplicating numeric values at
call sites would let the documented order drift. Toasts sit above every overlay so
an action failure remains visible while a palette, preview, context menu or dialog is
open.

The focus trap restores focus to whatever was focused when the overlay opened,
which is what lets `:` -> Escape put the cursor back on the tile it was on.

## 10. The binding table

`bindings.ts` is data. Each entry carries `keys`, a `title`, a `group` and its
scopes, and the `?` cheatsheet is generated by walking the same array. A
cheatsheet maintained by hand drifts from the code within a week, and a
cheatsheet that lies is worse than none.

## 11. The palette (`:`)

`palette.ts` holds the command table and two pure functions:

- `parseCommandLine(line)` -> `{ name, args, complete }`
- `completions(line, context)` -> the ranked candidate list

Argument completion is per-command: `:folder ` completes over the folder labels
the status response knows about, `:filter ` over the eight preset keys, `:view `
over the three views. `:budget 20GB` runs through the same `parseSize` the budget
editor uses, so a value the palette accepts is a value the settings panel would
have accepted, byte for byte.

`:q` closes the topmost overlay (the lightbox, or the palette itself) and `:h`
opens the cheatsheet, because a vim user will type them within the first minute
and having them do nothing is a small betrayal.

Commands that act on files use the selection when there is one and the cursor
item when there is not. That rule is stated in the palette's own hint line rather
than being folklore.

## 12. The finder (`F10`, `<leader>fe`)

Server-side `q` now performs literal substring matching across filename, relative path,
description and tags. It deliberately has no relevance ranking or FTS; fuzzy ranking remains
the client's job.

The corpus is bounded: up to `CORPUS_MAX = 2000` files for the current folder,
paged at the server's `maxPageLimit` of 500. Above that the finder says so in its
footer rather than quietly searching a fifth of the folder. Every query also fires the
extended server substring filter and merges those hits in by id, so a file outside the
window is still findable by an exact substring even when the window is full.

`fuzzy.ts` scores a subsequence match over `filename`, `relative_path`,
`folder_label`, `extension` and `status`, with bonuses for a match at a word
boundary, for consecutive runs and for a hit in the filename rather than the
path, and a penalty proportional to the gap between matched characters. It is
pure, it is where the fiddly off-by-ones live, and it is unit tested against
ordering assertions ("`IMG_2026.jpg` should outrank `a/i/m/g.txt` for `img`")
rather than against exact scores, which would freeze the tuning.

## 13. Testing

| Layer | How |
|---|---|
| `keys`, `motions`, `machine`, `registry`, `fuzzy`, `palette` | pure unit tests, no DOM |
| `KeymapProvider`, the overlays, cursor focus | vitest + Testing Library, jsdom |
| the whole thing against the real daemon | Playwright, over the seeded demo |

The grammar tests drive `step` with token arrays, which is the only way `2d3j`,
`d` + timeout, `v3jd` and `ggVG` are cheap enough to test exhaustively.

## 14. Rejected

- **A flat keymap table.** Cannot express operator-pending; the owner asked for
  the grammar explicitly.
- **A third-party library** (`hotkeys-js`, `tinykeys`, `react-hotkeys-hook`). All
  of them are chord matchers. None models an operator waiting for a range, and
  the project constraint is no new runtime dependencies for state.
- **`keydown` on a focused container instead of `window`.** It sounds tidier, but
  the focus would have to be stolen back after every action that re-renders, and
  a single window listener with an explicit suspend predicate is easier to reason
  about than an invisible focus contract.
- **Registers, marks and macros.** No use case here yet, and each one is a state field that
  every other feature then has to respect. `.` is implemented narrowly for replaying the
  last resolved tag operation; it does not create a generic command-repeat register.
- **Making Space both leader and "toggle selection"** (the tasklist's D3 wanted
  both). It cannot be both. Leader won because the owner named it; `x` toggles.

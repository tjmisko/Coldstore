import { cheatsheetGroups } from "../keymap/bindings";
import { Overlay } from "./Overlay";

/** The `?` overlay, generated from the binding table the engine dispatches on.
 *
 *  Not a hand-written list: a cheatsheet maintained separately drifts from the
 *  code within a week, and a cheatsheet that lies is worse than none. */
export function Cheatsheet({ onClose }: { onClose(): void }) {
  const groups = cheatsheetGroups();

  return (
    <Overlay label="Keyboard shortcuts" onClose={onClose} className="max-w-3xl" testAttribute="cheatsheet">
      <div className="rounded-xl border border-line-strong bg-surface shadow-2xl max-h-[80vh] overflow-y-auto">
        <div className="flex items-baseline justify-between border-b border-line px-5 py-3">
          <h2 className="text-base font-semibold">Keyboard</h2>
          <p className="text-meta text-ink-faint">
            Counts and operators compose: <code>3j</code>, <code>d3j</code>, <code>ggVG</code>.
            Leader is <code>Space</code>.
          </p>
        </div>

        <div className="grid gap-x-8 gap-y-5 p-5 md:grid-cols-2">
          {groups.map((group) => (
            <section key={group.group} data-cheatsheet-group={group.group}>
              <h3 className="mb-1.5 text-micro uppercase tracking-wider text-ink-faint">
                {group.group}
              </h3>
              <dl className="space-y-1">
                {group.bindings.map((binding) => (
                  <div key={binding.id} className="flex gap-3 text-base" data-cheatsheet-binding={binding.id}>
                    <dt className="w-24 shrink-0 font-mono text-meta text-ink pt-0.5">
                      {binding.keys}
                    </dt>
                    <dd className="flex-1 text-ink-muted">{binding.title}</dd>
                  </div>
                ))}
              </dl>
            </section>
          ))}
        </div>

        <div className="border-t border-line px-5 py-3 text-meta text-ink-faint">
          <code>:</code> opens the command palette, <code>F10</code> the fuzzy finder.
          Anything typed into a text field is left alone.
        </div>
      </div>
    </Overlay>
  );
}

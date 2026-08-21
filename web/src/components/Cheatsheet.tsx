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
      <div className="rounded-xl border border-neutral-700 bg-neutral-900 shadow-2xl max-h-[80vh] overflow-y-auto">
        <div className="flex items-baseline justify-between border-b border-neutral-800 px-5 py-3">
          <h2 className="text-base font-semibold">Keyboard</h2>
          <p className="text-xs text-neutral-500">
            Counts and operators compose: <code>3j</code>, <code>d3j</code>, <code>ggVG</code>.
            Leader is <code>Space</code>.
          </p>
        </div>

        <div className="grid gap-x-8 gap-y-5 p-5 md:grid-cols-2">
          {groups.map((group) => (
            <section key={group.group} data-cheatsheet-group={group.group}>
              <h3 className="mb-1.5 text-xs uppercase tracking-wide text-neutral-500">
                {group.group}
              </h3>
              <dl className="space-y-1">
                {group.bindings.map((binding) => (
                  <div key={binding.id} className="flex gap-3 text-sm" data-cheatsheet-binding={binding.id}>
                    <dt className="w-24 shrink-0 font-mono text-xs text-neutral-300 pt-0.5">
                      {binding.keys}
                    </dt>
                    <dd className="flex-1 text-neutral-400">{binding.title}</dd>
                  </div>
                ))}
              </dl>
            </section>
          ))}
        </div>

        <div className="border-t border-neutral-800 px-5 py-3 text-xs text-neutral-500">
          <code>:</code> opens the command palette, <code>F10</code> the fuzzy finder.
          Anything typed into a text field is left alone.
        </div>
      </div>
    </Overlay>
  );
}

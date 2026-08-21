import { useCallback, useSyncExternalStore } from "react";
import type { ActionId } from "../keymap/types";

export const QUICK_SLOT_COUNT = 9;
const EMPTY_SLOTS = Object.freeze(Array.from({ length: QUICK_SLOT_COUNT }, () => ""));
const prefix = "coldstore.quick-tags.";

interface CachedSlots {
  raw: string;
  slots: readonly string[];
}

const cache = new Map<string, CachedSlots>();
const listeners = new Map<string, Set<() => void>>();
const storageBlocked = new Set<string>();

function key(folder: string): string {
  return `${prefix}${folder}`;
}

function readRaw(folder: string): string {
  if (!folder || typeof localStorage === "undefined") return "";
  if (storageBlocked.has(folder)) return cache.get(folder)?.raw ?? "";
  try {
    return localStorage.getItem(key(folder)) ?? "";
  } catch {
    storageBlocked.add(folder);
    return cache.get(folder)?.raw ?? "";
  }
}

function parse(raw: string): readonly string[] {
  if (!raw) return EMPTY_SLOTS;
  try {
    const value = JSON.parse(raw) as unknown;
    if (!Array.isArray(value)) return EMPTY_SLOTS;
    return Object.freeze(Array.from(
      { length: QUICK_SLOT_COUNT },
      (_, index) => typeof value[index] === "string" ? value[index].trim() : "",
    ));
  } catch {
    return EMPTY_SLOTS;
  }
}

function snapshot(folder: string): readonly string[] {
  if (!folder) return EMPTY_SLOTS;
  const raw = readRaw(folder);
  const current = cache.get(folder);
  if (current?.raw === raw) return current.slots;
  const slots = parse(raw);
  cache.set(folder, { raw, slots });
  return slots;
}

function subscribe(folder: string, listener: () => void): () => void {
  if (!folder) return () => {};
  const group = listeners.get(folder) ?? new Set();
  group.add(listener);
  listeners.set(folder, group);
  return () => {
    group.delete(listener);
    if (group.size === 0) listeners.delete(folder);
  };
}

function write(folder: string, slots: readonly string[]): void {
  const normalized = Array.from(
    { length: QUICK_SLOT_COUNT },
    (_, index) => slots[index]?.trim() ?? "",
  );
  const raw = JSON.stringify(normalized);
  try {
    localStorage.setItem(key(folder), raw);
  } catch {
    storageBlocked.add(folder);
  }
  cache.set(folder, { raw, slots: Object.freeze(normalized) });
  for (const listener of listeners.get(folder) ?? []) listener();
}

export function useQuickSlots(folder: string) {
  const slots = useSyncExternalStore(
    (listener) => subscribe(folder, listener),
    () => snapshot(folder),
    () => EMPTY_SLOTS,
  );

  const setSlot = useCallback((slot: number, name: string) => {
    if (!folder || slot < 1 || slot > QUICK_SLOT_COUNT) return;
    const next = [...snapshot(folder)];
    next[slot - 1] = name;
    write(folder, next);
  }, [folder]);

  return { slots, setSlot };
}

export function quickSlotFromAction(action: ActionId): number | null {
  const match = /^quick-tag-([1-9])$/.exec(action);
  return match ? Number(match[1]) : null;
}

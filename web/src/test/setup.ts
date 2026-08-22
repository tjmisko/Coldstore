import "@testing-library/jest-dom/vitest";
import { cleanup } from "@testing-library/react";
import { afterEach } from "vitest";

// Every test gets a clean document. Without this, a component left mounted by
// one test answers the next test's queries.
afterEach(() => {
  cleanup();
  localStorage.removeItem("coldstore.workspace.v1");
});

// jsdom has no IntersectionObserver, and the Gallery and the FileBrowser both
// hang their infinite scroll off one. The stub observes nothing: paging is
// driven explicitly in the tests that care about it.
class NoopIntersectionObserver implements IntersectionObserver {
  readonly root = null;
  readonly rootMargin = "";
  readonly thresholds: ReadonlyArray<number> = [];
  disconnect() {}
  observe() {}
  unobserve() {}
  takeRecords(): IntersectionObserverEntry[] {
    return [];
  }
}
globalThis.IntersectionObserver = NoopIntersectionObserver as unknown as typeof IntersectionObserver;

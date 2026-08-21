/** Warms the lazily-loaded document readers in the background.
 *
 *  Called once the app is up and idle, so the first document the owner opens
 *  does not wait on a 1.5 MB fetch -- but the fetch is never on the path to
 *  first paint. The daemon is on a LAN, so this is a fast local request.
 *
 *  Fire-and-forget on purpose: a prefetch that fails costs nothing, because
 *  React.lazy simply fetches the chunk again on demand.
 *
 *  It lives in its own module rather than beside the components because a file
 *  that exports both components and plain functions breaks Vite's fast
 *  refresh. */
export function prefetchReaders(): void {
  void import("./PdfReader").catch(() => {});
  void import("./EpubReader").catch(() => {});
  void import("./TextReader").catch(() => {});
}

import { useEffect, useState } from "react";

interface TextReaderProps {
  /** The inline preview URL: the download route sets
   *  Content-Disposition: attachment. */
  src: string;
  filename: string;
}

interface TextState {
  body: string;
  truncated: boolean;
  error: string;
  loading: boolean;
}

/** The header the preview route sets when it stopped at the byte cap. */
const TRUNCATED_HEADER = "X-Coldstore-Truncated";

/** Plain text, including files with no extension at all -- the browser
 *  bookmark exports and loose notes this whole feature started from.
 *
 *  Whether a file IS text is decided in Go, never here: the server sniffs
 *  magic numbers and then applies git's heuristic, and caps the body. The
 *  client's job is to render what arrives and say when it is partial.
 *
 *  No syntax highlighting: highlight.js core plus grammars is 80-150 kB for
 *  content that is bookmarks and notes. */
export default function TextReader({ src, filename }: TextReaderProps) {
  const [state, setState] = useState<TextState>({
    body: "",
    truncated: false,
    error: "",
    loading: true,
  });

  useEffect(() => {
    const controller = new AbortController();
    setState({ body: "", truncated: false, error: "", loading: true });

    (async () => {
      try {
        const res = await fetch(src, { signal: controller.signal });
        if (!res.ok) {
          throw new Error(`The preview could not be loaded (${res.status}).`);
        }
        const body = await res.text();
        // headers.get is case-insensitive per the Fetch spec, so the exact
        // casing the daemon sends does not matter.
        const truncated = res.headers.get(TRUNCATED_HEADER) === "true";
        setState({ body, truncated, error: "", loading: false });
      } catch (err) {
        if (controller.signal.aborted) return;
        setState({
          body: "",
          truncated: false,
          error: err instanceof Error ? err.message : "The preview could not be loaded.",
          loading: false,
        });
      }
    })();

    // Arrowing through a folder must not leave a fetch per file in flight.
    return () => controller.abort();
  }, [src]);

  if (state.loading) {
    return (
      <p data-text-loading className="text-ink-muted text-base py-12">
        Loading {filename}...
      </p>
    );
  }

  if (state.error) {
    return (
      <div data-text-error className="text-center px-6">
        <p className="text-ink">{filename}</p>
        <p className="text-alarm text-base mt-2">{state.error}</p>
      </div>
    );
  }

  return (
    <div data-text-reader className="w-full max-w-3xl flex flex-col gap-2">
      <pre
        data-text-body
        className="whitespace-pre-wrap break-words font-mono text-meta text-ink bg-surface/80 border border-line rounded-md p-4 overflow-auto max-h-[68vh]"
      >
        {state.body}
      </pre>
      {state.truncated && (
        <p data-text-truncated className="text-meta text-ink-muted">
          This preview stops early. Download the file to read the rest.
        </p>
      )}
    </div>
  );
}

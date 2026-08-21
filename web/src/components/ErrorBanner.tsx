interface ErrorBannerProps {
  message: string;
  status: number | null;
}

/** The top-level API failure banner.
 *
 *  Without it, any failure of GET /status produced an empty sidebar, a null
 *  StatusBar and "Select a folder from the sidebar" -- an empty install and a
 *  rejected password looked exactly the same, and neither said anything. */
export function ErrorBanner({ message, status }: ErrorBannerProps) {
  const explanation =
    status === 401
      ? "Check the username and password in coldstore.toml, or the COLDSTORE_USERNAME / COLDSTORE_PASSWORD environment variables."
      : status === null
        ? "coldstore API unreachable -- the daemon may not be running, or this page is talking to the wrong address."
        : "The daemon answered, but not with what the control plane expected.";

  return (
    <div role="alert" className="bg-alarm-dim border-b border-alarm/45 px-4 py-2.5 text-base">
      <p className="font-medium text-ink">
        {status === 401 ? "Not authorised" : "Cannot reach coldstore"}
        <span className="font-normal text-alarm"> &mdash; {message}</span>
      </p>
      <p className="text-ink-muted text-meta mt-1">{explanation}</p>
    </div>
  );
}

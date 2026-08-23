import { useEffect, useMemo, useRef, useState, type FormEvent } from "react";
import { useJob } from "../hooks/useJob";
import { api, errorMessage } from "../lib/api";
import { useToast } from "../lib/toast";
import type { FileRecord } from "../types/api";
import { Overlay } from "./Overlay";

interface RenameFileDialogProps {
  file: FileRecord;
  onRenamed(): void | Promise<void>;
  onClose(): void;
}

function validateFilename(name: string, current: string): string | null {
  if (name === "") return "Enter a file name.";
  if (name === "." || name === "..") return "That name is reserved.";
  if (name.includes("/") || name.includes("\\")) return "Enter a name, not a path.";
  if ([...name].some((character) => character.charCodeAt(0) < 32 || character.charCodeAt(0) === 127)) {
    return "File names cannot contain control characters.";
  }
  if (name === current) return "Enter a different file name.";
  return null;
}

/** Rename one file without exposing the path-moving endpoint's broader
 * directory semantics. The existing parent directory is preserved exactly. */
export function RenameFileDialog({ file, onRenamed, onClose }: RenameFileDialogProps) {
  const [name, setName] = useState(file.filename);
  const [submitError, setSubmitError] = useState<string | null>(null);
  const [warnings, setWarnings] = useState<string[]>([]);
  const [jobId, setJobId] = useState<number | null>(null);
  const input = useRef<HTMLInputElement>(null);
  const settled = useRef<number | null>(null);
  const toast = useToast();
  const { job, error: pollError } = useJob(jobId);
  const validationError = useMemo(() => validateFilename(name, file.filename), [file.filename, name]);
  const running = jobId !== null && (
    !job || job.id !== jobId || job.state === "pending" || job.state === "running"
  );
  const jobFailure = jobId !== null && job?.id === jobId && job.state === "failed"
    ? job.error || job.failed[0]?.error || "The rename job failed."
    : null;

  useEffect(() => {
    input.current?.select();
  }, []);

  useEffect(() => {
    if (!job || job.id !== jobId || settled.current === job.id) return;
    if (job.state !== "done") return;
    settled.current = job.id;
    void Promise.resolve(onRenamed()).then(() => {
      toast.show("success", `Renamed ${file.filename} to ${name}`);
      onClose();
    });
  }, [file.filename, job, jobId, name, onClose, onRenamed, toast]);

  const submit = async (event: FormEvent) => {
    event.preventDefault();
    if (validationError || running) {
      setSubmitError(validationError);
      return;
    }
    setSubmitError(null);
    const slash = file.relative_path.lastIndexOf("/");
    const destination = `${slash === -1 ? "" : file.relative_path.slice(0, slash + 1)}${name}`;
    try {
      const created = await api.renamePath(file.folder_label, file.relative_path, destination);
      settled.current = null;
      setWarnings(created.warnings);
      setJobId(created.job_id);
      if (created.warnings.length > 0) {
        toast.show("info", "This rename affects one or more persistent path rules; review them in Settings.");
      }
    } catch (err) {
      setSubmitError(errorMessage(err));
    }
  };

  const close = () => {
    if (!running) onClose();
  };

  return (
    <Overlay label={`Rename ${file.filename}`} onClose={close} className="max-w-md" testAttribute="rename-file">
      <form onSubmit={(event) => void submit(event)} className="rounded-xl border border-line-strong bg-surface p-5 shadow-2xl">
        <h2 className="text-base font-semibold text-ink">Rename file</h2>
        <p className="mt-1 truncate text-meta text-ink-faint" title={`${file.folder_label}/${file.relative_path}`}>
          {file.folder_label}/{file.relative_path}
        </p>

        <label className="mt-4 block text-meta font-medium text-ink-muted" htmlFor={`rename-file-${file.id}`}>
          New name
        </label>
        <input
          ref={input}
          id={`rename-file-${file.id}`}
          aria-label={`New name for ${file.filename}`}
          value={name}
          disabled={running}
          onChange={(event) => {
            setName(event.target.value);
            setSubmitError(null);
          }}
          className="mt-1 w-full rounded-lg border border-line-strong bg-ground px-3 py-2 text-base text-ink outline-none focus:border-cursor focus:ring-2 focus:ring-cursor/40 disabled:opacity-60"
        />

        {(submitError || jobFailure || pollError) && (
          <p role="alert" className="mt-2 text-meta text-alarm">
            {submitError || jobFailure || `Waiting for the rename job: ${pollError}`}
          </p>
        )}
        {warnings.length > 0 && (
          <p className="mt-2 text-meta text-ink-muted">
            Review affected persistent rules in Settings: {warnings.join(", ")}
          </p>
        )}

        <footer className="mt-5 flex justify-end gap-2">
          <button
            type="button"
            onClick={close}
            disabled={running}
            className="rounded-md px-3 py-1.5 text-ink-muted ring-1 ring-line-strong hover:bg-raised-hover disabled:opacity-40"
          >
            Cancel
          </button>
          <button
            type="submit"
            disabled={running || validationError !== null}
            className="rounded-md bg-ink px-3 py-1.5 font-medium text-ground hover:brightness-95 disabled:cursor-not-allowed disabled:opacity-50"
          >
            {running ? "Renaming…" : "Rename"}
          </button>
        </footer>
      </form>
    </Overlay>
  );
}

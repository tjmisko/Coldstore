import { useCallback, useEffect, useState } from "react";
import { ApiRequestError, api, errorMessage } from "../lib/api";
import type { BudgetPreflightError, JobDetail, JobRequest } from "../types/api";

const POLL_INTERVAL_MS = 1000;

/** useJob polls one job until it settles.
 *
 *  A chained setTimeout rather than setInterval: a slow response cannot stack
 *  a second poll on top of the first, and there is exactly one timer to clear
 *  when StrictMode unmounts and remounts. GET /jobs/{id} is idempotent, so the
 *  duplicated first poll is harmless. */
export function useJob(jobId: number | null) {
  const [job, setJob] = useState<JobDetail | null>(null);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    if (jobId === null) return;
    let cancelled = false;
    let timer: ReturnType<typeof setTimeout> | undefined;

    const poll = async () => {
      try {
        const next = await api.job(jobId);
        if (cancelled) return;
        setJob(next);
        setError(null);
        if (next.state === "done" || next.state === "failed") return;
        timer = setTimeout(() => void poll(), POLL_INTERVAL_MS);
      } catch (err) {
        if (cancelled) return;
        setError(errorMessage(err));
        // Keep polling through a transient failure: a job that is running is
        // still running whether or not this request reached the daemon.
        timer = setTimeout(() => void poll(), POLL_INTERVAL_MS);
      }
    };

    void poll();
    return () => {
      cancelled = true;
      if (timer) clearTimeout(timer);
    };
  }, [jobId]);

  return { job, error };
}

export interface JobRunner {
  jobId: number | null;
  job: JobDetail | null;
  error: string | null;
  /** The 409 body from the restore budget pre-flight, or null. The caller
   *  turns it into a confirm dialog and re-submits with force. */
  preflight: BudgetPreflightError | null;
  running: boolean;
  start(req: JobRequest): Promise<void>;
  dismiss(): void;
}

/** useJobRunner is enqueue plus poll.
 *
 *  start() is only ever called from an event handler. Creating a job from an
 *  effect would post it twice under StrictMode -- and a duplicated archive job
 *  is a duplicated set of Syncthing deletions. */
export function useJobRunner(): JobRunner {
  const [jobId, setJobId] = useState<number | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [preflight, setPreflight] = useState<BudgetPreflightError | null>(null);
  const { job, error: pollError } = useJob(jobId);

  const start = useCallback(async (req: JobRequest) => {
    setError(null);
    setPreflight(null);
    try {
      const created = await api.createJob(req);
      setJobId(created.job_id);
    } catch (err) {
      if (err instanceof ApiRequestError && err.status === 409 && isPreflight(err.body)) {
        setPreflight(err.body);
        return;
      }
      setError(errorMessage(err));
    }
  }, []);

  const dismiss = useCallback(() => {
    setJobId(null);
    setError(null);
    setPreflight(null);
  }, []);

  // job is scoped to the CURRENT jobId. useJob keeps its last payload when the
  // id goes null -- it has no legal way to clear state from an effect -- so
  // without this the summary of a dismissed job would stay on screen forever.
  const currentJob = jobId === null ? null : job;

  return {
    jobId,
    job: currentJob,
    error: error ?? pollError,
    preflight,
    running:
      jobId !== null &&
      (currentJob === null || currentJob.state === "pending" || currentJob.state === "running"),
    start,
    dismiss,
  };
}

function isPreflight(body: unknown): body is BudgetPreflightError {
  if (typeof body !== "object" || body === null) return false;
  const candidate = body as Partial<BudgetPreflightError>;
  return typeof candidate.folder === "string" && typeof candidate.overage_bytes === "number";
}

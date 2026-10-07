import { useCallback, useEffect, useState, type ReactNode } from "react";

/** Loads data with fn and reloads it on demand; errors are shown inline. */
export function useLoad<T>(fn: () => Promise<T>) {
  const [data, setData] = useState<T | undefined>();
  const [error, setError] = useState<string | undefined>();
  const [loading, setLoading] = useState(true);
  const reload = useCallback(async () => {
    setLoading(true);
    try {
      setData(await fn());
      setError(undefined);
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e));
    } finally {
      setLoading(false);
    }
    // fn is stable per page (defined at module level or memoised)
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);
  useEffect(() => {
    void reload();
  }, [reload]);
  return { data, error, loading, reload };
}

/** Runs an action, reporting its error, then calls after (a reload). */
export function useAction() {
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | undefined>();
  const run = useCallback(async (f: () => Promise<unknown>, after?: () => unknown) => {
    setBusy(true);
    setError(undefined);
    try {
      await f();
      await after?.();
      return true;
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e));
      return false;
    } finally {
      setBusy(false);
    }
  }, []);
  return { busy, error, run, clearError: () => setError(undefined) };
}

export function PageHeader({ title, children }: { title: string; children?: ReactNode }) {
  return (
    <header className="page-header">
      <h1>{title}</h1>
      <div className="actions">{children}</div>
    </header>
  );
}

export function ErrorNote({ error }: { error?: string }) {
  return error ? <p className="error" role="alert">{error}</p> : null;
}

export function Toggle({ checked, onChange, label, disabled }: {
  checked: boolean;
  onChange: (v: boolean) => void;
  label: string;
  disabled?: boolean;
}) {
  return (
    <label className="toggle">
      <input type="checkbox" checked={checked} disabled={disabled} onChange={(e) => onChange(e.target.checked)} />
      <span>{label}</span>
    </label>
  );
}

export function timeAgo(iso?: string): string {
  if (!iso) return "never";
  const t = new Date(iso).getTime();
  if (!t || t < 0) return "never";
  const s = Math.round((Date.now() - t) / 1000);
  if (s < 60) return "just now";
  if (s < 3600) return `${Math.round(s / 60)} min ago`;
  if (s < 86400) return `${Math.round(s / 3600)} h ago`;
  return `${Math.round(s / 86400)} d ago`;
}

import { useEffect, useMemo, useRef, useState } from "react";
import { api, type LogEntry, type LogsResponse } from "../api";
import { ErrorNote, PageHeader, Toggle, useAction } from "../ui";

const levels = ["DEBUG", "INFO", "WARN", "ERROR"] as const;
const keep = 5000; // as many as the server buffers
const shown = 1000; // rows rendered at once

const rank = (level: string) => {
  const i = levels.findIndex((l) => level.startsWith(l));
  return i < 0 ? 1 : i;
};
const badge: Record<string, string> = { DEBUG: "off", INFO: "", WARN: "warn", ERROR: "bad" };

function line(e: LogEntry): string {
  const attrs = (e.Attrs ?? []).map((a) => `${a.Key}=${/\s/.test(a.Value) ? JSON.stringify(a.Value) : a.Value}`);
  return [e.Time, e.Level, JSON.stringify(e.Message), ...attrs].join(" ");
}

export default function Logs() {
  const [entries, setEntries] = useState<LogEntry[]>([]);
  const [capture, setCapture] = useState<string>();
  const [error, setError] = useState<string>();
  const [minLevel, setMinLevel] = useState("DEBUG");
  const [query, setQuery] = useState("");
  const [paused, setPaused] = useState(false);
  const last = useRef(0);
  const act = useAction();

  useEffect(() => {
    if (paused) return;
    let live = true;
    const poll = async () => {
      try {
        const r = await api<LogsResponse>("GET", `/blockbustr/logs?after=${last.current}&limit=${keep}`);
        if (!live) return;
        setCapture(r.Level);
        setError(undefined);
        if (r.Entries.length) {
          last.current = r.Entries[r.Entries.length - 1].Seq;
          setEntries((old) => [...old, ...r.Entries].slice(-keep));
        }
      } catch (e) {
        if (live) setError(e instanceof Error ? e.message : String(e));
      }
    };
    void poll();
    const id = window.setInterval(poll, 2000);
    return () => {
      live = false;
      window.clearInterval(id);
    };
  }, [paused]);

  const visible = useMemo(() => {
    const min = rank(minLevel);
    const q = query.trim().toLowerCase();
    const out: LogEntry[] = [];
    for (let i = entries.length - 1; i >= 0 && out.length < shown; i--) {
      const e = entries[i];
      if (rank(e.Level) < min) continue;
      if (q && !line(e).toLowerCase().includes(q)) continue;
      out.push(e);
    }
    return out;
  }, [entries, minLevel, query]);

  const download = () => {
    const blob = new Blob([entries.map(line).join("\n") + "\n"], { type: "text/plain" });
    const a = document.createElement("a");
    a.href = URL.createObjectURL(blob);
    a.download = `blockbustr-${new Date().toISOString().replace(/[:.]/g, "-")}.log`;
    a.click();
    URL.revokeObjectURL(a.href);
  };

  return (
    <>
      <PageHeader title="Logs">
        <Toggle label="Pause" checked={paused} onChange={setPaused} />
        <button onClick={() => setEntries([])} disabled={!entries.length}>Clear</button>
        <button onClick={download} disabled={!entries.length}>Download</button>
      </PageHeader>
      <ErrorNote error={error ?? act.error} />
      <div className="card">
        <div className="row" style={{ justifyContent: "space-between" }}>
          <label className="row">
            Capture
            <select
              value={capture ?? ""}
              disabled={!capture || act.busy}
              onChange={(e) => {
                const level = e.target.value;
                void act.run(() => api("POST", "/blockbustr/logs/level", { Level: level }), () => setCapture(level));
              }}
            >
              {levels.map((l) => <option key={l} value={l}>{l.toLowerCase()}</option>)}
            </select>
          </label>
          <div className="row">
            <select value={minLevel} onChange={(e) => setMinLevel(e.target.value)} aria-label="Show level">
              {levels.map((l) => <option key={l} value={l}>{l.toLowerCase()} and up</option>)}
            </select>
            <input type="text" placeholder="Search (path, item id, error…)" value={query} onChange={(e) => setQuery(e.target.value)} style={{ minWidth: 220 }} />
          </div>
        </div>
        <p className="muted small">
          Set capture to <b>debug</b> to see every request while you reproduce a problem, then set it back. It changes
          only this page, not the container log, and resets on restart. Newest first; the server keeps the last {keep}.
        </p>
      </div>
      <div className="card logs">
        {visible.length === 0 && <p className="muted">{entries.length ? "Nothing matches." : "No log lines yet."}</p>}
        {visible.map((e) => (
          <div key={e.Seq} className="log-line">
            <span className="muted">{new Date(e.Time).toLocaleTimeString()}</span>{" "}
            <span className={`badge ${badge[levels[rank(e.Level)]]}`}>{e.Level}</span>{" "}
            <span>{e.Message}</span>
            {e.Attrs?.map((a, i) => (
              <span key={i} className="log-attr"> <span className="muted">{a.Key}=</span>{a.Value}</span>
            ))}
          </div>
        ))}
      </div>
    </>
  );
}

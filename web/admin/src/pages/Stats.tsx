import { useEffect, useRef, useState } from "react";
import { api } from "../api";
import { bytes, ErrorNote, PageHeader } from "../ui";

// GET /blockbustr/stats: the server's metrics, summarised (internal/metrics
// Snapshot). Totals count since the server started; the page polls every
// few seconds and works out the proxy's current throughput from the change.

interface Timing {
  Name: string;
  Count: number;
  Errors: number;
  AvgMs: number;
  P95Ms: number;
  P95Bound?: boolean; // in the first bucket: at most P95Ms
}

interface Stats {
  UptimeSeconds: number;
  ActiveStreams: number;
  ProxiedBytes: number;
  Transcodes: number;
  DebridAccounts: number;
  DBInUse: number;
  DBIdle: number;
  HeapBytes: number;
  Goroutines: number;
  Requests: Timing[] | null;
  Addons: Timing[] | null;
  Resolves: { Name: string; Count: number }[] | null;
  Search: Timing;
}

const POLL_MS = 5000;
// Streams and HLS segments last as long as the client reads, so their
// timings say nothing about the server's speed; the admin UI's own files
// and unrouted requests are noise.
const HIDDEN_ROUTES = /\/(stream|hls|Videos|Audio)\b|^\/blockbustr\/ui|^unmatched$/i;

function uptime(s: number): string {
  const d = Math.floor(s / 86400);
  const h = Math.floor((s % 86400) / 3600);
  const m = Math.floor((s % 3600) / 60);
  return d > 0 ? `${d} d ${h} h` : h > 0 ? `${h} h ${m} min` : `${m} min`;
}

const ms = (v: number) => (v >= 1000 ? `${(v / 1000).toFixed(1)} s` : `${v} ms`);

function Stat({ label, value, note }: { label: string; value: string | number; note?: string }) {
  return (
    <div className="card stat">
      <div className="muted small">{label}</div>
      <div className="value">{value}</div>
      {note && <div className="muted small">{note}</div>}
    </div>
  );
}

function Timings({ title, rows, empty }: { title: string; rows: Timing[]; empty: string }) {
  return (
    <div className="card">
      <h2>{title}</h2>
      {rows.length === 0 ? (
        <p className="muted">{empty}</p>
      ) : (
        <table>
          <thead><tr><th>Name</th><th>Count</th><th>Errors</th><th>Average</th><th>95th percentile</th></tr></thead>
          <tbody>
            {rows.map((r) => (
              <tr key={r.Name}>
                <td><code>{r.Name || "—"}</code></td>
                <td>{r.Count}</td>
                <td className={r.Errors > 0 ? "error" : "muted"}>{r.Errors}</td>
                <td>{ms(r.AvgMs)}</td>
                <td>{r.P95Bound ? `≤ ${ms(r.P95Ms)}` : ms(r.P95Ms)}</td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
    </div>
  );
}

export default function StatsPage() {
  const [stats, setStats] = useState<Stats>();
  const [rate, setRate] = useState<number>(); // proxied bytes per second
  const [error, setError] = useState<string>();
  const last = useRef<{ at: number; bytes: number }>(undefined);

  useEffect(() => {
    let stopped = false;
    const poll = async () => {
      try {
        const s = await api<Stats>("GET", "/blockbustr/stats");
        if (stopped) return;
        const now = Date.now();
        const prev = last.current;
        // A restart resets the total; skip that interval.
        if (prev && s.ProxiedBytes >= prev.bytes) setRate(((s.ProxiedBytes - prev.bytes) * 1000) / (now - prev.at));
        last.current = { at: now, bytes: s.ProxiedBytes };
        setStats(s);
        setError(undefined);
      } catch (e) {
        if (!stopped) setError(e instanceof Error ? e.message : String(e));
      }
    };
    void poll();
    const id = setInterval(poll, POLL_MS);
    return () => {
      stopped = true;
      clearInterval(id);
    };
  }, []);

  const requests = (stats?.Requests ?? []).filter((r) => !HIDDEN_ROUTES.test(r.Name)).slice(0, 15);
  return (
    <>
      <PageHeader title="Stats">
        <span className="muted small">Updates every {POLL_MS / 1000} s · totals since the server started</span>
      </PageHeader>
      <ErrorNote error={error} />
      {stats && (
        <>
          <div className="grid">
            <Stat label="Streams proxied now" value={stats.ActiveStreams} />
            <Stat
              label="Proxy throughput"
              value={rate === undefined ? "…" : `${((rate * 8) / 1e6).toFixed(1)} Mbit/s`}
              note={`${bytes(stats.ProxiedBytes)} in total`}
            />
            <Stat label="Transcodes running" value={stats.Transcodes} />
            <Stat label="Uptime" value={uptime(stats.UptimeSeconds)} />
            <Stat label="Memory (Go heap)" value={bytes(stats.HeapBytes)} note={`${stats.Goroutines} goroutines`} />
            <Stat label="Database connections" value={stats.DBInUse} note={`${stats.DBIdle} idle`} />
            <Stat label="Debrid accounts in use" value={stats.DebridAccounts} />
            <Stat
              label="Search"
              value={stats.Search.Count ? `${stats.Search.P95Bound ? "≤ " : ""}${ms(stats.Search.P95Ms)}` : "—"}
              note={stats.Search.Count ? `95th percentile of ${stats.Search.Count} searches` : "no searches yet"}
            />
          </div>
          <Timings title="Busiest requests" rows={requests} empty="No requests yet." />
          <Timings
            title="Addon requests"
            rows={stats.Addons ?? []}
            empty="No addon requests yet (cached answers aren't counted)."
          />
          <div className="card">
            <h2>Link lookups</h2>
            {(stats.Resolves ?? []).length === 0 ? (
              <p className="muted">Nothing played from a remote source yet.</p>
            ) : (
              <table>
                <thead><tr><th>Source and result</th><th>Count</th></tr></thead>
                <tbody>
                  {(stats.Resolves ?? []).map((r) => (
                    <tr key={r.Name}><td><code>{r.Name}</code></td><td>{r.Count}</td></tr>
                  ))}
                </tbody>
              </table>
            )}
          </div>
        </>
      )}
    </>
  );
}

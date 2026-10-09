import { useCallback, useEffect, useState } from "react";
import { api, type PlaybackEntry, type PlaybackResponse, type PlaybackTotal } from "../api";
import { bytes, ErrorNote, PageHeader, timeAgo } from "../ui";

// GET /blockbustr/playback: who played what, which version and addon, how
// it reached the player and what blockbustr proxied for it. Debrid usage of
// links an addon resolved with its own keys shows here only as these plays;
// the debrid service's account page has the real traffic totals.

const PERIODS = [1, 7, 30, 90, 365];
const POLL_MS = 15000;
// A play with no stop that reported within this long is still going.
const LIVE_MS = 2 * 60 * 1000;

function duration(seconds: number): string {
  const h = Math.floor(seconds / 3600);
  const m = Math.round((seconds % 3600) / 60);
  return h > 0 ? `${h} h ${m} min` : `${m} min`;
}

function watched(e: PlaybackEntry): string {
  const end = new Date(e.StoppedAt ?? e.LastSeenAt).getTime();
  const s = Math.max(0, (end - new Date(e.StartedAt).getTime()) / 1000);
  const pct = e.RuntimeTicks ? ` · ${Math.min(100, Math.round((e.PositionTicks * 100) / e.RuntimeTicks))}%` : "";
  return duration(s) + pct;
}

function status(e: PlaybackEntry): { label: string; cls: string } {
  if (e.StoppedAt) return { label: "stopped", cls: "off" };
  if (Date.now() - new Date(e.LastSeenAt).getTime() < LIVE_MS) return { label: "playing", cls: "ok" };
  return { label: "ended", cls: "off" };
}

const deliveryBadge: Record<string, string> = { proxied: "warn", redirected: "ok", local: "" };

function Totals({ rows, days, user, onPick }: {
  rows: PlaybackTotal[];
  days: number;
  user?: string;
  onPick: (id?: string) => void;
}) {
  return (
    <div className="card">
      <h2>Per user, last {days === 1 ? "24 hours" : `${days} days`}</h2>
      {rows.length === 0 ? (
        <p className="muted">Nothing played in this period.</p>
      ) : (
        <table>
          <thead><tr><th>User</th><th>Plays</th><th>Time playing</th><th>Proxied</th><th>Last played</th></tr></thead>
          <tbody>
            {rows.map((t) => (
              <tr key={t.UserID ?? t.UserName}>
                <td>
                  {t.UserID ? (
                    <button className="link" onClick={() => onPick(user === t.UserID ? undefined : t.UserID)}>
                      {user === t.UserID ? <b>{t.UserName}</b> : t.UserName}
                    </button>
                  ) : (
                    <span className="muted">{t.UserName} (deleted)</span>
                  )}
                </td>
                <td>{t.Plays}</td>
                <td>{duration(t.Seconds)}</td>
                <td>{bytes(t.Bytes)}</td>
                <td>{timeAgo(t.LastPlayed)}</td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
    </div>
  );
}

export default function PlaybackPage() {
  const [days, setDays] = useState(30);
  const [user, setUser] = useState<string>();
  const [data, setData] = useState<PlaybackResponse>();
  const [older, setOlder] = useState<PlaybackEntry[]>([]);
  const [more, setMore] = useState(false);
  const [error, setError] = useState<string>();

  const query = useCallback(
    (before = 0) => `/blockbustr/playback?days=${days}${user ? `&userId=${user}` : ""}${before ? `&before=${before}` : ""}`,
    [days, user],
  );

  // The newest page, polled; older pages are loaded on demand and kept.
  useEffect(() => {
    let live = true;
    setOlder([]);
    setMore(false);
    const poll = async () => {
      try {
        const r = await api<PlaybackResponse>("GET", query());
        if (!live) return;
        setData(r);
        setMore((m) => m || r.More);
        setError(undefined);
      } catch (e) {
        if (live) setError(e instanceof Error ? e.message : String(e));
      }
    };
    void poll();
    const id = window.setInterval(poll, POLL_MS);
    return () => {
      live = false;
      window.clearInterval(id);
    };
  }, [query]);

  const entries = [...(data?.Entries ?? []), ...older.filter((o) => !data?.Entries.some((e) => e.ID === o.ID))];
  const loadOlder = async () => {
    const last = entries[entries.length - 1];
    if (!last) return;
    try {
      const r = await api<PlaybackResponse>("GET", query(last.ID));
      setOlder((o) => [...o, ...r.Entries]);
      setMore(r.More);
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e));
    }
  };
  const userName = data?.Totals.find((t) => t.UserID === user)?.UserName ?? entries.find((e) => e.UserID === user)?.UserName;

  return (
    <>
      <PageHeader title="Playback">
        <select value={days} onChange={(e) => setDays(Number(e.target.value))} aria-label="Totals period">
          {PERIODS.map((d) => <option key={d} value={d}>{d === 1 ? "Last 24 hours" : `Last ${d} days`}</option>)}
        </select>
      </PageHeader>
      <ErrorNote error={error} />
      {data && (
        <>
          <Totals rows={data.Totals} days={data.Days} user={user} onPick={setUser} />
          <div className="card">
            <div className="row" style={{ justifyContent: "space-between" }}>
              <h2>{user ? `Plays by ${userName ?? "this user"}` : "All plays"}</h2>
              {user && <button onClick={() => setUser(undefined)}>Show everyone</button>}
            </div>
            <p className="muted small">
              <b>Proxied</b> streams pass through blockbustr and their bytes are counted. <b>Redirected</b> ones go
              straight from the source (a trusted stream proxy, or a link the app may fetch itself) and aren't. Debrid
              links an addon resolved with its own keys count against that addon's debrid account.
            </p>
            {entries.length === 0 ? (
              <p className="muted">No plays logged yet.</p>
            ) : (
              <table>
                <thead>
                  <tr><th>Started</th><th>User · device</th><th>Title</th><th>Version</th><th>Delivery</th><th>Watched</th><th>Proxied</th></tr>
                </thead>
                <tbody>
                  {entries.map((e) => {
                    const st = status(e);
                    return (
                      <tr key={e.ID}>
                        <td title={new Date(e.StartedAt).toLocaleString()}>
                          {timeAgo(e.StartedAt)} <span className={`badge ${st.cls}`}>{st.label}</span>
                        </td>
                        <td>
                          {e.UserName}
                          <div className="muted small">{[e.DeviceName, e.Client].filter(Boolean).join(" · ") || "—"}</div>
                        </td>
                        <td>{e.ItemName}</td>
                        <td>
                          {e.SourceName || <span className="muted">—</span>}
                          {e.Addon && <div className="muted small">{e.Addon}</div>}
                        </td>
                        <td>
                          {e.Delivery ? <span className={`badge ${deliveryBadge[e.Delivery] ?? ""}`}>{e.Delivery}</span> : <span className="muted">—</span>}
                          {e.PlayMethod && <div className="muted small">{e.PlayMethod}</div>}
                          {e.LinkHost && <div className="muted small"><code>{e.LinkHost}</code></div>}
                        </td>
                        <td>{watched(e)}</td>
                        <td>{e.Delivery === "proxied" ? bytes(e.Bytes) : <span className="muted">—</span>}</td>
                      </tr>
                    );
                  })}
                </tbody>
              </table>
            )}
            {more && <button onClick={loadOlder}>Load older</button>}
          </div>
        </>
      )}
    </>
  );
}

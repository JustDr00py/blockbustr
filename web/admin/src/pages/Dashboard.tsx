import { api, type Session } from "../api";
import { ErrorNote, PageHeader, timeAgo, useLoad } from "../ui";

interface Info {
  ServerName: string;
  Version: string;
}

async function load() {
  const [info, sessions, users, addons] = await Promise.all([
    api<Info>("GET", "/System/Info"),
    api<Session[]>("GET", "/Sessions"),
    api<unknown[]>("GET", "/Users"),
    api<{ Enabled: boolean }[]>("GET", "/blockbustr/addons").catch(() => []),
  ]);
  return { info, sessions, users: users.length, addons: addons.filter((a) => a.Enabled).length };
}

const ticks = (t?: number) => {
  if (!t) return "";
  const s = Math.floor(t / 1e7);
  return `${Math.floor(s / 3600)}:${String(Math.floor((s % 3600) / 60)).padStart(2, "0")}:${String(s % 60).padStart(2, "0")}`;
};

export default function Dashboard() {
  const { data, error, reload, loading } = useLoad(load);
  return (
    <>
      <PageHeader title="Dashboard">
        <button onClick={reload} disabled={loading}>Refresh</button>
      </PageHeader>
      <ErrorNote error={error} />
      {data && (
        <>
          <div className="grid">
            <div className="card stat"><div className="muted small">Server</div><div className="value">{data.info.ServerName}</div><div className="muted small">reports Jellyfin {data.info.Version}</div></div>
            <div className="card stat"><div className="muted small">Active devices</div><div className="value">{data.sessions.length}</div></div>
            <div className="card stat"><div className="muted small">Users</div><div className="value">{data.users}</div></div>
            <div className="card stat"><div className="muted small">Enabled addons</div><div className="value">{data.addons}</div></div>
          </div>
          <div className="card">
            <h2>Devices</h2>
            {data.sessions.length === 0 ? (
              <p className="muted">No devices active recently.</p>
            ) : (
              <table>
                <thead><tr><th>Device</th><th>User</th><th>Playing</th><th>Last seen</th></tr></thead>
                <tbody>
                  {data.sessions.map((s) => (
                    <tr key={s.Id}>
                      <td>{s.DeviceName}<div className="muted small">{s.Client} {s.ApplicationVersion}</div></td>
                      <td>{s.UserName ?? ""}</td>
                      <td>
                        {s.NowPlayingItem ? (
                          <>
                            {s.NowPlayingItem.SeriesName ? `${s.NowPlayingItem.SeriesName} · ` : ""}{s.NowPlayingItem.Name}
                            <div className="muted small">
                              {s.PlayState?.IsPaused ? "paused" : "playing"} {ticks(s.PlayState?.PositionTicks)}
                              {s.PlayState?.PlayMethod ? ` · ${s.PlayState.PlayMethod}` : ""}
                            </div>
                          </>
                        ) : <span className="muted">—</span>}
                      </td>
                      <td className="muted">{timeAgo(s.LastActivityDate)}</td>
                    </tr>
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

import { useState } from "react";
import { api, type Addon } from "../api";
import { ErrorNote, PageHeader, Toggle, timeAgo, useAction, useLoad } from "../ui";

const load = () => api<Addon[]>("GET", "/blockbustr/addons");

export default function Addons() {
  const { data, error, reload } = useLoad(load);
  const act = useAction();
  const [url, setUrl] = useState("");

  return (
    <>
      <PageHeader title="Addons" />
      <ErrorNote error={error ?? act.error} />
      <form
        className="card row"
        onSubmit={(e) => {
          e.preventDefault();
          void act.run(() => api("POST", "/blockbustr/addons", { Url: url.trim() }), async () => {
            setUrl("");
            await reload();
          });
        }}
      >
        <input type="url" placeholder="https://…/manifest.json or stremio://…" value={url} onChange={(e) => setUrl(e.target.value)} style={{ flex: 1, minWidth: 220 }} />
        <button className="primary" type="submit" disabled={act.busy || !url.trim()}>Add addon</button>
        <p className="muted small" style={{ flexBasis: "100%", margin: 0 }}>
          Addon URLs often contain your debrid key: they're stored encrypted and never shown again.
        </p>
      </form>
      {data?.length === 0 && <p className="muted">No addons yet.</p>}
      {data?.map((a) => <AddonCard key={a.Id} addon={a} onChanged={reload} />)}
    </>
  );
}

function AddonCard({ addon, onChanged }: { addon: Addon; onChanged: () => Promise<void> }) {
  const act = useAction();
  const [priority, setPriority] = useState(addon.Priority);
  const [newUrl, setNewUrl] = useState("");
  const update = (body: object) => act.run(() => api("POST", `/blockbustr/addons/${addon.Id}`, body), onChanged);
  const usable = addon.Catalogs.filter((c) => c.Requires.length === 0 && (c.Type === "movie" || c.Type === "series"));

  return (
    <div className="card">
      <div className="row" style={{ justifyContent: "space-between" }}>
        <div>
          <h2 style={{ margin: 0 }}>
            {addon.Name} <span className="muted small">{addon.Version}</span>{" "}
            {addon.Enabled ? <span className="badge ok">enabled</span> : <span className="badge off">disabled</span>}
          </h2>
          <div className="muted small">{addon.Host} · {addon.Resources.join(", ")} · checked {timeAgo(addon.LastFetchedAt)}</div>
        </div>
        <div className="row">
          <Toggle label="Enabled" checked={addon.Enabled} disabled={act.busy} onChange={(v) => update({ Enabled: v })} />
          <label className="row small">
            Priority
            <input type="number" value={priority} onChange={(e) => setPriority(Number(e.target.value))} onBlur={() => priority !== addon.Priority && update({ Priority: priority })} />
          </label>
          <button disabled={act.busy} onClick={() => act.run(() => api("POST", `/blockbustr/addons/${addon.Id}/refresh`), onChanged)}>Refresh</button>
          <button
            className="danger"
            disabled={act.busy}
            onClick={() => window.confirm(`Remove ${addon.Name}? Its catalog libraries stop syncing.`) && act.run(() => api("DELETE", `/blockbustr/addons/${addon.Id}`), onChanged)}
          >
            Remove
          </button>
        </div>
      </div>
      {addon.Description && <p className="muted small">{addon.Description}</p>}
      <ErrorNote error={act.error} />
      <details>
        <summary>Change URL</summary>
        <form
          className="row"
          style={{ marginTop: 8 }}
          onSubmit={(e) => {
            e.preventDefault();
            void act.run(() => api("POST", `/blockbustr/addons/${addon.Id}`, { Url: newUrl.trim() }), async () => {
              setNewUrl("");
              await onChanged();
            });
          }}
        >
          <input
            type="url"
            placeholder="New manifest URL (e.g. after reconfiguring it, or a new debrid key)"
            value={newUrl}
            onChange={(e) => setNewUrl(e.target.value)}
            style={{ flex: 1, minWidth: 220 }}
          />
          <button className="primary" type="submit" disabled={act.busy || !newUrl.trim()}>Save URL</button>
          <p className="muted small" style={{ flexBasis: "100%", margin: 0 }}>
            The current URL isn't shown, as it may hold your debrid key. Catalogs, libraries and settings are kept; the new
            URL must be the same addon.
          </p>
        </form>
      </details>
      {usable.length > 0 && (
        <details>
          <summary>Catalogs ({usable.filter((c) => c.Enabled).length} of {usable.length} shown as libraries)</summary>
          <div className="catalogs">
            {usable.map((c) => (
              <Toggle
                key={c.Type + c.Id}
                label={`${c.Name} (${c.Type === "series" ? "shows" : "movies"})`}
                checked={c.Enabled}
                disabled={act.busy}
                onChange={(v) => act.run(() => api("POST", `/blockbustr/addons/${addon.Id}/catalogs/${c.Type}/${encodeURIComponent(c.Id)}`, { Enabled: v }), onChanged)}
              />
            ))}
          </div>
        </details>
      )}
    </div>
  );
}

import { useState } from "react";
import { api, type AdminLibrary } from "../api";
import { ErrorNote, PageHeader, Toggle, useAction, useLoad } from "../ui";

const load = () => api<AdminLibrary[]>("GET", "/blockbustr/libraries");

export default function Libraries() {
  const { data, error, reload } = useLoad(load);
  const act = useAction();
  const [scanNote, setScanNote] = useState<string>();
  const [editing, setEditing] = useState<{ id: string; name: string }>();

  const save = async () => {
    if (!editing) return;
    const ok = await act.run(
      () => api("POST", `/blockbustr/libraries/${editing.id}`, { Name: editing.name }),
      reload,
    );
    if (ok) setEditing(undefined);
  };
  // move swaps library i with its neighbour (-1 up, +1 down) and saves the
  // whole order.
  const move = (i: number, by: -1 | 1) => {
    if (!data) return;
    const ids = data.map((l) => l.Id);
    [ids[i], ids[i + by]] = [ids[i + by], ids[i]];
    void act.run(() => api("POST", "/blockbustr/libraries/order", { Ids: ids }), reload);
  };
  const setHidden = (l: AdminLibrary, hidden: boolean) =>
    void act.run(() => api("POST", `/blockbustr/libraries/${l.Id}`, { Hidden: hidden }), reload);

  return (
    <>
      <PageHeader title="Libraries">
        <button disabled={act.busy} onClick={() => act.run(() => api("POST", "/Library/Refresh"), () => setScanNote("Library scan started."))}>
          Scan libraries
        </button>
        <button disabled={act.busy} onClick={() => act.run(() => api("POST", "/blockbustr/catalogs/sync"), () => setScanNote("Catalog sync started."))}>
          Sync catalogs
        </button>
      </PageHeader>
      <ErrorNote error={error ?? act.error} />
      {scanNote && <p className="muted">{scanNote}</p>}
      <div className="card">
        <p className="muted small">
          Users get their libraries in this order, unless they set their own in their app's home settings. A hidden library
          leaves every user's library list and Latest rows, but keeps syncing and stays searchable. File and .strm libraries
          come from <code>config.yaml</code>; catalog libraries are created by enabling an addon's catalog on the Addons page,
          and can be renamed here.
        </p>
        <table>
          <thead><tr><th>Order</th><th>Name</th><th>Type</th><th>Source</th><th>Visibility</th><th></th></tr></thead>
          <tbody>
            {data?.map((l, i) => (
              <tr key={l.Id} className={l.Hidden ? "muted" : undefined}>
                <td>
                  <button className="link small" aria-label={`Move ${l.Name} up`} disabled={act.busy || i === 0} onClick={() => move(i, -1)}>↑</button>{" "}
                  <button className="link small" aria-label={`Move ${l.Name} down`} disabled={act.busy || i === data.length - 1} onClick={() => move(i, 1)}>↓</button>
                </td>
                <td>
                  {editing?.id === l.Id ? (
                    <input
                      aria-label={`New name for ${l.Name}`}
                      value={editing.name}
                      autoFocus
                      onChange={(e) => setEditing({ id: l.Id, name: e.target.value })}
                      onKeyDown={(e) => {
                        if (e.key === "Enter") void save();
                        if (e.key === "Escape") setEditing(undefined);
                      }}
                    />
                  ) : (
                    l.Name
                  )}
                </td>
                <td>{l.CollectionType || "mixed"}</td>
                <td className="muted small">{l.Catalog ? "Stremio catalog" : l.Locations.join(", ")}</td>
                <td><Toggle label="Hidden" checked={l.Hidden} disabled={act.busy} onChange={(on) => setHidden(l, on)} /></td>
                <td>
                  {l.Catalog &&
                    (editing?.id === l.Id ? (
                      <>
                        <button disabled={act.busy || !editing.name.trim()} onClick={() => void save()}>Save</button>{" "}
                        <button className="link small" onClick={() => setEditing(undefined)}>Cancel</button>
                      </>
                    ) : (
                      <button
                        className="link small"
                        disabled={act.busy}
                        onClick={() => {
                          act.clearError();
                          setEditing({ id: l.Id, name: l.Name });
                        }}
                      >
                        Rename
                      </button>
                    ))}
                </td>
              </tr>
            ))}
          </tbody>
        </table>
        <button className="link small" onClick={reload}>Refresh list</button>
      </div>
    </>
  );
}

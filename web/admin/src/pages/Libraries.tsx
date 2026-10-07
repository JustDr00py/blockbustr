import { useState } from "react";
import { api, type VirtualFolder } from "../api";
import { ErrorNote, PageHeader, useAction, useLoad } from "../ui";

const load = () => api<VirtualFolder[]>("GET", "/Library/VirtualFolders");

// A catalog library has no folders on disk; only those can be renamed here
// (file libraries are named in config.yaml).
const isCatalog = (l: VirtualFolder) => l.Locations.length === 0;

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
          File and .strm libraries come from <code>config.yaml</code>. Catalog libraries are created by enabling an addon's
          catalog on the Addons page, and can be renamed here: the name is what the apps show.
        </p>
        <table>
          <thead><tr><th>Name</th><th>Type</th><th>Source</th><th></th></tr></thead>
          <tbody>
            {data?.map((l) => (
              <tr key={l.ItemId}>
                <td>
                  {editing?.id === l.ItemId ? (
                    <input
                      aria-label={`New name for ${l.Name}`}
                      value={editing.name}
                      autoFocus
                      onChange={(e) => setEditing({ id: l.ItemId, name: e.target.value })}
                      onKeyDown={(e) => {
                        if (e.key === "Enter") void save();
                        if (e.key === "Escape") setEditing(undefined);
                      }}
                    />
                  ) : (
                    l.Name
                  )}
                </td>
                <td>{l.CollectionType ?? "mixed"}</td>
                <td className="muted small">{isCatalog(l) ? "Stremio catalog" : l.Locations.join(", ")}</td>
                <td>
                  {isCatalog(l) &&
                    (editing?.id === l.ItemId ? (
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
                          setEditing({ id: l.ItemId, name: l.Name });
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

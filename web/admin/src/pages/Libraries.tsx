import { useState } from "react";
import { api, type VirtualFolder } from "../api";
import { ErrorNote, PageHeader, useAction, useLoad } from "../ui";

const load = () => api<VirtualFolder[]>("GET", "/Library/VirtualFolders");

export default function Libraries() {
  const { data, error, reload } = useLoad(load);
  const act = useAction();
  const [scanNote, setScanNote] = useState<string>();
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
          catalog on the Addons page.
        </p>
        <table>
          <thead><tr><th>Name</th><th>Type</th><th>Source</th></tr></thead>
          <tbody>
            {data?.map((l) => (
              <tr key={l.ItemId}>
                <td>{l.Name}</td>
                <td>{l.CollectionType ?? "mixed"}</td>
                <td className="muted small">{l.Locations.length ? l.Locations.join(", ") : "Stremio catalog"}</td>
              </tr>
            ))}
          </tbody>
        </table>
        <button className="link small" onClick={reload}>Refresh list</button>
      </div>
    </>
  );
}

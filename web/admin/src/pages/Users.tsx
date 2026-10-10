import { useState } from "react";
import { api, type MediaFolder, type User, type UserPolicy } from "../api";
import { ErrorNote, PageHeader, Toggle, timeAgo, useAction, useLoad } from "../ui";

async function load() {
  const [users, folders] = await Promise.all([
    api<User[]>("GET", "/Users"),
    api<{ Items: MediaFolder[] }>("GET", "/Library/MediaFolders"),
  ]);
  return { users, folders: folders.Items };
}

// Jellyfin's parental rating scores (US).
const heights: { label: string; value: number }[] = [
  { label: "Any", value: 0 },
  { label: "1080p", value: 1080 },
  { label: "720p", value: 720 },
  { label: "480p", value: 480 },
];

const ratings: { label: string; value: number | null }[] = [
  { label: "No limit", value: null },
  { label: "G / TV-Y / TV-G", value: 0 },
  { label: "TV-Y7", value: 7 },
  { label: "PG / TV-PG", value: 10 },
  { label: "PG-13", value: 13 },
  { label: "TV-14", value: 14 },
  { label: "R / TV-MA", value: 17 },
];

export default function Users() {
  const { data, error, reload } = useLoad(load);
  const act = useAction();
  const [newName, setNewName] = useState("");
  const [newPw, setNewPw] = useState("");
  const [editing, setEditing] = useState<string>();

  return (
    <>
      <PageHeader title="Users" />
      <ErrorNote error={error ?? act.error} />
      <form
        className="card row"
        onSubmit={(e) => {
          e.preventDefault();
          void act.run(() => api("POST", "/Users/New", { Name: newName.trim(), Password: newPw || undefined }), async () => {
            setNewName("");
            setNewPw("");
            await reload();
          });
        }}
      >
        <input type="text" placeholder="New user name" value={newName} onChange={(e) => setNewName(e.target.value)} />
        <input type="password" placeholder="Password (optional)" value={newPw} onChange={(e) => setNewPw(e.target.value)} autoComplete="new-password" />
        <button className="primary" type="submit" disabled={act.busy || !newName.trim()}>Add user</button>
      </form>
      <div className="card">
        <table>
          <thead><tr><th>User</th><th>Access</th><th>Last sign-in</th><th></th></tr></thead>
          <tbody>
            {data?.users.map((u) => (
              <UserRow
                key={u.Id}
                user={u}
                folders={data.folders}
                editing={editing === u.Id}
                onEdit={() => setEditing(editing === u.Id ? undefined : u.Id)}
                onChanged={async () => {
                  setEditing(undefined);
                  await reload();
                }}
              />
            ))}
          </tbody>
        </table>
      </div>
    </>
  );
}

function accessSummary(p: UserPolicy, folders: MediaFolder[]): string {
  const parts: string[] = [];
  if (p.EnableAllFolders === false) {
    const names = folders.filter((f) => p.EnabledFolders?.some((id) => id.replace(/-/g, "") === f.Id)).map((f) => f.Name);
    parts.push(names.length ? names.join(", ") : "no libraries");
  } else parts.push("all libraries");
  if (p.MaxParentalRating != null) parts.push(`rated ≤ ${ratings.find((r) => r.value === p.MaxParentalRating)?.label ?? p.MaxParentalRating}`);
  if (p.BlockUnratedItems?.length) parts.push(`no unrated ${p.BlockUnratedItems.join("/").toLowerCase()}`);
  if (p.EnableMediaPlayback === false) parts.push("can't play");
  else {
    if (p.EnableContentDownloading === false) parts.push("can't download");
    if (p.EnableVideoPlaybackTranscoding === false) parts.push("no transcoding");
    if (p.MaxVideoHeight) parts.push(`up to ${p.MaxVideoHeight}p`);
    if (p.MaxFileSizeGB) parts.push(`files ≤ ${p.MaxFileSizeGB} GB`);
  }
  return parts.join(" · ");
}

function UserRow({ user, folders, editing, onEdit, onChanged }: {
  user: User;
  folders: MediaFolder[];
  editing: boolean;
  onEdit: () => void;
  onChanged: () => Promise<void>;
}) {
  const p = user.Policy;
  return (
    <>
      <tr>
        <td>
          <strong>{user.Name}</strong>{" "}
          {p.IsAdministrator && <span className="badge ok">admin</span>}{" "}
          {p.IsDisabled && <span className="badge bad">disabled</span>}
          {!user.HasPassword && <div className="muted small">no password</div>}
        </td>
        <td className="small">{accessSummary(p, folders)}</td>
        <td className="muted">{timeAgo(user.LastLoginDate)}</td>
        <td><button onClick={onEdit}>{editing ? "Close" : "Edit"}</button></td>
      </tr>
      {editing && (
        <tr>
          <td colSpan={4}><UserEditor user={user} folders={folders} onChanged={onChanged} /></td>
        </tr>
      )}
    </>
  );
}

function UserEditor({ user, folders, onChanged }: { user: User; folders: MediaFolder[]; onChanged: () => Promise<void> }) {
  const p = user.Policy;
  const [admin, setAdmin] = useState(!!p.IsAdministrator);
  const [disabled, setDisabled] = useState(!!p.IsDisabled);
  const [allFolders, setAllFolders] = useState(p.EnableAllFolders !== false);
  const [enabled, setEnabled] = useState<string[]>((p.EnabledFolders ?? []).map((id) => id.replace(/-/g, "")));
  const [rating, setRating] = useState<number | null>(p.MaxParentalRating ?? null);
  const [blockUnrated, setBlockUnrated] = useState<string[]>(p.BlockUnratedItems ?? []);
  const [playback, setPlayback] = useState(p.EnableMediaPlayback !== false);
  const [downloads, setDownloads] = useState(p.EnableContentDownloading !== false);
  const [transcoding, setTranscoding] = useState(p.EnableVideoPlaybackTranscoding !== false);
  const [height, setHeight] = useState(p.MaxVideoHeight ?? 0);
  const [sizeGB, setSizeGB] = useState(p.MaxFileSizeGB ?? 0);
  const [pw, setPw] = useState("");
  const act = useAction();

  const save = () =>
    act.run(
      () =>
        api("POST", `/Users/${user.Id}/Policy`, {
          IsAdministrator: admin,
          IsDisabled: disabled,
          EnableAllFolders: allFolders,
          EnabledFolders: allFolders ? [] : enabled,
          MaxParentalRating: rating,
          BlockUnratedItems: blockUnrated,
          EnableMediaPlayback: playback,
          EnableContentDownloading: downloads,
          EnableVideoPlaybackTranscoding: transcoding,
          MaxVideoHeight: height,
          MaxFileSizeGB: sizeGB,
        }),
      onChanged,
    );
  const toggleIn = (list: string[], v: string, on: boolean) => (on ? [...new Set([...list, v])] : list.filter((x) => x !== v));

  return (
    <div className="card" style={{ margin: "4px 0" }}>
      <div className="row" style={{ gap: 18, marginBottom: 10 }}>
        <Toggle label="Administrator" checked={admin} onChange={setAdmin} />
        <Toggle label="Disabled" checked={disabled} onChange={setDisabled} />
        <Toggle label="Can play media" checked={playback} onChange={setPlayback} />
        <Toggle label="Can download" checked={playback && downloads} disabled={!playback} onChange={setDownloads} />
        <Toggle label="Can transcode video" checked={playback && transcoding} disabled={!playback} onChange={setTranscoding} />
      </div>
      <div className="field">
        <span>Libraries</span>
        <Toggle label="All libraries" checked={allFolders} onChange={setAllFolders} />
        {!allFolders && (
          <div className="row" style={{ gap: 14, marginTop: 4 }}>
            {folders.map((f) => (
              <Toggle key={f.Id} label={f.Name} checked={enabled.includes(f.Id)} onChange={(on) => setEnabled(toggleIn(enabled, f.Id, on))} />
            ))}
          </div>
        )}
      </div>
      <div className="row" style={{ gap: 18 }}>
        <label className="field">
          <span>Highest rating allowed</span>
          <select value={rating ?? ""} onChange={(e) => setRating(e.target.value === "" ? null : Number(e.target.value))}>
            {ratings.map((r) => <option key={r.label} value={r.value ?? ""}>{r.label}</option>)}
          </select>
        </label>
        <label className="field" title="Highest resolution of the addon versions this user is offered. Lower keeps 4K remuxes off the debrid service's remote traffic.">
          <span>Highest addon resolution</span>
          <select value={height} onChange={(e) => setHeight(Number(e.target.value))}>
            {heights.map((h) => <option key={h.value} value={h.value}>{h.label}</option>)}
          </select>
        </label>
        <label className="field" title="Largest addon version this user is offered, in GB as the version labels show it. Versions whose size isn't known are still offered. 0: no limit.">
          <span>Largest addon file (GB)</span>
          <input type="number" min={0} step={1} value={sizeGB} onChange={(e) => setSizeGB(Math.max(0, Math.floor(Number(e.target.value) || 0)))} />
        </label>
        <div className="field">
          <span>Hide unrated</span>
          <div className="row">
            {["Movie", "Series"].map((t) => (
              <Toggle key={t} label={t === "Movie" ? "Movies" : "Shows"} checked={blockUnrated.includes(t)} onChange={(on) => setBlockUnrated(toggleIn(blockUnrated, t, on))} />
            ))}
          </div>
        </div>
      </div>
      <ErrorNote error={act.error} />
      <div className="row" style={{ justifyContent: "space-between", marginTop: 8 }}>
        <button className="primary" disabled={act.busy} onClick={save}>Save</button>
        <div className="row">
          <input type="password" placeholder="New password" value={pw} onChange={(e) => setPw(e.target.value)} autoComplete="new-password" />
          <button disabled={act.busy || !pw} onClick={() => act.run(() => api("POST", `/Users/${user.Id}/Password`, { NewPw: pw }), async () => { setPw(""); await onChanged(); })}>
            Set password
          </button>
          {user.HasPassword && (
            <button disabled={act.busy} onClick={() => act.run(() => api("POST", `/Users/${user.Id}/Password`, { ResetPassword: true }), onChanged)}>
              Clear password
            </button>
          )}
          <button
            className="danger"
            disabled={act.busy}
            onClick={() => {
              if (window.confirm(`Delete ${user.Name}? Their watch history goes with them.`)) void act.run(() => api("DELETE", `/Users/${user.Id}`), onChanged);
            }}
          >
            Delete
          </button>
        </div>
      </div>
    </div>
  );
}

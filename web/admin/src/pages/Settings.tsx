import { useState } from "react";
import { api, type Setting } from "../api";
import { ErrorNote, PageHeader, Toggle, useAction, useLoad } from "../ui";

const load = () => api<Setting[]>("GET", "/blockbustr/settings");

// The text a setting is edited as: a list comma-separated, a secret empty.
function asText(s: Setting): string {
  if (s.Kind === "list") return ((s.Value as string[] | undefined) ?? []).join(", ");
  if (s.Kind === "secret") return "";
  return s.Value === undefined ? "" : String(s.Value);
}

function parse(s: Setting, text: string): unknown {
  switch (s.Kind) {
    case "list":
      return text.split(/[,\n]/).map((v) => v.trim()).filter(Boolean);
    case "int":
      return Number(text);
    default:
      return text.trim();
  }
}

export default function Settings() {
  const { data, error, reload } = useLoad(load);
  return (
    <>
      <PageHeader title="Settings" />
      <ErrorNote error={error} />
      <div className="card">
        <p className="muted small">
          Changes apply at once, without a restart. A setting that <code>config.yaml</code> or <code>deploy/.env</code> sets is
          locked here: remove it there (and restart) to manage it on this page. Secrets are stored encrypted with
          BLOCKBUSTR_SECRET_KEY and never shown again.
        </p>
        {/* keyed by value too, so a row starts over from what was saved */}
        {data?.map((s) => <SettingRow key={`${s.Key}|${s.Source}|${JSON.stringify(s.Value)}|${s.IsSet}`} s={s} onSaved={reload} />)}
      </div>
    </>
  );
}

function SettingRow({ s, onSaved }: { s: Setting; onSaved: () => Promise<void> }) {
  const act = useAction();
  const [text, setText] = useState(asText(s));
  const save = (value: unknown) => act.run(() => api("POST", "/blockbustr/settings", { [s.Key]: value }), onSaved);
  const source = s.Locked ? `set in ${s.Source}` : s.Source === "admin" ? "set here" : "default";

  return (
    <div className="field" style={{ borderTop: "1px solid var(--border)", paddingTop: 10 }}>
      <span>
        <strong>{s.Label}</strong> <code className="small">{s.Key}</code>{" "}
        <span className={`badge ${s.Locked ? "warn" : s.Source === "admin" ? "ok" : "off"}`}>{source}</span>
      </span>
      <span className="muted small">{s.Help}</span>
      {s.Locked ? (
        <span className="small">
          {s.Kind === "secret" ? (s.IsSet ? "a key is set" : "not set") : asText(s) || "(empty)"}
        </span>
      ) : s.Kind === "bool" ? (
        <div className="row">
          <Toggle label={s.Value ? "On" : "Off"} checked={!!s.Value} disabled={act.busy} onChange={(on) => void save(on)} />
          {s.Source === "admin" && <button className="link small" disabled={act.busy} onClick={() => void save(null)}>Reset to default</button>}
        </div>
      ) : (
        <div className="row">
          <input
            aria-label={s.Label}
            type={s.Kind === "secret" ? "password" : s.Kind === "int" ? "number" : "text"}
            placeholder={s.Kind === "secret" ? (s.IsSet ? "a key is set: enter a new one to replace it" : "not set") : undefined}
            value={text}
            autoComplete="off"
            style={{ minWidth: 320 }}
            onChange={(e) => setText(e.target.value)}
            onKeyDown={(e) => {
              if (e.key === "Enter" && (s.Kind !== "secret" || text)) void save(parse(s, text));
            }}
          />
          <button disabled={act.busy || (s.Kind === "secret" ? !text : text === asText(s))} onClick={() => void save(parse(s, text))}>
            Save
          </button>
          {s.Source === "admin" && (
            <button className="link small" disabled={act.busy} onClick={() => void save(null)}>
              {s.Kind === "secret" ? "Clear" : "Reset to default"}
            </button>
          )}
        </div>
      )}
      <ErrorNote error={act.error} />
    </div>
  );
}

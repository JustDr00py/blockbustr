import { useState } from "react";
import { api, type DebridAccount, type DebridListing, type DebridStatus } from "../api";
import { ErrorNote, PageHeader, Toggle, useAction, useLoad } from "../ui";

const load = () => api<DebridListing>("GET", "/blockbustr/debrid");
const names: Record<string, string> = { realdebrid: "Real-Debrid", torbox: "TorBox" };

export default function Debrid() {
  const { data, error, reload } = useLoad(load);
  return (
    <>
      <PageHeader title="Debrid accounts" />
      <ErrorNote error={error} />
      {data && !data.CanStore && (
        <p className="error">Set BLOCKBUSTR_SECRET_KEY to store debrid keys (they're kept encrypted with it).</p>
      )}
      <p className="muted small">
        Changes apply immediately. Keys set in <code>.env</code> or <code>config.yaml</code> are re-applied on every start.
      </p>
      {data?.Providers.map((p) => (
        <AccountCard key={p} provider={p} account={data.Accounts.find((a) => a.Provider === p)} canStore={data.CanStore} onChanged={reload} />
      ))}
    </>
  );
}

function AccountCard({ provider, account, canStore, onChanged }: {
  provider: string;
  account?: DebridAccount;
  canStore: boolean;
  onChanged: () => Promise<void>;
}) {
  const act = useAction();
  const [key, setKey] = useState("");
  const [priority, setPriority] = useState(account?.Priority ?? 0);
  const [status, setStatus] = useState<DebridStatus>();
  const put = (body: object) => act.run(() => api("POST", `/blockbustr/debrid/${provider}`, body), onChanged);

  return (
    <div className="card">
      <div className="row" style={{ justifyContent: "space-between" }}>
        <h2 style={{ margin: 0 }}>
          {names[provider] ?? provider}{" "}
          {!account ? <span className="badge off">not set up</span> : account.Active ? <span className="badge ok">active</span> : <span className="badge warn">inactive</span>}
        </h2>
        {account && (
          <div className="row">
            <Toggle label="Enabled" checked={account.Enabled} disabled={act.busy} onChange={(v) => put({ Enabled: v })} />
            <label className="row small">
              Priority
              <input type="number" value={priority} onChange={(e) => setPriority(Number(e.target.value))} onBlur={() => priority !== account.Priority && put({ Priority: priority })} />
            </label>
            <button
              disabled={act.busy || !account.Active}
              onClick={() => act.run(async () => setStatus(await api<DebridStatus>("GET", `/blockbustr/debrid/${provider}/status`)))}
            >
              Check key
            </button>
            <button className="danger" disabled={act.busy} onClick={() => window.confirm(`Remove the ${names[provider] ?? provider} account?`) && act.run(() => api("DELETE", `/blockbustr/debrid/${provider}`), onChanged)}>
              Remove
            </button>
          </div>
        )}
      </div>
      {status && (
        <p className={status.OK ? "" : "error"}>
          {status.OK
            ? `Key works${status.PremiumUntil ? ` · premium until ${new Date(status.PremiumUntil).toLocaleDateString()}` : ""}.`
            : `Key check failed: ${status.Error}`}
        </p>
      )}
      <ErrorNote error={act.error} />
      <form
        className="row"
        style={{ marginTop: 8 }}
        onSubmit={(e) => {
          e.preventDefault();
          void put({ ApiKey: key.trim() }).then((ok) => ok && setKey(""));
        }}
      >
        <input type="password" placeholder={account ? "Replace API key" : "API key"} value={key} onChange={(e) => setKey(e.target.value)} autoComplete="off" style={{ flex: 1, minWidth: 200 }} />
        <button className="primary" type="submit" disabled={act.busy || !canStore || !key.trim()}>{account ? "Replace key" : "Add account"}</button>
      </form>
    </div>
  );
}

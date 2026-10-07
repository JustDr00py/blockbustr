import { useState, type FormEvent } from "react";
import { login } from "../api";
import { ErrorNote } from "../ui";

export default function Login({ onSignedIn }: { onSignedIn: () => void }) {
  const [name, setName] = useState("");
  const [pw, setPw] = useState("");
  const [error, setError] = useState<string>();
  const [busy, setBusy] = useState(false);

  async function submit(e: FormEvent) {
    e.preventDefault();
    setBusy(true);
    setError(undefined);
    try {
      await login(name, pw);
      onSignedIn();
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
    } finally {
      setBusy(false);
    }
  }

  return (
    <div className="login">
      <form className="card" onSubmit={submit}>
        <h1>blockbustr admin</h1>
        <label className="field">
          <span>User name</span>
          <input type="text" autoComplete="username" value={name} onChange={(e) => setName(e.target.value)} autoFocus />
        </label>
        <label className="field">
          <span>Password</span>
          <input type="password" autoComplete="current-password" value={pw} onChange={(e) => setPw(e.target.value)} />
        </label>
        <ErrorNote error={error} />
        <button className="primary" type="submit" disabled={busy || !name}>
          {busy ? "Signing in…" : "Sign in"}
        </button>
      </form>
    </div>
  );
}

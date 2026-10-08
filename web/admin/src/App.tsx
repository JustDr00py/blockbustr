import { useEffect, useState } from "react";
import { getToken, logout, setOnUnauthorized } from "./api";
import Login from "./pages/Login";
import Dashboard from "./pages/Dashboard";
import Libraries from "./pages/Libraries";
import Users from "./pages/Users";
import Addons from "./pages/Addons";
import Debrid from "./pages/Debrid";
import Stats from "./pages/Stats";
import Settings from "./pages/Settings";
import Logs from "./pages/Logs";

const pages = {
  dashboard: { label: "Dashboard", Component: Dashboard },
  libraries: { label: "Libraries", Component: Libraries },
  users: { label: "Users", Component: Users },
  addons: { label: "Addons", Component: Addons },
  debrid: { label: "Debrid", Component: Debrid },
  stats: { label: "Stats", Component: Stats },
  settings: { label: "Settings", Component: Settings },
  logs: { label: "Logs", Component: Logs },
} as const;
type Page = keyof typeof pages;

function pageFromHash(): Page {
  const h = window.location.hash.replace(/^#\/?/, "");
  return (h in pages ? h : "dashboard") as Page;
}

export default function App() {
  const [signedIn, setSignedIn] = useState(() => getToken() !== null);
  const [page, setPage] = useState<Page>(pageFromHash);

  useEffect(() => {
    setOnUnauthorized(() => setSignedIn(false));
    const onHash = () => setPage(pageFromHash());
    window.addEventListener("hashchange", onHash);
    return () => window.removeEventListener("hashchange", onHash);
  }, []);

  if (!signedIn) return <Login onSignedIn={() => setSignedIn(true)} />;

  const { Component } = pages[page];
  return (
    <div className="shell">
      <nav className="sidebar">
        <div className="brand">blockbustr</div>
        {(Object.keys(pages) as Page[]).map((p) => (
          <a key={p} href={`#/${p}`} className={p === page ? "active" : ""}>
            {pages[p].label}
          </a>
        ))}
        <button
          className="link signout"
          onClick={async () => {
            await logout();
            setSignedIn(false);
          }}
        >
          Sign out
        </button>
      </nav>
      <main className="content">
        <Component />
      </main>
    </div>
  );
}

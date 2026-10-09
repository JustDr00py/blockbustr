// A thin client for the endpoints the admin UI uses: Jellyfin's (users,
// libraries, sessions) and blockbustr's own (/blockbustr/addons, debrid).

const TOKEN_KEY = "blockbustr.admin.token";
const DEVICE_KEY = "blockbustr.admin.device";

function deviceId(): string {
  try {
    let id = localStorage.getItem(DEVICE_KEY);
    if (!id) {
      id = crypto.randomUUID().replace(/-/g, "");
      localStorage.setItem(DEVICE_KEY, id);
    }
    return id;
  } catch {
    return "blockbustr-admin";
  }
}

export function getToken(): string | null {
  try {
    return localStorage.getItem(TOKEN_KEY);
  } catch {
    return null;
  }
}

export function setToken(token: string | null) {
  try {
    if (token) localStorage.setItem(TOKEN_KEY, token);
    else localStorage.removeItem(TOKEN_KEY);
  } catch {
    /* private mode: the session lasts until reload */
  }
}

function authHeader(token: string | null): string {
  let h = `MediaBrowser Client="blockbustr admin", Device="Browser", DeviceId="${deviceId()}", Version="0.1.0"`;
  if (token) h += `, Token="${token}"`;
  return h;
}

export class ApiError extends Error {
  status: number;
  constructor(status: number, message: string) {
    super(message);
    this.status = status;
  }
}

/** Called on a 401, so the app can return to the login screen. */
export let onUnauthorized: () => void = () => {};
export function setOnUnauthorized(f: () => void) {
  onUnauthorized = f;
}

export async function api<T>(method: string, path: string, body?: unknown): Promise<T> {
  const res = await fetch(path, {
    method,
    headers: {
      Authorization: authHeader(getToken()),
      ...(body !== undefined ? { "Content-Type": "application/json" } : {}),
    },
    body: body !== undefined ? JSON.stringify(body) : undefined,
  });
  if (res.status === 401) {
    onUnauthorized();
    throw new ApiError(401, "Signed out");
  }
  const text = await res.text();
  if (!res.ok) {
    let msg = text || res.statusText;
    try {
      const j = JSON.parse(text);
      if (j && typeof j.Error === "string") msg = j.Error;
    } catch {
      /* plain text error */
    }
    throw new ApiError(res.status, msg || `HTTP ${res.status}`);
  }
  return (text ? JSON.parse(text) : undefined) as T;
}

export interface UserPolicy {
  IsAdministrator?: boolean;
  IsDisabled?: boolean;
  EnableAllFolders?: boolean;
  EnabledFolders?: string[];
  MaxParentalRating?: number | null;
  BlockUnratedItems?: string[];
  EnableMediaPlayback?: boolean;
  EnableContentDownloading?: boolean;
  EnableVideoPlaybackTranscoding?: boolean;
  /** blockbustr's own: caps the addon versions offered (1080: no 4K). */
  MaxVideoHeight?: number;
}

export interface User {
  Id: string;
  Name: string;
  HasPassword: boolean;
  LastLoginDate?: string;
  Policy: UserPolicy;
}

export async function login(username: string, password: string): Promise<User> {
  const res = await fetch("/Users/AuthenticateByName", {
    method: "POST",
    headers: { Authorization: authHeader(null), "Content-Type": "application/json" },
    body: JSON.stringify({ Username: username, Pw: password }),
  });
  if (res.status === 401) throw new ApiError(401, "Wrong user name or password");
  if (!res.ok) throw new ApiError(res.status, `Sign-in failed (HTTP ${res.status})`);
  const j = await res.json();
  if (!j.User?.Policy?.IsAdministrator) throw new ApiError(403, "Only administrators can use this page");
  setToken(j.AccessToken);
  return j.User as User;
}

export async function logout() {
  try {
    await api("POST", "/Sessions/Logout");
  } catch {
    /* already gone */
  }
  setToken(null);
}

export interface MediaFolder {
  Id: string;
  Name: string;
  CollectionType?: string;
}

// A row of GET /blockbustr/libraries, in the order users get them.
export interface AdminLibrary {
  Id: string;
  Name: string;
  CollectionType: string;
  Locations: string[];
  Catalog: boolean;
  Hidden: boolean;
}

export interface Session {
  Id: string;
  Client: string;
  DeviceName: string;
  ApplicationVersion: string;
  UserName?: string;
  LastActivityDate: string;
  RemoteEndPoint?: string;
  NowPlayingItem?: { Name: string; SeriesName?: string; Type: string };
  PlayState?: { PlayMethod?: string; IsPaused?: boolean; PositionTicks?: number };
}

export interface Catalog {
  Type: string;
  Id: string;
  Name: string;
  Enabled: boolean;
  LibraryId?: string;
  Requires: string[];
}

export interface Addon {
  Id: string;
  Host: string;
  Name: string;
  Version: string;
  Description: string;
  Types: string[];
  Resources: string[];
  Enabled: boolean;
  Priority: number;
  LastFetchedAt: string;
  Catalogs: Catalog[];
}

export interface DebridAccount {
  Provider: string;
  Enabled: boolean;
  Priority: number;
  Active: boolean;
}

export interface DebridListing {
  Accounts: DebridAccount[];
  Providers: string[];
  CanStore: boolean;
}

export interface DebridStatus {
  OK: boolean;
  PremiumUntil?: string;
  Error?: string;
}

// A row of GET /blockbustr/settings. Value is absent for a secret (IsSet
// says whether it has one); Locked ones are set in config.yaml or the
// environment (Source names which).
export interface Setting {
  Key: string;
  Label: string;
  Help: string;
  Kind: "int" | "bool" | "list" | "duration" | "secret";
  Value?: unknown;
  IsSet: boolean;
  Source: string;
  Locked: boolean;
}

// GET /blockbustr/logs: recent log records, oldest first, and the level the
// server captures them at (DEBUG, INFO, WARN or ERROR).
export interface LogEntry {
  Seq: number;
  Time: string;
  Level: string;
  Message: string;
  Attrs?: { Key: string; Value: string }[];
}

export interface LogsResponse {
  Level: string;
  Entries: LogEntry[];
}

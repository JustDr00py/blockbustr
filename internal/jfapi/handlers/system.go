package handlers

import (
	"net"
	"net/http"
	"net/netip"
	"runtime"
	"strconv"
	"strings"

	"github.com/sysadmin/blockbustr/internal/auth"

	"github.com/sysadmin/blockbustr/internal/jfapi"
	"github.com/sysadmin/blockbustr/internal/jfapi/dto"
)

// System endpoints. Info/Public and Ping are public (clients call them before
// signing in); Info and Endpoint need a signed-in user, as in 12.1.0.
func (a *api) registerSystem(rt *jfapi.Router) {
	d := a.Deps
	rt.Get("/System/Info/Public", func(w http.ResponseWriter, r *http.Request) {
		jfapi.WriteJSON(w, r, http.StatusOK, publicSystemInfo(d, r))
	})
	rt.Get("/System/Info", a.requireUser(func(w http.ResponseWriter, r *http.Request, _ auth.Session) {
		jfapi.WriteJSON(w, r, http.StatusOK, systemInfo(d, r))
	}))
	rt.Get("/System/Endpoint", a.requireUser(func(w http.ResponseWriter, r *http.Request, _ auth.Session) {
		local := isLocalNetwork(remoteIP(r))
		jfapi.WriteJSON(w, r, http.StatusOK, dto.EndPointInfo{IsLocal: ptr(local), IsInNetwork: ptr(local)})
	}))
	// Jellyfin answers GET and POST /System/Ping with its product name as a JSON string.
	ping := func(w http.ResponseWriter, r *http.Request) {
		jfapi.WriteJSON(w, r, http.StatusOK, d.Config.Compat.ProductName)
	}
	rt.Get("/System/Ping", ping)
	rt.Post("/System/Ping", ping)
}

// systemInfo is the authenticated /System/Info: the public fields plus
// server details. Paths are blockbustr's own; fields that don't apply
// (self-update, the jellyfin-web directory) get Jellyfin's neutral values.
func systemInfo(d Deps, r *http.Request) dto.SystemInfo {
	p := publicSystemInfo(d, r)
	c := d.Config
	return dto.SystemInfo{
		Id: p.Id, ServerName: p.ServerName, Version: p.Version, ProductName: p.ProductName,
		LocalAddress: p.LocalAddress, OperatingSystem: p.OperatingSystem, StartupWizardCompleted: p.StartupWizardCompleted,

		OperatingSystemDisplayName: ptr(""),
		HasPendingRestart:          ptr(false),
		IsShuttingDown:             ptr(false),
		SupportsLibraryMonitor:     ptr(true),
		WebSocketPortNumber:        ptr(listenPort(c.Server.Listen)),
		CompletedInstallations:     &[]dto.InstallationInfo{},
		CanSelfRestart:             ptr(false),
		CanLaunchWebBrowser:        ptr(false),
		ProgramDataPath:            ptr("/config"),
		WebPath:                    ptr(""),
		ItemsByNamePath:            ptr(c.Paths.Cache + "/metadata"),
		CachePath:                  ptr(c.Paths.Cache),
		LogPath:                    ptr(""),
		InternalMetadataPath:       ptr(c.Paths.Cache + "/metadata"),
		TranscodingTempPath:        ptr(c.Paths.TranscodeDir()),
		// Chromecast receiver apps Jellyfin clients can cast to (12.1.0 defaults).
		CastReceiverApplications: &[]dto.CastReceiverApplication{
			{Id: "F007D354", Name: "Stable"},
			{Id: "6F511C87", Name: "Unstable"},
		},
		HasUpdateAvailable: ptr(false),
		EncoderLocation:    ptr("System"),
		SystemArchitecture: ptr(dotnetArch(runtime.GOARCH)),
	}
}

// listenPort is the port of a listen address like ":8096" (0 if unparsable).
func listenPort(listen string) int32 {
	_, port, err := net.SplitHostPort(listen)
	if err != nil {
		return 0
	}
	n, _ := strconv.Atoi(port)
	return int32(n)
}

// dotnetArch maps GOARCH to .NET's Architecture names, as clients expect.
func dotnetArch(goarch string) string {
	switch goarch {
	case "amd64":
		return "X64"
	case "386":
		return "X86"
	case "arm64":
		return "Arm64"
	case "arm":
		return "Arm"
	}
	return goarch
}

// isLocalNetwork reports whether ip is loopback, private (RFC 1918/4193),
// link-local or CGNAT (Tailscale's 100.64.0.0/10).
func isLocalNetwork(ip string) bool {
	addr, err := netip.ParseAddr(ip)
	if err != nil {
		return false
	}
	addr = addr.Unmap()
	return addr.IsLoopback() || addr.IsPrivate() || addr.IsLinkLocalUnicast() || cgnat.Contains(addr)
}

var cgnat = netip.MustParsePrefix("100.64.0.0/10")

func publicSystemInfo(d Deps, r *http.Request) dto.PublicSystemInfo {
	c := d.Config
	return dto.PublicSystemInfo{
		Id:                     ptr(d.ServerID.String()),
		ServerName:             ptr(c.Server.ServerName),
		Version:                ptr(c.Compat.ReportedVersion),
		ProductName:            ptr(c.Compat.ProductName),
		LocalAddress:           ptr(localAddress(c.Server.ExternalURL, r)),
		OperatingSystem:        ptr(""), // 12.1.0 reports "" (deprecated field) rather than omitting it
		StartupWizardCompleted: ptr(true),
	}
}

// localAddress is the URL clients should use to reach the server: the
// configured external URL, else the scheme and host this request came in on.
func localAddress(external string, r *http.Request) string {
	if external != "" {
		return strings.TrimRight(external, "/")
	}
	scheme := "http"
	if r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}

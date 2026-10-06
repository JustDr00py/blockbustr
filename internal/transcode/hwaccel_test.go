package transcode

import (
	"context"
	"errors"
	"os/exec"
	"testing"
)

// stubRunTestEncode replaces runTestEncode for the duration of the test,
// returning success/failure based on which backend's args were passed
// (identified by the -c:v value), and restores the real one on cleanup.
func stubRunTestEncode(t *testing.T, ok map[string]bool) {
	t.Helper()
	orig := runTestEncode
	t.Cleanup(func() { runTestEncode = orig })
	runTestEncode = func(ctx context.Context, args ...string) error {
		for i, a := range args {
			if a == "-c:v" && i+1 < len(args) {
				codec := args[i+1]
				if ok[codec] {
					return nil
				}
				return errors.New("stub: backend unavailable")
			}
		}
		return errors.New("stub: no -c:v found in args")
	}
}

func TestProbe_WiringWithStubbedFFmpeg(t *testing.T) {
	stubRunTestEncode(t, map[string]bool{"h264_vaapi": true, "h264_qsv": false, "h264_nvenc": true})

	caps := Probe(context.Background(), "/dev/dri/renderD128")

	if !caps.Has(CapVAAPI) {
		t.Error("expected vaapi to be detected")
	}
	if caps.Has(CapQSV) {
		t.Error("expected qsv to NOT be detected")
	}
	if !caps.Has(CapNVENC) {
		t.Error("expected nvenc to be detected")
	}
	if !caps.Has(CapSoftware) {
		t.Error("software must always be present")
	}
	if caps.Device != "/dev/dri/renderD128" {
		t.Errorf("Device = %q, want the explicit device passed in", caps.Device)
	}
}

func TestProbe_NoRenderNodeSkipsQSVAndVAAPI(t *testing.T) {
	// The stub would report both available if actually tested; simulating
	// "no GPU present" (no /dev/dri/renderD* found) at the glob level, with
	// no explicit device override, proves vaapi/qsv are never attempted.
	stubRunTestEncode(t, map[string]bool{"h264_vaapi": true, "h264_qsv": true, "h264_nvenc": false})
	origGlob := globRenderNodes
	globRenderNodes = func() []string { return nil }
	t.Cleanup(func() { globRenderNodes = origGlob })

	caps := Probe(context.Background(), "")

	if caps.Has(CapVAAPI) || caps.Has(CapQSV) {
		t.Error("vaapi/qsv should not be tested when no render node is found")
	}
	if caps.Device != "" {
		t.Errorf("Device = %q, want empty when no render node is found", caps.Device)
	}
}

func TestChoose(t *testing.T) {
	full := Capabilities{Available: []Capability{CapVAAPI, CapQSV, CapNVENC, CapSoftware}}
	vaapiOnly := Capabilities{Available: []Capability{CapVAAPI, CapSoftware}}
	softwareOnly := Capabilities{Available: []Capability{CapSoftware}}

	cases := []struct {
		name string
		caps Capabilities
		pref string
		want Capability
	}{
		{"auto prefers qsv first", full, "auto", CapQSV},
		{"auto falls back through the priority list", vaapiOnly, "auto", CapVAAPI},
		{"auto falls back to software", softwareOnly, "auto", CapSoftware},
		{"empty pref behaves like auto", full, "", CapQSV},
		{"explicit available choice is honored", vaapiOnly, "vaapi", CapVAAPI},
		{"explicit unavailable choice falls back to software", softwareOnly, "vaapi", CapSoftware},
		{"software forces software even if hardware exists", full, "software", CapSoftware},
		{"none forces software", full, "none", CapSoftware},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := Choose(c.caps, c.pref); got != c.want {
				t.Errorf("Choose(%v, %q) = %q, want %q", c.caps.Available, c.pref, got, c.want)
			}
		})
	}
}

// TestProbe_RealFFmpeg exercises the actual ffmpeg subprocess path against
// whatever hardware this machine actually has, rather than only the
// stubbed wiring test above. It doesn't assert specific hardware is
// present (that varies by machine/CI) — only that Probe runs to completion
// without hanging and that software is always reported.
func TestProbe_RealFFmpeg(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not installed")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*testEncodeTimeout)
	defer cancel()
	caps := Probe(ctx, "")

	t.Logf("detected on this machine: %v (device=%q)", caps.Available, caps.Device)
	if !caps.Has(CapSoftware) {
		t.Error("software must always be reported available")
	}
	if last := caps.Available[len(caps.Available)-1]; last != CapSoftware {
		t.Errorf("software should be the last entry (lowest priority), got order %v", caps.Available)
	}
}

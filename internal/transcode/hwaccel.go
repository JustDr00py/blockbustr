// Package transcode runs ffmpeg for HLS remux and transcode sessions
// (TASKS P2.5, DESIGN §8.3). hwaccel.go, ported unchanged from jollyrogarr,
// detects the encoder backends at startup by test-encoding on each and
// picks one given the configured preference; session.go manages the ffmpeg
// processes.
package transcode

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"sort"
	"time"
)

// Capability is one video encoder backend.
type Capability string

const (
	CapQSV      Capability = "qsv"
	CapVAAPI    Capability = "vaapi"
	CapNVENC    Capability = "nvenc"
	CapSoftware Capability = "software" // libx264; always available
)

// Capabilities is what startup autodetection found.
type Capabilities struct {
	Available []Capability // hardware backends that passed a real test-encode
	Device    string       // DRI render node used for qsv/vaapi tests, if any
}

// Has reports whether cap was detected as usable.
func (c Capabilities) Has(cap Capability) bool {
	for _, a := range c.Available {
		if a == cap {
			return true
		}
	}
	return false
}

// runTestEncode is a var so tests can stub out the real ffmpeg subprocess.
var runTestEncode = func(ctx context.Context, args ...string) error {
	cmd := exec.CommandContext(ctx, "ffmpeg", args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%w: %s", err, stderr.String())
	}
	return nil
}

const testEncodeTimeout = 10 * time.Second

// Probe tests every hardware backend for real, rather than trusting a
// device file's mere existence — a device node can exist with no working
// driver behind it. device overrides which DRI render node qsv/vaapi are
// tested against; "" auto-discovers the first /dev/dri/renderD* found.
func Probe(ctx context.Context, device string) Capabilities {
	if device == "" {
		device = firstRenderNode()
	}

	var caps []Capability
	if device != "" {
		if testVAAPI(ctx, device) {
			caps = append(caps, CapVAAPI)
		}
		if testQSV(ctx, device) {
			caps = append(caps, CapQSV)
		}
	}
	if testNVENC(ctx) {
		caps = append(caps, CapNVENC)
	}
	caps = append(caps, CapSoftware)

	return Capabilities{Available: caps, Device: device}
}

// globRenderNodes is a var so tests can simulate "no GPU present" without
// depending on the real filesystem.
var globRenderNodes = func() []string {
	matches, _ := filepath.Glob("/dev/dri/renderD*")
	return matches
}

func firstRenderNode() string {
	matches := globRenderNodes()
	if len(matches) == 0 {
		return ""
	}
	sort.Strings(matches)
	return matches[0]
}

func testVAAPI(ctx context.Context, device string) bool {
	ctx, cancel := context.WithTimeout(ctx, testEncodeTimeout)
	defer cancel()
	err := runTestEncode(ctx,
		"-y", "-v", "error",
		"-init_hw_device", "vaapi=va:"+device, "-filter_hw_device", "va",
		"-f", "lavfi", "-i", "testsrc=duration=1:size=64x64:rate=5",
		"-vf", "format=nv12,hwupload",
		"-c:v", "h264_vaapi", "-f", "null", "-",
	)
	return err == nil
}

func testQSV(ctx context.Context, device string) bool {
	ctx, cancel := context.WithTimeout(ctx, testEncodeTimeout)
	defer cancel()
	err := runTestEncode(ctx,
		"-y", "-v", "error",
		"-init_hw_device", "qsv=qs:"+device, "-filter_hw_device", "qs",
		"-f", "lavfi", "-i", "testsrc=duration=1:size=64x64:rate=5",
		"-vf", "format=nv12,hwupload=extra_hw_frames=64",
		"-c:v", "h264_qsv", "-f", "null", "-",
	)
	return err == nil
}

func testNVENC(ctx context.Context) bool {
	ctx, cancel := context.WithTimeout(ctx, testEncodeTimeout)
	defer cancel()
	err := runTestEncode(ctx,
		"-y", "-v", "error",
		"-f", "lavfi", "-i", "testsrc=duration=1:size=64x64:rate=5",
		"-c:v", "h264_nvenc", "-f", "null", "-",
	)
	return err == nil
}

// Choose picks the encoder to actually use, given the configured
// preference (config.TranscodeConfig.HWAccel: "auto"|"qsv"|"vaapi"|
// "nvenc"|"software"|"none") and what Probe found. An explicit preference
// that didn't actually pass its test-encode falls back to software rather
// than silently failing every transcode at runtime.
func Choose(caps Capabilities, pref string) Capability {
	switch pref {
	case "", "auto":
		for _, c := range []Capability{CapQSV, CapVAAPI, CapNVENC} {
			if caps.Has(c) {
				return c
			}
		}
		return CapSoftware
	case "software", "none":
		return CapSoftware
	default:
		c := Capability(pref)
		if caps.Has(c) {
			return c
		}
		return CapSoftware
	}
}

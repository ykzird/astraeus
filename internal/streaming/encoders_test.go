package streaming

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestEncoderOutputArgs_PerFamily pins the flags each family needs. They are not
// interchangeable: every vendor spells "good quality, bitrate follows" its own
// way, and an unknown option makes ffmpeg refuse to open the encoder.
func TestEncoderOutputArgs_PerFamily(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		encoder  string
		contains []string
		absent   []string
	}{
		{
			name:     "nvenc uses a preset and constant quality in vbr",
			encoder:  "h264_nvenc",
			contains: []string{"-c:v h264_nvenc", "-preset p4", "-tune hq", "-rc vbr", "-cq 22", "-pix_fmt nv12"},
			// A target bitrate would override the constant-quality target.
			absent: []string{"-b:v"},
		},
		{
			name:     "nvenc hevc",
			encoder:  "hevc_nvenc",
			contains: []string{"-c:v hevc_nvenc", "-cq 22"},
		},
		{
			name:     "nvenc av1",
			encoder:  "av1_nvenc",
			contains: []string{"-c:v av1_nvenc", "-cq 22"},
		},
		{
			name:     "quick sync uses global_quality",
			encoder:  "h264_qsv",
			contains: []string{"-c:v h264_qsv", "-global_quality 22", "-pix_fmt nv12"},
		},
		{
			name:     "amf sets its quality preset and constant qp",
			encoder:  "h264_amf",
			contains: []string{"-c:v h264_amf", "-quality balanced", "-rc cqp", "-qp_i 22", "-qp_p 22"},
		},
		{
			name:     "videotoolbox is quality driven",
			encoder:  "h264_videotoolbox",
			contains: []string{"-c:v h264_videotoolbox", "-q:v 60"},
			// allow_sw would let a machine without the hardware silently use its
			// CPU instead of reporting that the encoder is unusable.
			absent: []string{"-allow_sw"},
		},
		{
			name:     "vaapi uploads to a hardware surface",
			encoder:  "h264_vaapi",
			contains: []string{"-c:v h264_vaapi", "-vf format=nv12,hwupload"},
		},
		{
			name:     "software h264 keeps crf",
			encoder:  "libx264",
			contains: []string{"-c:v libx264", "-preset veryfast", "-crf 21", "-pix_fmt yuv420p"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			joined := strings.Join(encoderOutputArgs(tt.encoder, 0, encoderDevice{}), " ")
			for _, want := range tt.contains {
				if !strings.Contains(joined, want) {
					t.Errorf("missing %q:\n%s", want, joined)
				}
			}
			for _, unwanted := range tt.absent {
				if strings.Contains(joined, unwanted) {
					t.Errorf("should not contain %q:\n%s", unwanted, joined)
				}
			}
		})
	}
}

// TestEncoderOutputArgs_ScalesForTheFamily covers the two ways scaling is done:
// VAAPI scales on the device after upload, everything else scales in software
// before the encoder sees the frame.
func TestEncoderOutputArgs_ScalesForTheFamily(t *testing.T) {
	t.Parallel()

	vaapi := strings.Join(encoderOutputArgs("h264_vaapi", 720, encoderDevice{RenderNode: "/dev/dri/renderD128"}), " ")
	if !strings.Contains(vaapi, "hwupload,scale_vaapi=w=-2:h=720") {
		t.Errorf("VAAPI should scale on the device after the upload:\n%s", vaapi)
	}

	qsv := strings.Join(encoderOutputArgs("h264_qsv", 720, encoderDevice{}), " ")
	if !strings.Contains(qsv, "-vf scale=-2:720") {
		t.Errorf("QuickSync should scale in software before the encoder:\n%s", qsv)
	}
}

// TestEncoderInputArgs_VAAPINeedsItsDevice is the regression test for a real
// bug: -vaapi_device was passed by the startup probe but never by a real
// session, so the probe could pass while playback failed with "a hardware
// device reference is required to upload frames to".
func TestEncoderInputArgs_VAAPINeedsItsDevice(t *testing.T) {
	t.Parallel()

	device := encoderDevice{RenderNode: "/dev/dri/renderD128"}

	got := encoderInputArgs("h264_vaapi", device)
	if len(got) != 2 || got[0] != "-vaapi_device" || got[1] != "/dev/dri/renderD128" {
		t.Errorf("VAAPI input args = %v, want the render node", got)
	}

	// No node discovered means the encoder should not have been offered at all;
	// emitting a device flag with an empty value would be worse than omitting it.
	if got := encoderInputArgs("h264_vaapi", encoderDevice{}); len(got) != 0 {
		t.Errorf("input args without a render node = %v, want none", got)
	}

	// Only VAAPI needs anything before the input.
	for _, encoder := range []string{"h264_nvenc", "h264_qsv", "h264_amf", "h264_videotoolbox", "libx264"} {
		if got := encoderInputArgs(encoder, device); len(got) != 0 {
			t.Errorf("%s input args = %v, want none", encoder, got)
		}
	}
}

// TestBuildFFmpegArgs_VAAPIDevicePrecedesInput checks the placement, not just
// the presence: -vaapi_device is a global option, and after -i it is not the
// same thing.
func TestBuildFFmpegArgs_VAAPIDevicePrecedesInput(t *testing.T) {
	t.Parallel()

	cfg := ManagerConfig{
		SegmentSeconds: 4,
		Server: ServerCapability{
			VideoEncoders:        []string{"h264_vaapi"},
			HardwareAcceleration: []string{"vaapi"},
			RenderNode:           "/dev/dri/renderD128",
		},
	}
	args, err := BuildFFmpegArgs("/tmp/s", "/media/movie.mkv", Decision{
		Mode: ModeTranscode, Deliverable: true,
		VideoAction: ActionTranscode, AudioAction: ActionCopy, TargetVideoCodec: "h264",
	}, cfg)
	if err != nil {
		t.Fatalf("BuildFFmpegArgs: %v", err)
	}

	device, input := -1, -1
	for i, arg := range args {
		switch arg {
		case "-vaapi_device":
			device = i
		case "-i":
			if input == -1 {
				input = i
			}
		}
	}
	if device == -1 {
		t.Fatalf("no -vaapi_device in the real command line:\n%s", strings.Join(args, " "))
	}
	if input == -1 || device > input {
		t.Errorf("-vaapi_device must precede -i; device at %d, input at %d:\n%s",
			device, input, strings.Join(args, " "))
	}
}

// TestEncoderFor_PrefersFamiliesInOrder covers a host with more than one GPU,
// which is exactly when the order matters.
func TestEncoderFor_PrefersFamiliesInOrder(t *testing.T) {
	t.Parallel()

	all := ServerCapability{VideoEncoders: []string{
		"h264_nvenc", "h264_qsv", "h264_videotoolbox", "h264_vaapi", "h264_amf", "libx264",
		"hevc_nvenc", "hevc_qsv", "hevc_vaapi", "libx265",
		"av1_nvenc", "av1_vaapi", "libsvtav1",
	}}

	tests := []struct {
		codec string
		want  string
	}{
		{codec: "h264", want: "h264_nvenc"},
		{codec: "hevc", want: "hevc_nvenc"},
		{codec: "av1", want: "av1_nvenc"},
	}

	for _, tt := range tests {
		t.Run(tt.codec, func(t *testing.T) {
			t.Parallel()
			if got := EncoderFor(tt.codec, all); got != tt.want {
				t.Errorf("EncoderFor(%q) = %q, want %q", tt.codec, got, tt.want)
			}
		})
	}

	// With NVENC rejected, QuickSync leads.
	noNVENC := all
	noNVENC.VideoEncoders = []string{"h264_qsv", "h264_vaapi", "libx264"}
	if got := EncoderFor("h264", noNVENC); got != "h264_qsv" {
		t.Errorf("EncoderFor(h264) = %q, want h264_qsv", got)
	}

	// A host with only AMD's AMF is still a hardware host.
	amdOnly := ServerCapability{VideoEncoders: []string{"h264_amf"}}
	if got := EncoderFor("h264", amdOnly); got != "h264_amf" {
		t.Errorf("EncoderFor(h264) = %q, want h264_amf", got)
	}

	// And an Apple host.
	apple := ServerCapability{VideoEncoders: []string{"h264_videotoolbox"}}
	if got := EncoderFor("h264", apple); got != "h264_videotoolbox" {
		t.Errorf("EncoderFor(h264) = %q, want h264_videotoolbox", got)
	}
}

func TestHardwareFamilies(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		encoders []string
		want     []string
	}{
		{name: "software only", encoders: []string{"libx264", "libx265"}, want: nil},
		{name: "quick sync", encoders: []string{"h264_qsv", "libx264"}, want: []string{"qsv"}},
		{name: "nvidia", encoders: []string{"h264_nvenc", "hevc_nvenc"}, want: []string{"nvenc"}},
		{name: "apple", encoders: []string{"hevc_videotoolbox"}, want: []string{"videotoolbox"}},
		{name: "amd", encoders: []string{"h264_amf", "av1_amf"}, want: []string{"amf"}},
		{
			// A machine with both, which is the case the list exists for.
			name:     "nvidia beside an intel igpu",
			encoders: []string{"h264_nvenc", "h264_qsv", "libx264"},
			want:     []string{"nvenc", "qsv"},
		},
		{
			name:     "reported in preference order regardless of listing order",
			encoders: []string{"libx264", "h264_amf", "h264_qsv", "h264_nvenc"},
			want:     []string{"nvenc", "qsv", "amf"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := hardwareFamilies(tt.encoders)
			if len(got) != len(tt.want) {
				t.Fatalf("hardwareFamilies(%v) = %v, want %v", tt.encoders, got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Fatalf("hardwareFamilies(%v) = %v, want %v", tt.encoders, got, tt.want)
				}
			}
		})
	}
}

func TestIsHardwareEncoder(t *testing.T) {
	t.Parallel()

	hardware := []string{
		"h264_nvenc", "hevc_nvenc", "av1_nvenc",
		"h264_qsv", "hevc_qsv", "av1_qsv",
		"h264_vaapi", "hevc_vaapi", "av1_vaapi",
		"h264_amf", "hevc_amf", "av1_amf",
		"h264_videotoolbox", "hevc_videotoolbox", "av1_videotoolbox",
	}
	for _, encoder := range hardware {
		if !isHardwareEncoder(encoder) {
			t.Errorf("%s should be treated as a hardware encoder", encoder)
		}
	}

	software := []string{"libx264", "libx265", "libvpx-vp9", "libsvtav1", "libaom-av1"}
	for _, encoder := range software {
		if isHardwareEncoder(encoder) {
			t.Errorf("%s should not be treated as a hardware encoder", encoder)
		}
	}
}

// TestDetectServerCapability_SimulatedHardwareHost covers the families that
// cannot be exercised on the machine this was written on: a stub ffmpeg stands
// in for a host with an NVIDIA card, an Intel GPU whose driver is missing, and
// a VAAPI render node.
//
// It is the closest thing to running on that hardware that is available here,
// and it checks the property that makes the design safe: the startup probe
// renders its options from the same functions a real session does, so an
// encoder cannot pass verification and then fail during playback.
func TestDetectServerCapability_SimulatedHardwareHost(t *testing.T) {
	// Not parallel: the stub reads its argv log path from the environment.
	if runtime.GOOS == "windows" {
		t.Skip("the stub ffmpeg is a shell script")
	}

	dir := t.TempDir()
	argvLog := filepath.Join(dir, "argv.log")

	// A device directory that looks like a machine with a render node, so the
	// VAAPI path is exercised rather than skipped.
	deviceDir := filepath.Join(dir, "dri")
	if err := os.MkdirAll(deviceDir, 0o755); err != nil {
		t.Fatalf("creating the fake device directory: %v", err)
	}
	if err := os.WriteFile(filepath.Join(deviceDir, "renderD128"), nil, 0o644); err != nil {
		t.Fatalf("creating the fake render node: %v", err)
	}

	stub := filepath.Join(dir, "ffmpeg-stub")
	script := `#!/bin/sh
printf '%s\n' "$*" >> "$ASTRAEUS_ARGV_LOG"
for arg in "$@"; do
  if [ "$arg" = "-encoders" ]; then
    cat <<'TABLE'
 Encoders:
 V..... h264_nvenc           NVIDIA NVENC H.264 encoder (codec h264)
 V..... hevc_nvenc           NVIDIA NVENC HEVC encoder (codec hevc)
 V..... h264_qsv             H.264 video encoder (codec h264)
 V..... h264_vaapi           H.264/AVC (VAAPI) (codec h264)
 V..... libx264              libx264 H.264 (codec h264)
 A..... aac                  AAC (Advanced Audio Coding)
TABLE
    exit 0
  fi
done
for arg in "$@"; do
  case "$arg" in
    *_qsv)
      echo "[h264_qsv @ 0x1] Error creating a MFX session: -9." >&2
      exit 1
      ;;
  esac
done
exit 0
`
	if err := os.WriteFile(stub, []byte(script), 0o755); err != nil {
		t.Fatalf("writing the stub ffmpeg: %v", err)
	}
	t.Setenv("ASTRAEUS_ARGV_LOG", argvLog)

	capability := DetectServerCapability(context.Background(), stub, stub, deviceDir)

	// Software is trusted, hardware that proved itself is offered, hardware that
	// failed is not.
	for _, want := range []string{"libx264", "h264_nvenc", "hevc_nvenc", "h264_vaapi"} {
		if !containsFold(capability.VideoEncoders, want) {
			t.Errorf("%s should be offered; got %v", want, capability.VideoEncoders)
		}
	}
	if containsFold(capability.VideoEncoders, "h264_qsv") {
		t.Errorf("h264_qsv failed its probe and must not be offered: %v", capability.VideoEncoders)
	}

	// The rejection is recorded with the reason, which is what makes a machine
	// with a broken driver distinguishable from one with no GPU.
	var qsvReason string
	for _, rejected := range capability.RejectedEncoders {
		if rejected.Encoder == "h264_qsv" {
			qsvReason = rejected.Reason
		}
	}
	if qsvReason == "" {
		t.Fatalf("h264_qsv was dropped without recording why: %+v", capability.RejectedEncoders)
	}
	if !strings.Contains(qsvReason, "MFX session") {
		t.Errorf("the reason should carry ffmpeg's complaint, got %q", qsvReason)
	}

	// Two families proved themselves here - NVENC and, because the fake device
	// directory supplied a render node, VAAPI - and they are reported in
	// preference order rather than the order ffmpeg listed them.
	got := hardwareFamilies(capability.VideoEncoders)
	want := []string{"nvenc", "vaapi"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("hardware families = %v, want %v", got, want)
	}
	if capability.RenderNode == "" {
		t.Error("the render node was not recorded for VAAPI to use")
	}

	// NVENC is chosen over QuickSync and over software.
	if got := EncoderFor("h264", capability); got != "h264_nvenc" {
		t.Errorf("EncoderFor(h264) = %q, want h264_nvenc", got)
	}

	// And a real session gets NVENC's options, not some other family's.
	args, err := BuildFFmpegArgs("/tmp/session", "/media/movie.mkv", Decision{
		Mode: ModeTranscode, Deliverable: true,
		VideoAction: ActionTranscode, AudioAction: ActionTranscode,
		TargetVideoCodec: "h264", TargetAudioCodec: "aac",
	}, ManagerConfig{SegmentSeconds: 4, Server: capability})
	if err != nil {
		t.Fatalf("BuildFFmpegArgs: %v", err)
	}
	joined := strings.Join(args, " ")
	for _, want := range []string{"-c:v h264_nvenc", "-preset p4", "-rc vbr", "-cq 22"} {
		if !strings.Contains(joined, want) {
			t.Errorf("the session command line is missing %q:\n%s", want, joined)
		}
	}

	// The anti-drift check: what the probe ran must carry the same encoder
	// options as the session above, or verification proves nothing.
	recorded, err := os.ReadFile(argvLog)
	if err != nil {
		t.Fatalf("reading the recorded argv: %v", err)
	}
	var probeLine string
	for _, line := range strings.Split(string(recorded), "\n") {
		if strings.Contains(line, "h264_nvenc") && strings.Contains(line, "testsrc") {
			probeLine = line
		}
	}
	if probeLine == "" {
		t.Fatalf("the stub never ran an nvenc probe; recorded:\n%s", recorded)
	}
	for _, want := range []string{"-preset p4", "-rc vbr", "-cq 22", "-pix_fmt nv12"} {
		if !strings.Contains(probeLine, want) {
			t.Errorf("the probe did not use %q, so it verifies something other than what runs:\n%s",
				want, probeLine)
		}
	}

	// The VAAPI probe must have been given its device, before the input.
	for _, line := range strings.Split(string(recorded), "\n") {
		if !strings.Contains(line, "h264_vaapi") || !strings.Contains(line, "testsrc") {
			continue
		}
		deviceAt := strings.Index(line, "-vaapi_device")
		inputAt := strings.Index(line, " -i ")
		if deviceAt == -1 {
			t.Errorf("the VAAPI probe ran without -vaapi_device:\n%s", line)
		} else if inputAt != -1 && deviceAt > inputAt {
			t.Errorf("-vaapi_device must precede -i:\n%s", line)
		}
	}
}

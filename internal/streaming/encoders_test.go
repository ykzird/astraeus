package streaming

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
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

			joined := strings.Join(encoderOutputArgs(tt.encoder, videoPlan{}, encoderDevice{}, 1, 0), " ")
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

	vaapi := strings.Join(encoderOutputArgs("h264_vaapi", videoPlan{Height: 720}, encoderDevice{RenderNode: "/dev/dri/renderD128"}, 1, 0), " ")
	if !strings.Contains(vaapi, "hwupload,scale_vaapi=w=-2:h=720") {
		t.Errorf("VAAPI should scale on the device after the upload:\n%s", vaapi)
	}

	qsv := strings.Join(encoderOutputArgs("h264_qsv", videoPlan{Height: 720}, encoderDevice{}, 1, 0), " ")
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
	//
	// There are now two probes per encoder - the 8-bit one and the 10-bit one -
	// so each is matched on its own pixel format rather than on the encoder
	// name alone.
	recorded, err := os.ReadFile(argvLog)
	if err != nil {
		t.Fatalf("reading the recorded argv: %v", err)
	}
	probeLine, hdrProbeLine := "", ""
	for _, line := range strings.Split(string(recorded), "\n") {
		if !strings.Contains(line, "h264_nvenc") || !strings.Contains(line, "testsrc") {
			continue
		}
		if strings.Contains(line, "-pix_fmt p010le") {
			hdrProbeLine = line
			continue
		}
		probeLine = line
	}
	if probeLine == "" {
		t.Fatalf("the stub never ran an 8-bit nvenc probe; recorded:\n%s", recorded)
	}
	for _, want := range []string{"-preset p4", "-rc vbr", "-cq 22", "-pix_fmt nv12"} {
		if !strings.Contains(probeLine, want) {
			t.Errorf("the probe did not use %q, so it verifies something other than what runs:\n%s",
				want, probeLine)
		}
	}

	// The 10-bit probe is what makes HDR offerable, so it must exist and it must
	// use the same encoder options a real HDR session would.
	if hdrProbeLine == "" {
		t.Fatalf("no 10-bit nvenc probe ran, so HDR was never verified; recorded:\n%s", recorded)
	}
	for _, want := range []string{"-preset p4", "-rc vbr", "-cq 22", "-pix_fmt p010le"} {
		if !strings.Contains(hdrProbeLine, want) {
			t.Errorf("the 10-bit probe did not use %q:\n%s", want, hdrProbeLine)
		}
	}

	support, ok := EncoderForHDR("hevc", capability)
	if !ok {
		t.Fatalf("hevc_nvenc passed the 10-bit probe but is not offered for HDR: %+v",
			capability.HDRVideoEncoders)
	}
	if support.Encoder != "hevc_nvenc" || support.PixelFormat != "p010le" {
		t.Errorf("EncoderForHDR(hevc) = %+v, want hevc_nvenc with p010le", support)
	}

	// A session that keeps HDR is built by the same rule, and it must match the
	// probe: the point of the probe is that this is what will actually run.
	hdrArgs, err := BuildFFmpegArgs("/tmp/session", "/media/movie.mkv", Decision{
		Mode: ModeTranscode, Deliverable: true,
		VideoAction: ActionTranscode, AudioAction: ActionTranscode,
		TargetVideoCodec: "hevc", TargetAudioCodec: "aac",
		TargetDynamicRange: RangeHDR10,
	}, ManagerConfig{SegmentSeconds: 4, Server: capability})
	if err != nil {
		t.Fatalf("BuildFFmpegArgs for HDR: %v", err)
	}
	hdrJoined := strings.Join(hdrArgs, " ")
	for _, want := range []string{
		"-c:v hevc_nvenc", "-preset p4", "-rc vbr", "-cq 22", "-pix_fmt p010le",
		// The colour is set on the frames, not through -color_* options: those
		// were measured not to reach the output. See hdrTagFilter.
		"setparams=color_primaries=bt2020:color_trc=smpte2084:colorspace=bt2020nc",
	} {
		if !strings.Contains(hdrJoined, want) {
			t.Errorf("the HDR session command line is missing %q:\n%s", want, hdrJoined)
		}
	}
	// A tone map would be nonsense here: the client can show HDR and the encoder
	// can produce it.
	if strings.Contains(hdrJoined, "zscale") {
		t.Errorf("an HDR pass-through must not tone map:\n%s", hdrJoined)
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

// ---- dynamic range ---------------------------------------------------------

// TestVideoFilters_ToneMapIsBuiltForSoftwareAndVAAPI covers the two shapes the
// chain takes. Both have to set the output colour, because that is what the
// encoder writes into the stream.
func TestVideoFilters_ToneMapIsBuiltForSoftwareAndVAAPI(t *testing.T) {
	t.Parallel()

	software := strings.Join(videoFilters("libx264", videoPlan{Height: 1080, ToneMap: true}), ",")
	// Scaling first is not cosmetic: the float conversion is the expensive part
	// and a 1080p frame is a quarter of the work of a 4K one.
	if !strings.HasPrefix(software, "scale=-2:1080,zscale=t=linear") {
		t.Errorf("software tone mapping should scale before converting:\n%s", software)
	}
	for _, want := range []string{"tonemap=tonemap=hable", "zscale=t=bt709:m=bt709:r=tv", "format=yuv420p"} {
		if !strings.Contains(software, want) {
			t.Errorf("the tone map chain is missing %q:\n%s", want, software)
		}
	}

	// VAAPI scales on the device after upload, so the tone map runs first and
	// the upload carries the format the encoder will accept.
	vaapi := strings.Join(videoFilters("h264_vaapi", videoPlan{Height: 720, ToneMap: true}), ",")
	toneMapAt := strings.Index(vaapi, "tonemap=tonemap=hable")
	uploadAt := strings.Index(vaapi, "hwupload")
	if toneMapAt == -1 || uploadAt == -1 || toneMapAt > uploadAt {
		t.Errorf("VAAPI must tone map in software before uploading:\n%s", vaapi)
	}
	if !strings.Contains(vaapi, "format=nv12,hwupload,scale_vaapi=w=-2:h=720") {
		t.Errorf("VAAPI should still upload nv12 and scale on the device:\n%s", vaapi)
	}
	// A tone-mapped frame is 8-bit; asking for a 10-bit surface would be a
	// contradiction.
	if strings.Contains(vaapi, "p010le") {
		t.Errorf("a tone-mapped VAAPI session must not upload 10-bit surfaces:\n%s", vaapi)
	}
}

// TestVideoFilters_HDRStatesItsColourOnTheFrames is the regression test for
// what was measured rather than assumed: -color_primaries and -color_trc do not
// reach the output, so the tags have to be set on the frames with setparams.
func TestVideoFilters_HDRStatesItsColourOnTheFrames(t *testing.T) {
	t.Parallel()

	joined := strings.Join(videoFilters("libx265", videoPlan{
		HDRPixelFormat: "yuv420p10le",
		TargetRange:    RangeHDR10,
	}), ",")

	want := "setparams=color_primaries=bt2020:color_trc=smpte2084:colorspace=bt2020nc:range=limited"
	if !strings.Contains(joined, want) {
		t.Errorf("an HDR chain must state its colour on the frames:\n%s", joined)
	}
	if strings.Contains(joined, "tonemap") {
		t.Errorf("an HDR chain must not tone map:\n%s", joined)
	}

	// HLG is a different transfer and has to be named as such: tagging HLG as PQ
	// would make a player interpret it with the wrong curve.
	hlg := strings.Join(videoFilters("libx265", videoPlan{
		HDRPixelFormat: "yuv420p10le",
		TargetRange:    RangeHLG,
	}), ",")
	if !strings.Contains(hlg, "color_trc=arib-std-b67") {
		t.Errorf("HLG must be tagged as HLG:\n%s", hlg)
	}
}

// TestVideoFilters_PlainSDROutputIsUntouched guards the common path: changing
// nothing about a picture that needs no colour work.
func TestVideoFilters_PlainSDROutputIsUntouched(t *testing.T) {
	t.Parallel()

	if got := videoFilters("libx264", videoPlan{Height: 720}); len(got) != 1 || got[0] != "scale=-2:720" {
		t.Errorf("a plain SDR scale-down = %v, want just the scale filter", got)
	}
	if got := videoFilters("libx264", videoPlan{}); len(got) != 0 {
		t.Errorf("a plain SDR transcode with no scaling = %v, want no filters", got)
	}
}

func TestOutputPixelFormat(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		encoder string
		plan    videoPlan
		want    string
	}{
		{name: "software SDR is 8-bit", encoder: "libx264", plan: videoPlan{}, want: "yuv420p"},
		{name: "hardware SDR is nv12", encoder: "h264_nvenc", plan: videoPlan{}, want: "nv12"},
		{name: "VAAPI lets its upload filter decide", encoder: "h264_vaapi", plan: videoPlan{}, want: ""},
		{
			name: "HDR keeps the verified 10-bit format", encoder: "libx265",
			plan: videoPlan{HDRPixelFormat: "yuv420p10le"}, want: "yuv420p10le",
		},
		{
			name: "hardware HDR keeps the format it proved", encoder: "hevc_nvenc",
			plan: videoPlan{HDRPixelFormat: "p010le"}, want: "p010le",
		},
		{
			name: "VAAPI HDR is the upload format, not a -pix_fmt", encoder: "hevc_vaapi",
			plan: videoPlan{HDRPixelFormat: "p010le"}, want: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := outputPixelFormat(tt.encoder, tt.plan); got != tt.want {
				t.Errorf("outputPixelFormat(%q, %+v) = %q, want %q", tt.encoder, tt.plan, got, tt.want)
			}
		})
	}
}

// TestEncoderForHDR covers the rule that HDR is only offered by an encoder that
// was actually driven at 10 bits, in the usual family order when there is more
// than one.
func TestEncoderForHDR(t *testing.T) {
	t.Parallel()

	server := ServerCapability{
		VideoEncoders: []string{"hevc_nvenc", "hevc_qsv", "libx265"},
		HDRVideoEncoders: []HDREncoder{
			{Encoder: "hevc_qsv", PixelFormat: "p010le"},
			{Encoder: "libx265", PixelFormat: "yuv420p10le"},
		},
	}

	// NVENC is preferred but did not prove 10-bit here, so it is not offered.
	got, ok := EncoderForHDR("hevc", server)
	if !ok {
		t.Fatal("hevc_qsv and libx265 both passed the 10-bit probe")
	}
	if got.Encoder != "hevc_qsv" || got.PixelFormat != "p010le" {
		t.Errorf("EncoderForHDR = %+v, want hevc_qsv with p010le", got)
	}

	// A codec with no verified 10-bit encoder is a normal, reportable outcome.
	if _, ok := EncoderForHDR("h264", server); ok {
		t.Error("no H.264 encoder here proved 10-bit, so HDR must not be offered for it")
	}
	if _, ok := EncoderForHDR("av1", server); ok {
		t.Error("no AV1 encoder here proved 10-bit")
	}
}

// TestVideoEncoderFor covers the selection that ties a decision to an encoder.
func TestVideoEncoderFor(t *testing.T) {
	t.Parallel()

	server := ServerCapability{
		VideoEncoders:    []string{"hevc_nvenc", "libx265", "libx264"},
		HDRVideoEncoders: []HDREncoder{{Encoder: "libx265", PixelFormat: "yuv420p10le"}},
	}

	// An HDR decision must not be served by the hardware encoder that only
	// proved 8-bit, even though the ordinary preference would pick it.
	encoder, plan, err := videoEncoderFor(Decision{
		VideoAction: ActionTranscode, TargetVideoCodec: "hevc",
		TargetDynamicRange: RangeHDR10, TargetHeight: 1080,
	}, server)
	if err != nil {
		t.Fatalf("videoEncoderFor: %v", err)
	}
	if encoder != "libx265" {
		t.Errorf("encoder = %q, want libx265 - the only one verified at 10-bit", encoder)
	}
	if plan.HDRPixelFormat != "yuv420p10le" || plan.TargetRange != RangeHDR10 || plan.Height != 1080 {
		t.Errorf("plan = %+v, want a 1080p HDR plan at yuv420p10le", plan)
	}

	// The same decision without HDR keeps the normal preference.
	encoder, plan, err = videoEncoderFor(Decision{
		VideoAction: ActionTranscode, TargetVideoCodec: "hevc",
		TargetDynamicRange: RangeSDR,
	}, server)
	if err != nil {
		t.Fatalf("videoEncoderFor: %v", err)
	}
	if encoder != "hevc_nvenc" {
		t.Errorf("encoder = %q, want hevc_nvenc for an SDR transcode", encoder)
	}
	if plan.HDR() {
		t.Errorf("an SDR plan must not carry an HDR pixel format: %+v", plan)
	}

	// An HDR decision with no verified encoder is an error the caller reports,
	// not something to paper over here.
	if _, _, err := videoEncoderFor(Decision{
		VideoAction: ActionTranscode, TargetVideoCodec: "h264",
		TargetDynamicRange: RangeHDR10,
	}, server); err == nil {
		t.Error("expected an error when no encoder proved 10-bit for the codec")
	}
}

// TestWithoutHardware_AlsoDropsTheHDRList is the regression guard for the
// retry path: leaving a failed hardware encoder in the 10-bit list would make
// the software retry pick it again and fail the same way.
func TestWithoutHardware_AlsoDropsTheHDRList(t *testing.T) {
	t.Parallel()

	cfg := ManagerConfig{Server: ServerCapability{
		VideoEncoders:        []string{"hevc_nvenc", "libx265"},
		HDRVideoEncoders:     []HDREncoder{{Encoder: "hevc_nvenc", PixelFormat: "p010le"}},
		HardwareAcceleration: []string{"nvenc"},
		RenderNode:           "/dev/dri/renderD128",
	}}

	software := withoutHardware(cfg)
	if containsFold(software.Server.VideoEncoders, "hevc_nvenc") {
		t.Errorf("the hardware encoder survived: %v", software.Server.VideoEncoders)
	}
	if len(software.Server.HDRVideoEncoders) != 0 {
		t.Errorf("the failed hardware encoder is still offered for HDR: %+v", software.Server.HDRVideoEncoders)
	}
	if software.Server.RenderNode != "" || len(software.Server.HardwareAcceleration) != 0 {
		t.Errorf("the device should be dropped too: %+v", software.Server)
	}
}

// TestSoftwareOnlyDecision_ToneMapsWhenHDRCannotSurvive covers the fallback
// after a hardware failure: an HDR stream that was going to be encoded in
// hardware has to become tone-mapped SDR if no software encoder can do 10-bit,
// because failing a request a working encoder could have served is worse.
func TestSoftwareOnlyDecision_ToneMapsWhenHDRCannotSurvive(t *testing.T) {
	t.Parallel()

	decision := Decision{
		Mode: ModeTranscode, Deliverable: true,
		VideoAction: ActionTranscode, TargetVideoCodec: "hevc",
		TargetDynamicRange: RangeHDR10,
		Reasons:            []string{"original"},
	}

	// No software 10-bit encoder: fall back to SDR, keeping the reason.
	softwareOnly := ServerCapability{VideoEncoders: []string{"libx265"}}
	got := softwareOnlyDecision(decision, softwareOnly)
	if !got.ToneMap || got.TargetDynamicRange != RangeSDR {
		t.Errorf("expected a tone-mapped SDR decision, got range %q tonemap %v",
			got.TargetDynamicRange, got.ToneMap)
	}
	if !strings.Contains(strings.Join(got.Reasons, "; "), "hardware encoder failed") {
		t.Errorf("the retry must say why HDR was dropped: %s", strings.Join(got.Reasons, "; "))
	}

	// With a software 10-bit encoder the HDR decision stands untouched.
	withSoftwareHDR := ServerCapability{
		VideoEncoders:    []string{"libx265"},
		HDRVideoEncoders: []HDREncoder{{Encoder: "libx265", PixelFormat: "yuv420p10le"}},
	}
	unchanged := softwareOnlyDecision(decision, withSoftwareHDR)
	if unchanged.ToneMap || unchanged.TargetDynamicRange != RangeHDR10 {
		t.Errorf("libx265 can keep HDR in software, so the decision should stand: %+v", unchanged)
	}

	// An SDR decision is never rewritten. Negotiate always fills the range in,
	// but a hand-built decision with none is still not an HDR one, and the retry
	// must leave it exactly as it found it.
	sdr := Decision{Mode: ModeTranscode, Deliverable: true, VideoAction: ActionTranscode, TargetVideoCodec: "hevc"}
	if got := softwareOnlyDecision(sdr, softwareOnly); !reflect.DeepEqual(got, sdr) {
		t.Errorf("an SDR decision should pass through unchanged:\n got %+v\nwant %+v", got, sdr)
	}
}

// TestEncoderOutputArgs_BitrateCeiling covers the VBV constraint that makes a
// client's bitrate limit real. -maxrate with -bufsize is the spelling every
// family understands, so the ceiling is applied uniformly rather than each
// family inventing a target bitrate.
func TestEncoderOutputArgs_BitrateCeiling(t *testing.T) {
	t.Parallel()

	for _, encoder := range []string{"libx264", "libx265", "h264_nvenc", "h264_qsv", "h264_amf", "h264_vaapi"} {
		t.Run(encoder, func(t *testing.T) {
			t.Parallel()

			joined := strings.Join(encoderOutputArgs(encoder, videoPlan{BitrateKbps: 5_000}, encoderDevice{}, 1, 0), " ")
			for _, want := range []string{"-maxrate 5000k", "-bufsize 10000k"} {
				if !strings.Contains(joined, want) {
					t.Errorf("%s is missing %q:\n%s", encoder, want, joined)
				}
			}
			// Quality-driven encodes must stay quality-driven: a hard -b:v would
			// turn CRF and CQ modes into fixed-rate ones.
			if strings.Contains(joined, "-b:v") {
				t.Errorf("%s should bound the rate, not dictate it:\n%s", encoder, joined)
			}
		})
	}
}

// TestEncoderOutputArgs_NoCeilingMeansNoConstraint guards the common path.
func TestEncoderOutputArgs_NoCeilingMeansNoConstraint(t *testing.T) {
	t.Parallel()

	joined := strings.Join(encoderOutputArgs("libx264", videoPlan{Height: 720}, encoderDevice{}, 1, 0), " ")
	if strings.Contains(joined, "-maxrate") || strings.Contains(joined, "-bufsize") {
		t.Errorf("an unlimited session must not gain rate-control flags:\n%s", joined)
	}
}

// TestVideoEncoderFor_CarriesTheBitrateCeiling checks that the plan the args are
// built from actually receives the decision's ceiling - the step that would
// otherwise drop it silently.
func TestVideoEncoderFor_CarriesTheBitrateCeiling(t *testing.T) {
	t.Parallel()

	server := ServerCapability{VideoEncoders: []string{"libx264"}}
	_, plan, err := videoEncoderFor(Decision{
		VideoAction: ActionTranscode, TargetVideoCodec: "h264",
		TargetDynamicRange: RangeSDR, TargetBitrateKbps: 4_500,
	}, server)
	if err != nil {
		t.Fatalf("videoEncoderFor: %v", err)
	}
	if plan.BitrateKbps != 4_500 {
		t.Errorf("plan.BitrateKbps = %d, want 4500", plan.BitrateKbps)
	}
}

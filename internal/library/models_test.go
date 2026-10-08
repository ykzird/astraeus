package library

import "testing"

// TestPlaybackProgressIsFinished pins the boundary that decides whether a film
// is treated as watched through. Getting it wrong in either direction is
// visible: too eager and the last few minutes are unwatchable, too reluctant and
// every play starts three seconds from the end.
func TestPlaybackProgressIsFinished(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		progress *PlaybackProgress
		want     bool
	}{
		{name: "no progress at all", progress: nil, want: false},
		{name: "an unknown duration cannot be finished", progress: &PlaybackProgress{PositionSeconds: 900, DurationSeconds: 0}, want: false},
		{name: "half way", progress: &PlaybackProgress{PositionSeconds: 300, DurationSeconds: 600}, want: false},
		{name: "just short of the threshold", progress: &PlaybackProgress{PositionSeconds: 569, DurationSeconds: 600}, want: false},
		{name: "at the threshold", progress: &PlaybackProgress{PositionSeconds: 570, DurationSeconds: 600}, want: true},
		{name: "past the end", progress: &PlaybackProgress{PositionSeconds: 601, DurationSeconds: 600}, want: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := tt.progress.IsFinished(); got != tt.want {
				t.Errorf("IsFinished(%+v) = %v, want %v", tt.progress, got, tt.want)
			}
		})
	}
}

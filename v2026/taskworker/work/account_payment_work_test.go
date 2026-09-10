package work

import (
	"testing"
	"time"
)

func TestNextPayoutTimeAvoidsBackupAndMaintenanceWindows(t *testing.T) {
	tests := []struct {
		name string
		now  time.Time
		want time.Time
	}{
		{
			name: "saturday",
			now:  time.Date(2026, time.August, 15, 23, 30, 0, 0, time.UTC),
			want: time.Date(2026, time.August, 16, payoutHourUtc, 0, 0, 0, time.UTC),
		},
		{
			name: "sunday always advances one week",
			now:  time.Date(2026, time.August, 16, 1, 0, 0, 0, time.UTC),
			want: time.Date(2026, time.August, 23, payoutHourUtc, 0, 0, 0, time.UTC),
		},
		{
			name: "normalizes to utc",
			now:  time.Date(2026, time.August, 14, 23, 0, 0, 0, time.FixedZone("test", -5*60*60)),
			want: time.Date(2026, time.August, 16, payoutHourUtc, 0, 0, 0, time.UTC),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := nextPayoutTime(tt.now); !got.Equal(tt.want) {
				t.Fatalf("nextPayoutTime(%s) = %s, want %s", tt.now, got, tt.want)
			}
		})
	}
}

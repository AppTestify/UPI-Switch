package api

import (
	"testing"
	"time"

	"github.com/AppTestify/UPI-Switch/internal/npci"
	"github.com/AppTestify/UPI-Switch/internal/store"
)

func TestTechnicalCodesCoverFailoverSet(t *testing.T) {
	for _, c := range []string{"U09", "U90", "XY", "U91", "96"} {
		if npci.IsTechnicalDecline(c) && !isTechnical(c) {
			t.Errorf("%s triggers failover but is not reported as a technical decline", c)
		}
	}
	for _, c := range []string{"Z9", "ZM", "U69", "ZA", "00"} {
		if isTechnical(c) {
			t.Errorf("%s is a business decline", c)
		}
	}
}

func TestStatsWindowAndFill(t *testing.T) {
	ist, _ := time.LoadLocation("Asia/Kolkata")
	now := time.Date(2026, 10, 18, 11, 40, 0, 0, ist)
	from, to, prev, bucket := statsWindow("today", now, ist)
	if !from.Equal(time.Date(2026, 10, 18, 0, 0, 0, 0, ist)) || !to.Equal(now) || !prev.Equal(from.Add(-24*time.Hour)) || bucket != "hour" {
		t.Fatalf("today: %v %v %v %s", from, to, prev, bucket)
	}
	s := fillSeries([]store.Bucket{{At: time.Date(2026, 10, 18, 3, 0, 0, 0, ist), Count: 7}}, from, to, bucket, ist)
	if len(s) != 12 || s[3].Count != 7 || s[0].Count != 0 {
		t.Fatalf("fill: %d buckets, %+v", len(s), s[3])
	}
	from, _, prev, bucket = statsWindow("30d", now, ist)
	if bucket != "day" || now.Sub(from) != 30*24*time.Hour || from.Sub(prev) != 30*24*time.Hour {
		t.Fatalf("30d: %v %v %s", from, prev, bucket)
	}
	if d := fillSeries(nil, from, now, "day", ist); len(d) != 31 {
		t.Fatalf("30d fill: %d", len(d))
	}
}

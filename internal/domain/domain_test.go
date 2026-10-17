package domain

import "testing"

func TestParseAmount(t *testing.T) {
	ok := map[string]int64{"1": 100, "1.5": 150, "149.50": 14950, "0.01": 1, "100000.00": 10000000}
	for in, want := range ok {
		got, err := ParseAmount(in)
		if err != nil || got != want {
			t.Errorf("ParseAmount(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "-1", "+1", "1.234", "abc", "1.", "1e3", "1.-5"} {
		if _, err := ParseAmount(bad); err == nil {
			t.Errorf("ParseAmount(%q) should fail", bad)
		}
	}
	if FormatAmount(14950) != "149.50" || FormatAmount(5) != "0.05" {
		t.Fatal("FormatAmount")
	}
}

func TestStateMachine(t *testing.T) {
	allowed := [][2]TxnStatus{
		{StatusInitiated, StatusPending}, {StatusPending, StatusSuccess}, {StatusPending, StatusFailed},
		{StatusPending, StatusDeemed}, {StatusDeemed, StatusSuccess}, {StatusDeemed, StatusFailed}, {StatusInitiated, StatusSuccess},
	}
	for _, p := range allowed {
		if !CanTransition(p[0], p[1]) {
			t.Errorf("%s → %s should be allowed", p[0], p[1])
		}
	}
	denied := [][2]TxnStatus{
		{StatusSuccess, StatusFailed}, {StatusFailed, StatusSuccess}, {StatusSuccess, StatusPending},
		{StatusDeemed, StatusPending}, {StatusPending, StatusInitiated},
	}
	for _, p := range denied {
		if CanTransition(p[0], p[1]) {
			t.Errorf("%s → %s should be denied", p[0], p[1])
		}
	}
}

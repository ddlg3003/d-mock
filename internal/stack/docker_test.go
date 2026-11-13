package stack

import "testing"

func TestParseBytes(t *testing.T) {
	cases := map[string]int64{"512MiB": 512 << 20, "1.5GiB": 3 << 29, "100kB": 100000, "42B": 42, "2GB": 2e9}
	for in, want := range cases {
		if got := parseBytes(in); got != want {
			t.Errorf("parseBytes(%q) = %d, want %d", in, got, want)
		}
	}
	if parsePercent(" 12.5% ") != 12.5 {
		t.Error("parsePercent")
	}
}

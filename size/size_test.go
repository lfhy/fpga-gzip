package size

import "testing"

func TestParse(t *testing.T) {
	tests := map[string]uint64{
		"0":     0,
		"4096":  4096,
		"4K":    4096,
		"4KiB":  4096,
		"1.5M":  1572864,
		"2 MiB": 2097152,
	}
	for input, want := range tests {
		got, err := Parse(input)
		if err != nil {
			t.Fatalf("Parse(%q): %v", input, err)
		}
		if got != want {
			t.Errorf("Parse(%q) = %d, want %d", input, got, want)
		}
	}
}

func TestParseRejectsInvalid(t *testing.T) {
	for _, input := range []string{"", "-1", "abc", "1PB", "1..2"} {
		if _, err := Parse(input); err == nil {
			t.Errorf("Parse(%q) unexpectedly succeeded", input)
		}
	}
}

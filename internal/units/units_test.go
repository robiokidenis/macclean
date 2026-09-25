package units

import "testing"

func TestFormat(t *testing.T) {
	cases := []struct {
		in   int64
		want string
	}{
		{0, "0 B"},
		{512, "512 B"},
		{1024, "1.0 KB"},
		{1536, "1.5 KB"},
		{10 * 1024, "10.0 KB"},
		{42 * 1024 * 1024, "42.0 MB"},
		{428 * 1024 * 1024 * 1024 / 10, "42.8 GB"},
		{-1024, "-1.0 KB"},
	}
	for _, c := range cases {
		if got := Format(c.in); got != c.want {
			t.Errorf("Format(%d) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestParseSize(t *testing.T) {
	cases := []struct {
		in   string
		want int64
	}{
		{"10MB", 10 << 20},
		{"1GB", 1 << 30},
		{"1.5GiB", 1610612736},
		{"500KB", 500 << 10},
		{"2G", 2 << 30},
		{"1024", 1024},
		{"1tb", 1 << 40},
	}
	for _, c := range cases {
		got, err := ParseSize(c.in)
		if err != nil {
			t.Errorf("ParseSize(%q) error: %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("ParseSize(%q) = %d, want %d", c.in, got, c.want)
		}
	}
	if _, err := ParseSize("abc"); err == nil {
		t.Error("ParseSize(abc) should fail")
	}
}

// Package units formats and parses byte sizes.
package units

import (
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode"
)

// Format renders a byte count the way Finder does: one decimal, e.g.
// "42.8 GB". Format(0) == "0 B".
func Format(n int64) string {
	if n < 0 {
		return "-" + Format(-n)
	}
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTPE"[exp])
}

// ParseSize accepts forms like "10MB", "1.5GiB", "1GB", "500KB", "2G", "1024".
// SI (MB=10^6) and binary (MiB=2^20) suffixes are honored; a bare B/K/M/G/T
// suffix is treated as binary, matching how sizes are reported.
func ParseSize(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("empty size")
	}
	i := 0
	for i < len(s) && (unicode.IsDigit(rune(s[i])) || s[i] == '.' || s[i] == ',') {
		i++
	}
	numPart, suffix := s[:i], strings.TrimSpace(strings.ToUpper(s[i:]))
	numPart = strings.ReplaceAll(numPart, ",", "")
	num, err := strconv.ParseFloat(numPart, 64)
	if err != nil {
		return 0, fmt.Errorf("bad size %q", s)
	}
	mult := int64(1)
	switch suffix {
	case "", "B":
		mult = 1
	case "K", "KB":
		mult = 1 << 10
	case "KI", "KIB":
		mult = 1 << 10
	case "M", "MB":
		mult = 1 << 20
	case "MI", "MIB":
		mult = 1 << 20
	case "G", "GB":
		mult = 1 << 30
	case "GI", "GIB":
		mult = 1 << 30
	case "T", "TB":
		mult = 1 << 40
	case "TI", "TIB":
		mult = 1 << 40
	case "P", "PB":
		mult = 1 << 50
	case "PI", "PIB":
		mult = 1 << 50
	default:
		return 0, fmt.Errorf("unknown size suffix %q", suffix)
	}
	return int64(num * float64(mult)), nil
}

// Age renders a duration as "182 days ago" style text.
func Age(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < 0:
		return "in the future"
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%d min ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%d h ago", int(d.Hours()))
	case d < 30*24*time.Hour:
		return fmt.Sprintf("%d days ago", int(d.Hours()/24))
	case d < 365*24*time.Hour:
		return fmt.Sprintf("%d months ago", int(d.Hours()/24/30))
	default:
		years := int(d.Hours() / 24 / 365)
		if years == 1 {
			return "1 year ago"
		}
		return fmt.Sprintf("%d years ago", years)
	}
}

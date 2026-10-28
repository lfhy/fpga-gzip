// Package size parses human-readable byte sizes used by the CLI and config
// file.  The syntax intentionally mirrors the C implementation that this
// project replaces while also accepting the conventional KiB/MiB/GiB forms.
package size

import (
	"fmt"
	"math"
	"strconv"
	"strings"
)

// Parse converts a byte-size string into bytes.  Examples: 4096, 4K, 4KiB,
// 1.5M and 1GiB.
func Parse(value string) (uint64, error) {
	text := strings.TrimSpace(value)
	if text == "" {
		return 0, fmt.Errorf("size is empty")
	}

	cut := 0
	for cut < len(text) {
		c := text[cut]
		if (c >= '0' && c <= '9') || c == '.' {
			cut++
			continue
		}
		break
	}
	if cut == 0 {
		return 0, fmt.Errorf("invalid size %q", value)
	}

	number, err := strconv.ParseFloat(text[:cut], 64)
	if err != nil || math.IsNaN(number) || math.IsInf(number, 0) || number < 0 {
		return 0, fmt.Errorf("invalid size %q", value)
	}

	suffix := strings.ToLower(strings.TrimSpace(text[cut:]))
	multiplier, ok := map[string]float64{
		"":    1,
		"b":   1,
		"k":   1 << 10,
		"kb":  1 << 10,
		"kib": 1 << 10,
		"m":   1 << 20,
		"mb":  1 << 20,
		"mib": 1 << 20,
		"g":   1 << 30,
		"gb":  1 << 30,
		"gib": 1 << 30,
		"t":   1 << 40,
		"tb":  1 << 40,
		"tib": 1 << 40,
	}[suffix]
	if !ok {
		return 0, fmt.Errorf("invalid size suffix %q in %q", suffix, value)
	}

	bytes := number * multiplier
	if bytes > float64(math.MaxUint64) || bytes < 0 {
		return 0, fmt.Errorf("size %q overflows uint64", value)
	}
	return uint64(bytes), nil
}

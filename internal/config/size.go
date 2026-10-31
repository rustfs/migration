package config

import (
	"fmt"
	"math/big"
	"strings"

	"gopkg.in/yaml.v3"
)

// ByteRate is a bytes-per-second cap. Zero means unlimited.
//
// Text form accepts a bare byte count or a human size:
//
//	20M / 20MB  = 20 × 1000 × 1000 bytes/s   (decimal MB)
//	20Mi / 20MiB = 20 × 1024 × 1024 bytes/s  (binary MiB)
//
// The same split applies to K/KB vs Ki/KiB and G/GB vs Gi/GiB.
// Suffixes are case-insensitive. An optional "/s" is ignored.
// 0, 0M, and 0MiB are unlimited. A negative value is rejected.
type ByteRate int64

func (b ByteRate) String() string {
	if b <= 0 {
		return "unlimited"
	}
	n := int64(b)
	switch {
	case n%(1000*1000*1000) == 0:
		return fmt.Sprintf("%dGB/s", n/(1000*1000*1000))
	case n%(1024*1024*1024) == 0:
		return fmt.Sprintf("%dGiB/s", n/(1024*1024*1024))
	case n%(1000*1000) == 0:
		return fmt.Sprintf("%dMB/s", n/(1000*1000))
	case n%(1024*1024) == 0:
		return fmt.Sprintf("%dMiB/s", n/(1024*1024))
	default:
		return fmt.Sprintf("%dB/s", n)
	}
}

// UnmarshalYAML accepts either a bare integer (bytes/s) or a size string
// such as 20M. Missing fields stay 0 (unlimited) because UnmarshalYAML is
// not called when the key is absent.
func (b *ByteRate) UnmarshalYAML(node *yaml.Node) error {
	if node == nil {
		*b = 0
		return nil
	}
	if node.Kind != yaml.ScalarNode {
		return fmt.Errorf("rate_limit must be a size such as 20M, 20MiB, or 0")
	}
	if node.Tag == "!!null" {
		*b = 0
		return nil
	}
	n, err := ParseByteRate(node.Value)
	if err != nil {
		return fmt.Errorf("rate_limit: %w", err)
	}
	*b = ByteRate(n)
	return nil
}

// ParseByteRate parses a per-second byte cap. See ByteRate for the unit rules.
func ParseByteRate(s string) (int64, error) {
	s = strings.ToLower(strings.Join(strings.Fields(s), ""))
	if s == "" {
		return 0, fmt.Errorf("rate limit is empty")
	}
	s = strings.TrimSuffix(s, "/s")
	if s == "" {
		return 0, fmt.Errorf("rate limit is empty")
	}
	if strings.HasPrefix(s, "-") {
		return 0, fmt.Errorf("rate limit must be >= 0, got %q", s)
	}
	s = strings.TrimPrefix(s, "+")

	i := 0
	for i < len(s) && s[i] >= '0' && s[i] <= '9' {
		i++
	}
	if i < len(s) && s[i] == '.' {
		i++
		for i < len(s) && s[i] >= '0' && s[i] <= '9' {
			i++
		}
	}
	if i == 0 || s[:i] == "." {
		return 0, fmt.Errorf("rate limit %q is not a size", s)
	}
	num, suf := s[:i], s[i:]
	mult, err := rateMultiplier(suf)
	if err != nil {
		return 0, err
	}
	return scaleRate(num, mult)
}

func rateMultiplier(suf string) (int64, error) {
	switch suf {
	case "", "b":
		return 1, nil
	case "k", "kb":
		return 1000, nil
	case "ki", "kib":
		return 1024, nil
	case "m", "mb":
		return 1000 * 1000, nil
	case "mi", "mib":
		return 1024 * 1024, nil
	case "g", "gb":
		return 1000 * 1000 * 1000, nil
	case "gi", "gib":
		return 1024 * 1024 * 1024, nil
	case "t", "tb":
		return 1000 * 1000 * 1000 * 1000, nil
	case "ti", "tib":
		return 1024 * 1024 * 1024 * 1024, nil
	default:
		return 0, fmt.Errorf("unknown size suffix %q (use B, K, KB, Ki, KiB, M, MB, Mi, MiB, G, GB, Gi, GiB, T, TB, Ti, TiB)", suf)
	}
}

func scaleRate(num string, mult int64) (int64, error) {
	// The parser above only admits decimal digits and a decimal point. Scale
	// before rounding so e.g. 0.000000001G still accounts for one byte/s.
	rate, ok := new(big.Rat).SetString(num)
	if !ok {
		return 0, fmt.Errorf("rate limit %q is not a size", num)
	}
	rate.Mul(rate, new(big.Rat).SetInt64(mult))
	whole := new(big.Int).Quo(rate.Num(), rate.Denom())
	if !whole.IsInt64() {
		return 0, fmt.Errorf("rate limit overflows")
	}
	if whole.Sign() == 0 && rate.Sign() != 0 {
		return 0, fmt.Errorf("rate limit is too small")
	}
	return whole.Int64(), nil
}

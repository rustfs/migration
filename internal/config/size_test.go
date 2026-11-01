package config

import (
	"strings"
	"testing"
)

func TestParseByteRate(t *testing.T) {
	const mib = 1024 * 1024
	cases := []struct {
		in      string
		want    int64
		wantErr string
	}{
		{in: "0", want: 0},
		{in: "0M", want: 0},
		{in: "0MiB", want: 0},
		{in: "0.0", want: 0},
		{in: "20M", want: 20 * 1000 * 1000},
		{in: "20m", want: 20 * 1000 * 1000},
		{in: "20MB", want: 20 * 1000 * 1000},
		{in: "20mb/s", want: 20 * 1000 * 1000},
		{in: "30M", want: 30 * 1000 * 1000},
		{in: "50M", want: 50 * 1000 * 1000},
		{in: "  20 M  ", want: 20 * 1000 * 1000},
		{in: "20Mi", want: 20 * mib},
		{in: "20MiB", want: 20 * mib},
		{in: "20mib", want: 20 * mib},
		{in: "0.000000001G", want: 1},
		{in: "001.000000019G", want: 1_000_000_019},
		{in: "9223372036854775807", want: 9223372036854775807},
		{in: "9223372036854775808", wantErr: "overflow"},
		{in: "1.5M", want: 1_500_000},
		{in: "20.5MiB", want: 20*mib + mib/2},
		{in: "1024", want: 1024},
		{in: "2KiB", want: 2048},
		{in: "2KB", want: 2000},
		{in: "2K", want: 2000},
		{in: "1G", want: 1000 * 1000 * 1000},
		{in: "1Gi", want: 1024 * 1024 * 1024},
		{in: "+20M", want: 20 * 1000 * 1000},
		{in: "", wantErr: "empty"},
		{in: "-1", wantErr: "must be >= 0"},
		{in: "-20M", wantErr: "must be >= 0"},
		{in: "20X", wantErr: "unknown size suffix"},
		{in: "nope", wantErr: "not a size"},
		{in: "0.1", wantErr: "too small"},
		{in: "999999999999TiB", wantErr: "overflow"},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			got, err := ParseByteRate(tc.in)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("error %v, want substring %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Fatalf("got %d, want %d", got, tc.want)
			}
		})
	}
}

func TestByteRateStringDistinguishesMBAndMiB(t *testing.T) {
	mb := ByteRate(20 * 1000 * 1000)
	mi := ByteRate(20 * 1024 * 1024)
	if mb.String() != "20MB/s" {
		t.Fatalf("MB string %q", mb.String())
	}
	if mi.String() != "20MiB/s" {
		t.Fatalf("MiB string %q", mi.String())
	}
	if ByteRate(0).String() != "unlimited" {
		t.Fatalf("zero string %q", ByteRate(0).String())
	}
}

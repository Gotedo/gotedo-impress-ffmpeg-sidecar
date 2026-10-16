package main

import (
	"math"
	"testing"
)

func TestConversionPTSRescalingMakesDuplicateTimestampsStrictlyIncreasing(t *testing.T) {
	last := int64(math.MinInt64) // FFmpeg's AV_NOPTS_VALUE sentinel.
	first, last := rescaleConversionPTS(100, 1, 1000, 1, 60000, last)
	if first != 6000 || last != first {
		t.Fatalf("first rescaled PTS = %d (last %d), want 6000", first, last)
	}

	duplicate, last := rescaleConversionPTS(100, 1, 1000, 1, 60000, last)
	if duplicate != first+1 || last != duplicate {
		t.Fatalf("duplicate timestamp normalized to %d (last %d), want %d", duplicate, last, first+1)
	}

	next, last := rescaleConversionPTS(101, 1, 1000, 1, 60000, last)
	if next != 6060 || last != next {
		t.Fatalf("next rescaled PTS = %d (last %d), want 6060", next, last)
	}
}

package main

import (
	"context"
	"testing"

	"github.com/gotedo/gotedo-impress-ffmpeg-sidecar/proto"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestConversionConcurrencyLimitUsesPhysicalThenLogicalCores(t *testing.T) {
	for _, test := range []struct{ physical, logical, want int }{
		{8, 16, 4}, {7, 16, 3}, {0, 12, 6}, {0, 1, 1}, {1, 8, 1},
	} {
		if got := conversionConcurrencyLimit(test.physical, test.logical); got != test.want {
			t.Errorf("limit(%d physical, %d logical) = %d, want %d", test.physical, test.logical, got, test.want)
		}
	}
}

func TestConversionCapacityAndCoreLeases(t *testing.T) {
	server := &ffmpegServer{}
	capacity, err := server.GetConversionCapacity(context.Background(), &proto.ConversionCapacityRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if capacity.GetMaxConcurrent() < 1 || capacity.GetLogicalCpus() < 1 {
		t.Fatalf("invalid conversion capacity: %+v", capacity)
	}
	leases := make([]int, 0, capacity.GetMaxConcurrent())
	for i := int32(0); i < capacity.GetMaxConcurrent(); i++ {
		core, err := server.acquireConversionLease(string(rune('a' + i)))
		if err != nil {
			t.Fatalf("lease %d failed: %v", i, err)
		}
		for _, previous := range leases {
			if core == previous {
				t.Fatalf("duplicate core lease %d", core)
			}
		}
		leases = append(leases, core)
	}
	if _, err := server.acquireConversionLease("over-capacity"); status.Code(err) != codes.ResourceExhausted {
		t.Fatalf("over-capacity lease error = %v, want ResourceExhausted", err)
	}
	active, err := server.GetConversionCapacity(context.Background(), &proto.ConversionCapacityRequest{})
	if err != nil || active.GetActiveSlots() != capacity.GetMaxConcurrent() {
		t.Fatalf("active capacity = %+v, err=%v", active, err)
	}
	server.releaseConversionLease(leases[0], "a")
	if _, err := server.acquireConversionLease("replacement"); err != nil {
		t.Fatalf("released lease was not reusable: %v", err)
	}
}

func TestConversionAffinityUnsupportedInputIsGraceful(t *testing.T) {
	state, pinned := pinConversionThread(-1)
	defer restoreConversionThreadAffinity(state)
	if pinned || state != nil {
		t.Fatal("invalid affinity request should fall back without creating state")
	}
}

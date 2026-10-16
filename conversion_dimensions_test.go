package main

import "testing"

func TestComputeConversionOutputDimensions(t *testing.T) {
	tests := []struct {
		name          string
		width, height int
		resolution    int
		wantWidth     int
		wantHeight    int
	}{
		{name: "4K source defaults to 1080p bounds", width: 3840, height: 2160, resolution: 1080, wantWidth: 1920, wantHeight: 1080},
		{name: "8K source scales to 1440p", width: 7680, height: 4320, resolution: 1440, wantWidth: 2560, wantHeight: 1440},
		{name: "portrait aspect ratio is preserved", width: 2160, height: 3840, resolution: 1080, wantWidth: 1080, wantHeight: 1920},
		{name: "ultrawide aspect ratio is preserved", width: 5120, height: 2160, resolution: 1080, wantWidth: 1920, wantHeight: 810},
		{name: "sub-4K source is not downscaled", width: 2560, height: 1440, resolution: 720, wantWidth: 2560, wantHeight: 1440},
		{name: "4K option retains 4K source", width: 3840, height: 2160, resolution: 2160, wantWidth: 3840, wantHeight: 2160},
		{name: "zero preserves legacy source dimensions", width: 3840, height: 2160, resolution: 0, wantWidth: 3840, wantHeight: 2160},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			width, height, err := computeConversionOutputDimensions(tt.width, tt.height, tt.resolution)
			if err != nil {
				t.Fatal(err)
			}
			if width != tt.wantWidth || height != tt.wantHeight {
				t.Fatalf("dimensions = %dx%d, want %dx%d", width, height, tt.wantWidth, tt.wantHeight)
			}
		})
	}

	if _, _, err := computeConversionOutputDimensions(3840, 2160, 900); err == nil {
		t.Fatal("expected unsupported output resolution to fail")
	}
}

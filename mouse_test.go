package tiletea

import (
	"math"
	"testing"
)

func TestClickToLatLng(t *testing.T) {
	lat, lng := 40.7128, -74.0060
	m := New(lat, lng, 14)
	m.width, m.height = 100, 40

	gotLat, gotLng, ok := m.clickToLatLng(m.width/2, (m.height-1)/2)
	if !ok {
		t.Fatal("expected ok for center click")
	}
	// Cell clicks are quantized to the middle of a terminal cell, so allow up
	// to half a cell of deviation from the exact center.
	if math.Abs(gotLat-lat) > 1e-3 || math.Abs(gotLng-lng) > 1e-3 {
		t.Fatalf("center click = %f,%f, want %f,%f", gotLat, gotLng, lat, lng)
	}

	eastLat, eastLng, ok := m.clickToLatLng(m.width/2+10, (m.height-1)/2)
	if !ok {
		t.Fatal("expected ok for east-of-center click")
	}
	if math.Abs(eastLat-gotLat) > 1e-9 {
		t.Fatalf("east click changed latitude: %f vs %f", eastLat, gotLat)
	}
	if eastLng <= gotLng {
		t.Fatalf("east click longitude %f should be > center %f", eastLng, gotLng)
	}

	northLat, _, ok := m.clickToLatLng(m.width/2, (m.height-1)/2-5)
	if !ok {
		t.Fatal("expected ok for north-of-center click")
	}
	if northLat <= gotLat {
		t.Fatalf("north click latitude %f should be > center %f", northLat, gotLat)
	}

	if _, _, ok := m.clickToLatLng(-1, 0); ok {
		t.Fatal("expected out-of-bounds column to be rejected")
	}
	if _, _, ok := m.clickToLatLng(0, m.height-1); ok {
		t.Fatal("expected status bar row to be rejected")
	}
}

package tiletea

import (
	"github.com/peterstace/simplefeatures/carto"
	"github.com/peterstace/simplefeatures/geom"
)

// tileSize is the Web Mercator tile size in logical pixels used by maprender.
// It must match maprender.TileSize.
const tileSize = 512

// WithClickCallback registers fn to be invoked with the WGS84 latitude and
// longitude whenever the user clicks (left button) on the map.
//
// fn is called synchronously during the Map's Update, so it should be quick
// and non-blocking.
func WithClickCallback(fn func(lat, lng float64)) Option {
	return func(m *Map) { m.onClick = fn }
}

// SetClickCallback sets or clears (nil) the click callback. See
// WithClickCallback.
func (m *Map) SetClickCallback(fn func(lat, lng float64)) {
	m.onClick = fn
}

// clickToLatLng converts a click at the given terminal cell coordinates,
// relative to the top-left corner of the map viewport, to WGS84 coordinates
// using the current center, zoom and viewport size. It returns false when the
// size is not yet known or the click falls outside the rendered image.
func (m *Map) clickToLatLng(col, row int) (lat, lng float64, ok bool) {
	rows := m.height - 1 // the bottom line shows the status bar
	if m.width == 0 || rows <= 0 || col < 0 || col >= m.width || row < 0 || row >= rows {
		return 0, 0, false
	}

	// Click position in logical pixels relative to the viewport center. The
	// rendered image spans cellWidth x devicePixelRatio physical pixels per
	// column and cellHeight x devicePixelRatio per row.
	logW := float64(m.width*cellWidth) / devicePixelRatio
	logH := float64(rows*cellHeight) / devicePixelRatio
	dx := (float64(col)+0.5)*cellWidth/devicePixelRatio - logW/2
	dy := (float64(row)+0.5)*cellHeight/devicePixelRatio - logH/2

	wm := carto.NewWebMercator(m.zoom)
	center := wm.Forward(geom.XY{X: m.lng, Y: m.lat})
	p := wm.Reverse(geom.XY{
		X: center.X + dx/tileSize,
		Y: center.Y + dy/tileSize,
	})
	return p.Y, p.X, true
}

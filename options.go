package tiletea

import (
	"log/slog"

	"github.com/akhenakh/maprender"
)

const (
	// DefaultStyleURL is the Mapbox GL style fetched when none is configured.
	DefaultStyleURL = "https://tiles.openfreemap.org/styles/liberty"

	// DefaultSourceURL is the TileJSON endpoint used to resolve the tile URL
	// template and source zoom range.
	DefaultSourceURL = "https://tiles.openfreemap.org/planet"

	// DefaultTileURLTemplate is used when the tile source cannot be resolved.
	DefaultTileURLTemplate = "https://tiles.openfreemap.org/planet/{z}/{x}/{y}.pbf"

	// MinZoom and MaxZoom bound the zoom level.
	MinZoom = 0
	MaxZoom = 18

	cellWidth        = 10
	cellHeight       = 20
	devicePixelRatio = 2
)

// Option configures a Map.
type Option func(*Map)

// WithMarker places a marker at the given coordinates.
func WithMarker(lat, lng float64) Option {
	return func(m *Map) {
		m.markerLat = &lat
		m.markerLng = &lng
	}
}

// WithOverlays appends geometry overlays drawn on top of the map. Overlays are
// drawn in WGS84 (lon-lat) coordinates and accept GeoJSON/WKT/WKB-parsed
// geometry via the maprender.OverlayFrom* helpers.
func WithOverlays(overlays ...maprender.Overlay) Option {
	return func(m *Map) {
		m.overlays = append(m.overlays, overlays...)
	}
}

// WithFitOverlays centers and zooms the map to fit the overlays on the first
// render. After the initial fit, the user can pan and zoom freely.
func WithFitOverlays() Option {
	return func(m *Map) { m.fitOverlays = true }
}

// WithStyle supplies a pre-fetched map style, bypassing style fetching.
func WithStyle(style *maprender.MapStyle) Option {
	return func(m *Map) { m.style = style }
}

// WithStyleURL overrides the URL used to fetch the map style.
func WithStyleURL(url string) Option {
	return func(m *Map) { m.styleURL = url }
}

// WithTileSource overrides the TileJSON endpoint used to resolve the tile URL
// template.
func WithTileSource(url string) Option {
	return func(m *Map) { m.sourceURL = url }
}

// WithTileURLTemplate supplies the tile URL template directly, bypassing the
// TileJSON lookup. Use WithSourceMaxZoom to enable overzoom in this case.
func WithTileURLTemplate(template string) Option {
	return func(m *Map) { m.tileURLTemplate = template }
}

// WithSourceMaxZoom sets the source's maximum zoom, used for overzoom when a
// tile URL template is supplied directly.
func WithSourceMaxZoom(maxZoom int) Option {
	return func(m *Map) { m.sourceMaxZoom = maxZoom }
}

// WithLogger sets the logger used for render and debug output. A nil logger is
// ignored.
func WithLogger(logger *slog.Logger) Option {
	return func(m *Map) {
		if logger != nil {
			m.logger = logger
		}
	}
}

// WithAltScreen enables or disables the alternate screen buffer. It is enabled
// by default.
func WithAltScreen(enabled bool) Option {
	return func(m *Map) { m.altScreen = enabled }
}

package tiletea

import (
	tea "charm.land/bubbletea/v2"
	"github.com/akhenakh/maprender"
	"github.com/peterstace/simplefeatures/geom"
)

// GeomView is a Bubble Tea model that renders geometry on an interactive map,
// fitted to the geometry's bounds. It is a thin wrapper around Map that parses
// GeoJSON, WKT, WKB, or a geom.Geometry into overlays and centers the view on
// them.
type GeomView struct {
	m *Map
}

// NewGeomView creates a map view fitted to the given overlays. The map is
// centered and zoomed to the combined bounds of the overlays on first render.
func NewGeomView(overlays []maprender.Overlay, opts ...Option) *GeomView {
	opts = append(opts, WithOverlays(overlays...), WithFitOverlays())
	return &GeomView{m: New(0, 0, 0, opts...)}
}

// NewGeomViewFromGeoJSON parses GeoJSON (a Geometry, Feature, or
// FeatureCollection) and creates a map view fitted to it.
func NewGeomViewFromGeoJSON(data []byte, opts ...Option) (*GeomView, error) {
	overlays, err := maprender.OverlayFromGeoJSON(data)
	if err != nil {
		return nil, err
	}
	return NewGeomView(overlays, opts...), nil
}

// NewGeomViewFromWKT parses a WKT string and creates a map view fitted to it.
func NewGeomViewFromWKT(wkt string, opts ...Option) (*GeomView, error) {
	overlay, err := maprender.OverlayFromWKT(wkt)
	if err != nil {
		return nil, err
	}
	return NewGeomView([]maprender.Overlay{overlay}, opts...), nil
}

// NewGeomViewFromWKB parses a WKB byte slice and creates a map view fitted to
// it.
func NewGeomViewFromWKB(wkb []byte, opts ...Option) (*GeomView, error) {
	overlay, err := maprender.OverlayFromWKB(wkb)
	if err != nil {
		return nil, err
	}
	return NewGeomView([]maprender.Overlay{overlay}, opts...), nil
}

// NewGeomViewFromGeometry creates a map view fitted to a geom.Geometry.
func NewGeomViewFromGeometry(g geom.Geometry, opts ...Option) *GeomView {
	return NewGeomView([]maprender.Overlay{{Geometry: g}}, opts...)
}

// Map returns the underlying map component.
func (g *GeomView) Map() *Map { return g.m }

// Init implements tea.Model.
func (g *GeomView) Init() tea.Cmd { return g.m.Init() }

// Update implements tea.Model.
func (g *GeomView) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	m, cmd := g.m.Update(msg)
	g.m = m.(*Map)
	return g, cmd
}

// View implements tea.Model.
func (g *GeomView) View() tea.View { return g.m.View() }

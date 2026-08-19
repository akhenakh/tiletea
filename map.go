// Package tiletea provides a Bubble Tea component for rendering an
// interactive slippy map in the terminal using the Kitty graphics protocol.
package tiletea

import (
	"context"
	"fmt"
	"image"
	"io"
	"log/slog"
	"math"

	tea "charm.land/bubbletea/v2"
	"github.com/akhenakh/maprender"
)

// Map is a Bubble Tea model that renders an interactive slippy map. It can be
// used standalone or embedded in a larger application.
//
// The zero value is not usable; construct one with New.
type Map struct {
	lat, lng             float64
	zoom                 int
	markerLat, markerLng *float64

	width, height int

	styleURL  string
	sourceURL string
	style     *maprender.MapStyle

	tileURLTemplate string
	sourceMaxZoom   int

	renderedImage *image.RGBA
	kittySequence string
	loading       bool

	ctx    context.Context
	cancel context.CancelFunc

	logger    *slog.Logger
	altScreen bool
}

type mapRenderedMsg struct {
	img *image.RGBA
	seq string
}

// New creates a map component centered at the given coordinates and zoom.
//
// The map style and tile source are fetched synchronously on construction,
// falling back to built-in defaults when the network is unavailable.
func New(lat, lng float64, zoom int, opts ...Option) *Map {
	m := &Map{
		lat:       lat,
		lng:       lng,
		zoom:      zoom,
		styleURL:  DefaultStyleURL,
		sourceURL: DefaultSourceURL,
		logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
		altScreen: true,
		loading:   true,
	}
	for _, opt := range opts {
		opt(m)
	}
	m.resolveStyle()
	m.resolveTileSource()
	return m
}

// Init implements tea.Model.
func (m *Map) Init() tea.Cmd {
	return m.renderMapCmd()
}

// Update implements tea.Model.
func (m *Map) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		return m, m.renderMapCmd()

	case tea.KeyMsg:
		switch msg.String() {
		case "q", "ctrl+c":
			return m, tea.Quit
		case "+", "=":
			if m.zoom < MaxZoom {
				m.zoom++
				return m, m.renderMapCmd()
			}
		case "-":
			if m.zoom > MinZoom {
				m.zoom--
				return m, m.renderMapCmd()
			}
		case "up", "k":
			m.lat += panStep(m.zoom)
			return m, m.renderMapCmd()
		case "down", "j":
			m.lat -= panStep(m.zoom)
			return m, m.renderMapCmd()
		case "left", "h":
			m.lng -= panStep(m.zoom)
			return m, m.renderMapCmd()
		case "right", "l":
			m.lng += panStep(m.zoom)
			return m, m.renderMapCmd()
		}

	case mapRenderedMsg:
		m.loading = false
		m.renderedImage = msg.img
		m.kittySequence = msg.seq
		return m, nil
	}

	return m, nil
}

// View implements tea.Model.
func (m *Map) View() tea.View {
	var content string
	if m.loading && m.kittySequence == "" {
		content = "Loading map...\nControls: Arrows to pan, +/- to zoom, q to quit."
	} else {
		content = fmt.Sprintf("Lat: %.4f | Lng: %.4f | Zoom: %d | Loading: %v\n",
			m.lat, m.lng, m.zoom, m.loading) + m.kittySequence
	}

	v := tea.NewView(content)
	v.AltScreen = m.altScreen
	return v
}

// Center returns the current map center.
func (m *Map) Center() (lat, lng float64) {
	return m.lat, m.lng
}

// Zoom returns the current zoom level.
func (m *Map) Zoom() int {
	return m.zoom
}

// SetMarker sets the optional marker location. A nil lat or lng clears the
// marker.
func (m *Map) SetMarker(lat, lng *float64) {
	m.markerLat = lat
	m.markerLng = lng
}

func (m *Map) renderMapCmd() tea.Cmd {
	if m.cancel != nil {
		m.cancel()
	}
	ctx, cancel := context.WithCancel(context.Background())
	m.ctx = ctx
	m.cancel = cancel
	m.loading = true

	return func() tea.Msg {
		if m.width == 0 || m.height == 0 {
			m.logger.Debug("skipping render", "reason", "zero size")
			return nil
		}

		req := maprender.RenderRequest{
			CenterLat:        m.lat,
			CenterLng:        m.lng,
			Zoom:             m.zoom,
			Width:            m.width * cellWidth,
			Height:           (m.height - 1) * cellHeight,
			DevicePixelRatio: devicePixelRatio,
			Style:            m.style,
			TileURLTemplate:  m.tileURLTemplate,
			SourceMaxZoom:    m.sourceMaxZoom,
			MarkerLat:        m.markerLat,
			MarkerLng:        m.markerLng,
			Logger:           m.logger,
		}

		img, err := maprender.Render(ctx, req)
		if err != nil {
			if ctx.Err() != nil {
				m.logger.Debug("render cancelled")
			} else {
				m.logger.Debug("render failed", "err", err)
			}
			return nil
		}

		return mapRenderedMsg{img: img, seq: encodeKittyGraphics(img, m.width, m.height-1)}
	}
}

func (m *Map) resolveStyle() {
	if m.style != nil {
		return
	}
	style, err := maprender.FetchStyle(m.styleURL)
	if err != nil || style == nil {
		m.logger.Debug("style fetch failed, using fallback", "err", err)
		m.style = fallbackStyle()
		return
	}
	m.logger.Debug("loaded style", "layers", len(style.Layers))
	m.style = style
}

func (m *Map) resolveTileSource() {
	if m.tileURLTemplate != "" {
		return
	}
	tj, err := maprender.FetchTileJSON(m.sourceURL)
	if err != nil || len(tj.Tiles) == 0 {
		m.logger.Debug("tile source fetch failed, using fallback", "err", err)
		m.tileURLTemplate = DefaultTileURLTemplate
		return
	}
	m.tileURLTemplate = tj.Tiles[0]
	m.sourceMaxZoom = tj.MaxZoom
}

func fallbackStyle() *maprender.MapStyle {
	return &maprender.MapStyle{Layers: []maprender.StyleLayer{
		{ID: "background", Type: "background", Paint: maprender.PaintProps{BackgroundColor: "#f8f4f0"}},
	}}
}

func panStep(zoom int) float64 {
	return 10.0 / math.Pow(2, float64(zoom))
}

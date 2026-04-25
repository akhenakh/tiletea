package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"io"
	"math"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/fogleman/gg"
	"github.com/paulmach/orb"
	"github.com/paulmach/orb/encoding/mvt"
	"github.com/paulmach/orb/geojson"
	"github.com/peterstace/simplefeatures/carto"
	"github.com/peterstace/simplefeatures/geom"
)

// Cell dimensions in pixels (approximation for the terminal)
const (
	CellWidth  = 10
	CellHeight = 20
	TileSize   = 256
)

// Default embedded style JSON
const defaultStyleJSON = `
{"version":8,"sources":{"openmaptiles":{"type":"vector","url":"https://tiles.openfreemap.org/planet"}},"layers":[{"id":"background","type":"background","paint":{"background-color":"#f8f4f0"}},{"id":"water","type":"fill","source-layer":"water","paint":{"fill-color":"rgb(158,189,255)"}},{"id":"park","type":"fill","source-layer":"park","paint":{"fill-color":"#d8e8c8"}},{"id":"landcover_wood","type":"fill","source-layer":"landcover","filter":["==","class","wood"],"paint":{"fill-color":"hsla(98,61%,72%,0.7)"}},{"id":"landcover_grass","type":"fill","source-layer":"landcover","filter":["==","class","grass"],"paint":{"fill-color":"rgba(176, 213, 154, 1)"}},{"id":"building","type":"fill","source-layer":"building","paint":{"fill-color":"hsl(35,8%,85%)"}},{"id":"waterway_river","type":"line","source-layer":"waterway","filter":["==","class","river"],"paint":{"line-color":"#a0c8f0","line-width":2}},{"id":"road_motorway","type":"line","source-layer":"transportation","filter":["==","class","motorway"],"paint":{"line-color":"#fc8","line-width":3}},{"id":"road_primary","type":"line","source-layer":"transportation","filter":["==","class","primary"],"paint":{"line-color":"#fea","line-width":2}}]}
`

// --- Style Parsing Types ---

type StyleLayer struct {
	ID          string        `json:"id"`
	Type        string        `json:"type"`
	SourceLayer string        `json:"source-layer"`
	Paint       PaintProps    `json:"paint"`
	Filter      []interface{} `json:"filter"`
}

type PaintProps struct {
	BackgroundColor interface{} `json:"background-color"`
	FillColor       interface{} `json:"fill-color"`
	LineColor       interface{} `json:"line-color"`
	LineWidth       interface{} `json:"line-width"`
}

type MapStyle struct {
	Layers []StyleLayer `json:"layers"`
}

// --- Map Model ---

type MapModel struct {
	Lat, Lng  float64
	Zoom      int
	MarkerLat *float64
	MarkerLng *float64

	Width, Height int // In terminal cells

	TileURLTemplate string
	Style           *MapStyle

	renderedImage *image.RGBA
	kittySequence string
	loading       bool
	ctx           context.Context
	cancel        context.CancelFunc
}

type mapRenderedMsg struct {
	img *image.RGBA
	seq string
}

func NewMapModel(lat, lng float64, zoom int) *MapModel {
	var style MapStyle
	_ = json.Unmarshal([]byte(defaultStyleJSON), &style)

	return &MapModel{
		Lat:             lat,
		Lng:             lng,
		Zoom:            zoom,
		TileURLTemplate: "https://tiles.openfreemap.org/planet/%d/%d/%d.pbf",
		Style:           &style,
		loading:         true,
	}
}

func (m *MapModel) Init() tea.Cmd {
	return m.renderMapCmd()
}

func (m *MapModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {

	case tea.WindowSizeMsg:
		m.Width = msg.Width
		m.Height = msg.Height
		return m, m.renderMapCmd()

	case tea.KeyMsg:
		switch msg.String() {
		case "q", "ctrl+c":
			return m, tea.Quit
		case "+":
			if m.Zoom < 18 {
				m.Zoom++
				return m, m.renderMapCmd()
			}
		case "-":
			if m.Zoom > 0 {
				m.Zoom--
				return m, m.renderMapCmd()
			}
		case "up", "k":
			m.Lat += 10.0 / math.Pow(2, float64(m.Zoom))
			return m, m.renderMapCmd()
		case "down", "j":
			m.Lat -= 10.0 / math.Pow(2, float64(m.Zoom))
			return m, m.renderMapCmd()
		case "left", "h":
			m.Lng -= 10.0 / math.Pow(2, float64(m.Zoom))
			return m, m.renderMapCmd()
		case "right", "l":
			m.Lng += 10.0 / math.Pow(2, float64(m.Zoom))
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

func (m *MapModel) View() tea.View {
	var content string

	if m.loading && m.kittySequence == "" {
		content = "Loading map...\nControls: Arrows to pan, +/- to zoom, q to quit."
	} else {
		// Print the kitty sequence. It binds to the terminal grid at the current cursor.
		// We overlay some basic text information at the top.
		header := fmt.Sprintf("Lat: %.4f | Lng: %.4f | Zoom: %d | Loading: %v\n", m.Lat, m.Lng, m.Zoom, m.loading)
		content = header + m.kittySequence
	}

	// AltScreen is now set on the View struct in Bubble Tea v2
	v := tea.NewView(content)
	v.AltScreen = true
	return v
}

// --- Map Rendering Logic ---

func (m *MapModel) renderMapCmd() tea.Cmd {
	if m.cancel != nil {
		m.cancel()
	}
	ctx, cancel := context.WithCancel(context.Background())
	m.ctx = ctx
	m.cancel = cancel
	m.loading = true

	return func() tea.Msg {
		if m.Width == 0 || m.Height == 0 {
			return nil
		}

		pxWidth := m.Width * CellWidth
		pxHeight := (m.Height - 1) * CellHeight // -1 for header

		// Use simplefeatures/carto to get WebMercator coordinates
		wm := carto.NewWebMercator(m.Zoom)

		// Transform Lat/Lng to WebMercator tile space [0, 2^Zoom]
		centerXY := wm.Forward(geom.XY{X: m.Lng, Y: m.Lat})

		globalPxX := centerXY.X * TileSize
		globalPxY := centerXY.Y * TileSize

		minPxX := globalPxX - float64(pxWidth)/2
		minPxY := globalPxY - float64(pxHeight)/2
		maxPxX := globalPxX + float64(pxWidth)/2
		maxPxY := globalPxY + float64(pxHeight)/2

		minTileX := int(math.Floor(minPxX / TileSize))
		minTileY := int(math.Floor(minPxY / TileSize))
		maxTileX := int(math.Floor(maxPxX / TileSize))
		maxTileY := int(math.Floor(maxPxY / TileSize))

		dc := gg.NewContext(pxWidth, pxHeight)

		// Draw Background
		if bgLayer := getLayerByID(m.Style, "background"); bgLayer != nil {
			dc.SetColor(parseColor(bgLayer.Paint.BackgroundColor))
			dc.Clear()
		} else {
			dc.SetColor(color.RGBA{248, 244, 240, 255})
			dc.Clear()
		}

		// Fetch and render tiles
		for ty := minTileY; ty <= maxTileY; ty++ {
			for tx := minTileX; tx <= maxTileX; tx++ {
				// Stop if context cancelled by newer render
				if ctx.Err() != nil {
					return nil
				}

				// Handle world wrap
				maxTiles := 1 << m.Zoom
				wrapTx := (tx%maxTiles + maxTiles) % maxTiles

				tileData, err := fetchTile(fmt.Sprintf(m.TileURLTemplate, m.Zoom, wrapTx, ty))
				if err != nil {
					continue
				}

				collections, err := mvt.UnmarshalGzipped(tileData)
				if err != nil {
					collections, err = mvt.Unmarshal(tileData)
					if err != nil {
						continue
					}
				}

				offsetX := float64(tx*TileSize) - minPxX
				offsetY := float64(ty*TileSize) - minPxY

				// Render layers in the exact order specified by style.json
				for _, layerStyle := range m.Style.Layers {
					if layerStyle.Type == "background" {
						continue
					}

					var mvtLayer *mvt.Layer
					for _, l := range collections {
						if l.Name == layerStyle.SourceLayer {
							mvtLayer = l
							break
						}
					}
					if mvtLayer == nil {
						continue
					}

					scale := TileSize / float64(mvtLayer.Extent)

					for _, feature := range mvtLayer.Features {
						if !evaluateFilter(layerStyle.Filter, feature.Properties) {
							continue
						}

						drawFeature(dc, feature.Geometry, offsetX, offsetY, scale, &layerStyle)
					}
				}
			}
		}

		// Optional: Draw Marker
		if m.MarkerLat != nil && m.MarkerLng != nil {
			markerXY := wm.Forward(geom.XY{X: *m.MarkerLng, Y: *m.MarkerLat})
			mx := markerXY.X*TileSize - minPxX
			my := markerXY.Y*TileSize - minPxY

			dc.SetColor(color.RGBA{255, 0, 0, 255})
			dc.DrawCircle(mx, my, 6)
			dc.Fill()
			dc.SetColor(color.RGBA{255, 255, 255, 255})
			dc.DrawCircle(mx, my, 6)
			dc.SetLineWidth(2)
			dc.Stroke()
		}

		img := dc.Image().(*image.RGBA)
		seq := encodeKittyGraphics(img, m.Width, m.Height-1)

		return mapRenderedMsg{img: img, seq: seq}
	}
}

func drawFeature(dc *gg.Context, geometry orb.Geometry, offsetX, offsetY, scale float64, style *StyleLayer) {
	switch g := geometry.(type) {
	case orb.Polygon:
		dc.SetColor(parseColor(style.Paint.FillColor))
		for _, ring := range g {
			for i, pt := range ring {
				x := offsetX + pt[0]*scale
				y := offsetY + pt[1]*scale
				if i == 0 {
					dc.MoveTo(x, y)
				} else {
					dc.LineTo(x, y)
				}
			}
			dc.ClosePath()
		}
		dc.Fill()

	case orb.MultiPolygon:
		dc.SetColor(parseColor(style.Paint.FillColor))
		for _, poly := range g {
			for _, ring := range poly {
				for i, pt := range ring {
					x := offsetX + pt[0]*scale
					y := offsetY + pt[1]*scale
					if i == 0 {
						dc.MoveTo(x, y)
					} else {
						dc.LineTo(x, y)
					}
				}
				dc.ClosePath()
			}
		}
		dc.Fill()

	case orb.LineString:
		dc.SetColor(parseColor(style.Paint.LineColor))
		lw := 1.0
		if w, ok := style.Paint.LineWidth.(float64); ok {
			lw = w
		}
		dc.SetLineWidth(lw)
		for i, pt := range g {
			x := offsetX + pt[0]*scale
			y := offsetY + pt[1]*scale
			if i == 0 {
				dc.MoveTo(x, y)
			} else {
				dc.LineTo(x, y)
			}
		}
		dc.Stroke()

	case orb.MultiLineString:
		dc.SetColor(parseColor(style.Paint.LineColor))
		lw := 1.0
		if w, ok := style.Paint.LineWidth.(float64); ok {
			lw = w
		}
		dc.SetLineWidth(lw)
		for _, ls := range g {
			for i, pt := range ls {
				x := offsetX + pt[0]*scale
				y := offsetY + pt[1]*scale
				if i == 0 {
					dc.MoveTo(x, y)
				} else {
					dc.LineTo(x, y)
				}
			}
		}
		dc.Stroke()
	}
}

// --- Fetching & Graphics Encoders ---

func fetchTile(url string) ([]byte, error) {
	req, _ := http.NewRequest("GET", url, nil)
	req.Header.Set("Accept-Encoding", "gzip")
	req.Header.Set("User-Agent", "Charm-Bubbletea-MapViewer")

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("bad status: %d", resp.StatusCode)
	}

	var reader io.Reader = resp.Body
	if resp.Header.Get("Content-Encoding") == "gzip" {
		reader, _ = gzip.NewReader(resp.Body)
	}

	var buf bytes.Buffer
	_, err = io.Copy(&buf, reader)
	return buf.Bytes(), err
}

func encodeKittyGraphics(img *image.RGBA, cols, rows int) string {
	encoded := base64.StdEncoding.EncodeToString(img.Pix)
	chunkSize := 4096

	var b bytes.Buffer
	first := true

	for i := 0; i < len(encoded); i += chunkSize {
		end := i + chunkSize
		if end > len(encoded) {
			end = len(encoded)
		}
		chunk := encoded[i:end]

		m := 1
		if end == len(encoded) {
			m = 0
		}

		if first {
			// a=T: transmit & display
			// f=32: 32-bit RGBA
			// C=1: Do not move cursor! Keeps layout static.
			// z=-1: Draw behind text (allows overlays)
			b.WriteString(fmt.Sprintf("\033_Ga=T,f=32,s=%d,v=%d,c=%d,r=%d,C=1,z=-1,m=%d;%s\033\\",
				img.Bounds().Dx(), img.Bounds().Dy(), cols, rows, m, chunk))
			first = false
		} else {
			b.WriteString(fmt.Sprintf("\033_Gm=%d;%s\033\\", m, chunk))
		}
	}
	return b.String()
}

// --- Styling Helpers ---

func getLayerByID(style *MapStyle, id string) *StyleLayer {
	for _, l := range style.Layers {
		if l.ID == id {
			return &l
		}
	}
	return nil
}

func evaluateFilter(filter []interface{}, props geojson.Properties) bool {
	if len(filter) == 0 {
		return true
	}

	// Basic parsing for ["==", "class", "water"]
	if len(filter) == 3 {
		op, okOp := filter[0].(string)
		key, okKey := filter[1].(string)
		val := filter[2]

		if okOp && okKey {
			propVal, hasProp := props[key]
			if !hasProp {
				// Handle typical mapbox style where they use ["==", ["get", "class"], "water"]
				if getArr, isArr := filter[1].([]interface{}); isArr && len(getArr) == 2 && getArr[0] == "get" {
					key = getArr[1].(string)
					propVal, hasProp = props[key]
				}
			}

			if hasProp {
				switch op {
				case "==":
					return fmt.Sprintf("%v", propVal) == fmt.Sprintf("%v", val)
				case "!=":
					return fmt.Sprintf("%v", propVal) != fmt.Sprintf("%v", val)
				}
			}
		}
	}

	// Default pass-through for unhandled complex filters
	return true
}

func parseColor(val interface{}) color.Color {
	cStr, ok := val.(string)
	if !ok {
		return color.RGBA{0, 0, 0, 0}
	}

	cStr = strings.TrimSpace(cStr)

	// Hex (#RRGGBB or #RGB)
	if strings.HasPrefix(cStr, "#") {
		hex := strings.TrimPrefix(cStr, "#")
		if len(hex) == 3 {
			hex = string([]byte{hex[0], hex[0], hex[1], hex[1], hex[2], hex[2]})
		}
		if len(hex) == 6 {
			r, _ := strconv.ParseUint(hex[0:2], 16, 8)
			g, _ := strconv.ParseUint(hex[2:4], 16, 8)
			b, _ := strconv.ParseUint(hex[4:6], 16, 8)
			return color.RGBA{uint8(r), uint8(g), uint8(b), 255}
		}
	}

	// rgba(r, g, b, a) or rgb(r, g, b)
	if strings.HasPrefix(cStr, "rgb") {
		re := regexp.MustCompile(`rgba?\(\s*(\d+)\s*,\s*(\d+)\s*,\s*(\d+)(?:\s*,\s*([\d.]+))?\s*\)`)
		matches := re.FindStringSubmatch(cStr)
		if len(matches) >= 4 {
			r, _ := strconv.ParseUint(matches[1], 10, 8)
			g, _ := strconv.ParseUint(matches[2], 10, 8)
			b, _ := strconv.ParseUint(matches[3], 10, 8)
			a := uint8(255)
			if len(matches) == 5 && matches[4] != "" {
				af, _ := strconv.ParseFloat(matches[4], 64)
				a = uint8(af * 255)
			}
			return color.RGBA{uint8(r), uint8(g), uint8(b), a}
		}
	}

	// hsla(h, s%, l%, a) or hsl(h, s%, l%)
	if strings.HasPrefix(cStr, "hsl") {
		re := regexp.MustCompile(`hsla?\(\s*(\d+)\s*,\s*([\d.]+)%\s*,\s*([\d.]+)%(?:\s*,\s*([\d.]+))?\s*\)`)
		matches := re.FindStringSubmatch(cStr)
		if len(matches) >= 4 {
			h, _ := strconv.ParseFloat(matches[1], 64)
			s, _ := strconv.ParseFloat(matches[2], 64)
			l, _ := strconv.ParseFloat(matches[3], 64)
			a := 1.0
			if len(matches) == 5 && matches[4] != "" {
				a, _ = strconv.ParseFloat(matches[4], 64)
			}
			return hslToRGBA(h, s, l, a)
		}
	}

	return color.RGBA{0, 0, 0, 0}
}

func hslToRGBA(h, s, l, a float64) color.RGBA {
	s /= 100
	l /= 100
	var r, g, b float64
	if s == 0 {
		r, g, b = l, l, l
	} else {
		var q float64
		if l < 0.5 {
			q = l * (1 + s)
		} else {
			q = l + s - l*s
		}
		p := 2*l - q
		r = hueToRGB(p, q, h/360+1.0/3)
		g = hueToRGB(p, q, h/360)
		b = hueToRGB(p, q, h/360-1.0/3)
	}
	return color.RGBA{uint8(r * 255), uint8(g * 255), uint8(b * 255), uint8(a * 255)}
}

func hueToRGB(p, q, t float64) float64 {
	if t < 0 {
		t += 1
	}
	if t > 1 {
		t -= 1
	}
	if t < 1.0/6 {
		return p + (q-p)*6*t
	}
	if t < 1.0/2 {
		return q
	}
	if t < 2.0/3 {
		return p + (q-p)*(2.0/3-t)*6
	}
	return p
}

// --- Main execution ---

func main() {
	// NYC Coordinates
	nycLat, nycLng := 40.7128, -74.0060

	// Initialize the Bubbletea map model
	model := NewMapModel(nycLat, nycLng, 14)

	// Set an optional marker at the exact center
	model.MarkerLat = &nycLat
	model.MarkerLng = &nycLng

	p := tea.NewProgram(model) // WithAltScreen is handled in the View() func now
	if _, err := p.Run(); err != nil {
		fmt.Printf("Bubbletea Error: %v\n", err)
	}
}

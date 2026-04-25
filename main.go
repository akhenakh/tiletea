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
	"log"
	"math"
	"net/http"
	"os"
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

var debugEnabled bool

func debugf(format string, args ...interface{}) {
	if debugEnabled {
		log.Printf(format, args...)
	}
}

func debugln(args ...interface{}) {
	if debugEnabled {
		log.Println(args...)
	}
}

// Cell dimensions in pixels (approximation for the terminal)
const (
	CellWidth  = 10
	CellHeight = 20
	TileSize   = 256
)

const defaultStyleURL = "https://tiles.openfreemap.org/styles/liberty"

type StyleLayer struct {
	ID          string        `json:"id"`
	Type        string        `json:"type"`
	SourceLayer string        `json:"source-layer"`
	Paint       PaintProps    `json:"paint"`
	Filter      []interface{} `json:"filter"`
	MinZoom     *float64      `json:"minzoom"`
	MaxZoom     *float64      `json:"maxzoom"`
}

type PaintProps struct {
	BackgroundColor interface{} `json:"background-color"`
	FillColor       interface{} `json:"fill-color"`
	FillOpacity     interface{} `json:"fill-opacity"`
	LineColor       interface{} `json:"line-color"`
	LineWidth       interface{} `json:"line-width"`
	LineOpacity     interface{} `json:"line-opacity"`
	LineDashArray   interface{} `json:"line-dasharray"`
}

type MapStyle struct {
	Layers []StyleLayer `json:"layers"`
}

type TileJSON struct {
	Tiles []string `json:"tiles"`
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

func fetchStyle(styleURL string) *MapStyle {
	resp, err := http.Get(styleURL)
	if err != nil {
		debugf("Failed to fetch style from %s: %v", styleURL, err)
		return nil
	}
	defer resp.Body.Close()

	var raw struct {
		Layers []StyleLayer `json:"layers"`
		Sources map[string]struct {
			Type string `json:"type"`
			URL  string `json:"url"`
		} `json:"sources"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		debugf("Failed to decode style JSON: %v", err)
		return nil
	}

	renderable := make([]StyleLayer, 0, len(raw.Layers))
	for _, l := range raw.Layers {
		switch l.Type {
		case "background", "fill", "line":
			renderable = append(renderable, l)
		default:
			debugf("Skipping non-renderable layer %q (type=%s)", l.ID, l.Type)
		}
	}

	debugf("Loaded style with %d renderable layers (out of %d total)", len(renderable), len(raw.Layers))
	return &MapStyle{Layers: renderable}
}

func fetchTileURLTemplate(sourceURL string) string {
	resp, err := http.Get(sourceURL)
	if err != nil {
		debugf("Failed to fetch TileJSON from %s: %v", sourceURL, err)
		return ""
	}
	defer resp.Body.Close()

	var tj TileJSON
	if err := json.NewDecoder(resp.Body).Decode(&tj); err != nil {
		debugf("Failed to decode TileJSON: %v", err)
		return ""
	}
	if len(tj.Tiles) == 0 {
		debugf("TileJSON contains no tile URLs")
		return ""
	}

	template := tj.Tiles[0]
	debugf("TileJSON resolved tile URL template: %s", template)
	return template
}

func NewMapModel(lat, lng float64, zoom int) *MapModel {
	style := fetchStyle(defaultStyleURL)
	if style == nil {
		debugf("Falling back to hardcoded style")
		style = &MapStyle{Layers: []StyleLayer{
			{ID: "background", Type: "background", Paint: PaintProps{BackgroundColor: "#f8f4f0"}},
		}}
	}

	tileURLTemplate := fetchTileURLTemplate("https://tiles.openfreemap.org/planet")
	if tileURLTemplate == "" {
		tileURLTemplate = "https://tiles.openfreemap.org/planet/{z}/{x}/{y}.pbf"
	}

	return &MapModel{
		Lat:             lat,
		Lng:             lng,
		Zoom:            zoom,
		TileURLTemplate: tileURLTemplate,
		Style:           style,
		loading:         true,
	}
}

func (m *MapModel) Init() tea.Cmd {
	return m.renderMapCmd()
}

func (m *MapModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {

	case tea.WindowSizeMsg:
		debugf("Window resized to Width: %d, Height: %d", msg.Width, msg.Height)
		m.Width = msg.Width
		m.Height = msg.Height
		return m, m.renderMapCmd()

	case tea.KeyMsg:
		switch msg.String() {
		case "q", "ctrl+c":
			debugln("Quit requested.")
			return m, tea.Quit
		case "+":
			if m.Zoom < 18 {
				m.Zoom++
				debugf("Zooming in to %d", m.Zoom)
				return m, m.renderMapCmd()
			}
		case "-":
			if m.Zoom > 0 {
				m.Zoom--
				debugf("Zooming out to %d", m.Zoom)
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
		debugln("Map render completed and received by Update.")
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
		header := fmt.Sprintf("Lat: %.4f | Lng: %.4f | Zoom: %d | Loading: %v\n", m.Lat, m.Lng, m.Zoom, m.loading)
		content = header + m.kittySequence
	}

	v := tea.NewView(content)
	v.AltScreen = true
	return v
}

// --- Map Rendering Logic ---

func (m *MapModel) renderMapCmd() tea.Cmd {
	if m.cancel != nil {
		m.cancel() // Cancel any ongoing render
	}
	ctx, cancel := context.WithCancel(context.Background())
	m.ctx = ctx
	m.cancel = cancel
	m.loading = true

	return func() tea.Msg {
		if m.Width == 0 || m.Height == 0 {
			debugln("Skipping render: width or height is 0")
			return nil
		}

		pxWidth := m.Width * CellWidth
		pxHeight := (m.Height - 1) * CellHeight // -1 for header

		debugf("Starting map render: %dx%d pixels (zoom %d, lat: %.4f, lng: %.4f)", pxWidth, pxHeight, m.Zoom, m.Lat, m.Lng)

		wm := carto.NewWebMercator(m.Zoom)

		centerXY := wm.Forward(geom.XY{X: m.Lng, Y: m.Lat})
		globalPxX := centerXY.X * TileSize
		globalPxY := centerXY.Y * TileSize
		debugf("Center XY: (%.4f, %.4f) -> globalPx: (%.2f, %.2f)", centerXY.X, centerXY.Y, globalPxX, globalPxY)

		minPxX := globalPxX - float64(pxWidth)/2
		minPxY := globalPxY - float64(pxHeight)/2
		maxPxX := globalPxX + float64(pxWidth)/2
		maxPxY := globalPxY + float64(pxHeight)/2

		minTileX := int(math.Floor(minPxX / TileSize))
		minTileY := int(math.Floor(minPxY / TileSize))
		maxTileX := int(math.Floor(maxPxX / TileSize))
		maxTileY := int(math.Floor(maxPxY / TileSize))

		debugf("Pixel bounds: X[%.2f-%.2f], Y[%.2f-%.2f]", minPxX, maxPxX, minPxY, maxPxY)

		dc := gg.NewContext(pxWidth, pxHeight)

		// Draw Background
		if bgLayer := getLayerByID(m.Style, "background"); bgLayer != nil {
			dc.SetColor(parseColor(resolvePaintValue(bgLayer.Paint.BackgroundColor, float64(m.Zoom))))
		} else {
			dc.SetColor(color.RGBA{248, 244, 240, 255})
		}
		dc.Clear()

		debugf("Tiles required: X[%d-%d], Y[%d-%d]", minTileX, maxTileX, minTileY, maxTileY)

		for ty := minTileY; ty <= maxTileY; ty++ {
			for tx := minTileX; tx <= maxTileX; tx++ {
				if ctx.Err() != nil {
					debugln("Render cancelled by context")
					return nil
				}

				maxTiles := 1 << m.Zoom
				wrapTx := (tx%maxTiles + maxTiles) % maxTiles

				url := strings.ReplaceAll(m.TileURLTemplate, "{z}", strconv.Itoa(m.Zoom))
				url = strings.ReplaceAll(url, "{x}", strconv.Itoa(wrapTx))
				url = strings.ReplaceAll(url, "{y}", strconv.Itoa(ty))
				debugf("Fetching tile: %s", url)

				tileData, err := fetchTile(url)
				if err != nil {
					debugf("Failed to fetch tile %s: %v", url, err)
					continue
				}

				debugf("Fetched %d bytes for tile %d/%d/%d", len(tileData), m.Zoom, wrapTx, ty)

				// Try to unmarshal (it might be gzipped, or our fetchTile already decompressed it)
				collections, err := mvt.UnmarshalGzipped(tileData)
				if err != nil {
					debugf("UnmarshalGzipped failed: %v, trying plain Unmarshal", err)
					collections, err = mvt.Unmarshal(tileData)
					if err != nil {
						debugf("Failed to unmarshal tile data: %v", err)
						continue
					}
				}

				debugf("Successfully unmarshalled tile. Found %d MVT layers.", len(collections))
				for _, l := range collections {
					debugf("  MVT layer: %q (extent=%d, features=%d)", l.Name, l.Extent, len(l.Features))
				}

				offsetX := float64(tx*TileSize) - minPxX
				offsetY := float64(ty*TileSize) - minPxY

				for _, layerStyle := range m.Style.Layers {
					if layerStyle.Type == "background" {
						continue
					}
					if layerStyle.MinZoom != nil && float64(m.Zoom) < *layerStyle.MinZoom {
						continue
					}
					if layerStyle.MaxZoom != nil && float64(m.Zoom) > *layerStyle.MaxZoom {
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
						debugf("  Style layer %q (source: %q): no matching MVT layer found", layerStyle.ID, layerStyle.SourceLayer)
						continue
					}

					scale := TileSize / float64(mvtLayer.Extent)
					debugf("  Style layer %q matched MVT %q: scale=%.4f, offsetX=%.2f, offsetY=%.2f, extent=%d, %d features",
						layerStyle.ID, layerStyle.SourceLayer, scale, offsetX, offsetY, mvtLayer.Extent, len(mvtLayer.Features))
					renderedCount := 0
					filteredCount := 0

					for _, feature := range mvtLayer.Features {
						if !evaluateFilter(layerStyle.Filter, feature.Properties, feature.Geometry) {
							filteredCount++
							continue
						}

						drawFeature(dc, feature.Geometry, offsetX, offsetY, scale, &layerStyle, float64(m.Zoom))
						renderedCount++
					}

					if renderedCount > 0 {
						debugf("  -> Layer '%s' (source: %s): rendered %d features, filtered %d", layerStyle.ID, layerStyle.SourceLayer, renderedCount, filteredCount)
					} else if filteredCount > 0 {
						debugf("  -> Layer '%s' (source: %s): ALL %d features filtered out", layerStyle.ID, layerStyle.SourceLayer, filteredCount)
					}
				}
			}
		}

		// Optional: Draw Marker
		if m.MarkerLat != nil && m.MarkerLng != nil {
			markerXY := wm.Forward(geom.XY{X: *m.MarkerLng, Y: *m.MarkerLat})
			mx := markerXY.X*TileSize - minPxX
			my := markerXY.Y*TileSize - minPxY

			debugf("Drawing marker at screen px: (%.2f, %.2f)", mx, my)

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

		debugln("Render complete, sending mapRenderedMsg")
		return mapRenderedMsg{img: img, seq: seq}
	}
}

func drawFeature(dc *gg.Context, geometry orb.Geometry, offsetX, offsetY, scale float64, style *StyleLayer, zoom float64) {
	switch g := geometry.(type) {
	case orb.Polygon:
		c := resolvePaintValue(style.Paint.FillColor, zoom)
		dc.SetColor(parseColor(c))
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
		c := resolvePaintValue(style.Paint.FillColor, zoom)
		dc.SetColor(parseColor(c))
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
		c := resolvePaintValue(style.Paint.LineColor, zoom)
		dc.SetColor(parseColor(c))
		lw := resolveLineWidth(style.Paint.LineWidth, zoom)
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
		c := resolvePaintValue(style.Paint.LineColor, zoom)
		dc.SetColor(parseColor(c))
		lw := resolveLineWidth(style.Paint.LineWidth, zoom)
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

	case orb.Point:
		c := resolvePaintValue(style.Paint.FillColor, zoom)
		dc.SetColor(parseColor(c))
		x := offsetX + g[0]*scale
		y := offsetY + g[1]*scale
		dc.DrawCircle(x, y, 3)
		dc.Fill()

	case orb.MultiPoint:
		c := resolvePaintValue(style.Paint.FillColor, zoom)
		dc.SetColor(parseColor(c))
		for _, pt := range g {
			x := offsetX + pt[0]*scale
			y := offsetY + pt[1]*scale
			dc.DrawCircle(x, y, 3)
			dc.Fill()
		}
	}
}

func resolveLineWidth(val interface{}, zoom float64) float64 {
	if val == nil {
		return 1.0
	}
	r := resolvePaintValue(val, zoom)
	if f, ok := toFloat(r); ok {
		return f
	}
	return 1.0
}

// --- Fetching & Graphics Encoders ---

func fetchTile(url string) ([]byte, error) {
	req, _ := http.NewRequest("GET", url, nil)
	// We set this to explicitly ask for gzip, but Go's http.Transport
	// WILL still decompress it automatically and remove the header.
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
	// Just in case the server sent gzip and Go's transport DIDN'T strip it
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

func evalExpr(expr interface{}, zoom float64) interface{} {
	arr, ok := expr.([]interface{})
	if !ok {
		return expr
	}
	if len(arr) == 0 {
		return expr
	}

	switch arr[0] {
	case "interpolate":
		return evalInterpolate(arr, zoom)
	case "coalesce":
		for _, v := range arr[1:] {
			r := evalExpr(v, zoom)
			if r != nil {
				return r
			}
		}
		return nil
	case "get":
		return nil
	case "match":
		return arr[len(arr)-1]
	case "step":
		return evalStep(arr, zoom)
	default:
		return nil
	}
}

func evalInterpolate(arr []interface{}, zoom float64) interface{} {
	if len(arr) < 5 {
		return nil
	}
	_, ok := arr[2].([]interface{})
	if !ok {
		return nil
	}
	zoomExpr := arr[2]
	if ze, ok := zoomExpr.([]interface{}); !ok || len(ze) < 2 || ze[0] != "zoom" {
		return nil
	}

	type kv struct {
		z float64
		v interface{}
	}
	var pairs []kv
	for i := 3; i < len(arr)-1; i += 2 {
		zf, _ := toFloat(arr[i])
		pairs = append(pairs, kv{zf, arr[i+1]})
	}
	if len(pairs) < 2 {
		return pairs[0].v
	}

	if zoom <= pairs[0].z {
		return pairs[0].v
	}
	if zoom >= pairs[len(pairs)-1].z {
		return pairs[len(pairs)-1].v
	}

	for i := 0; i < len(pairs)-1; i++ {
		if zoom >= pairs[i].z && zoom <= pairs[i+1].z {
			z1, v1 := pairs[i].z, pairs[i].v
			z2, v2 := pairs[i+1].z, pairs[i+1].v
			v1f, ok1 := toFloat(v1)
			v2f, ok2 := toFloat(v2)
			if ok1 && ok2 && z2 != z1 {
				t := (zoom - z1) / (z2 - z1)
				return v1f + t*(v2f-v1f)
			}
			return v1
		}
	}
	return pairs[len(pairs)-1].v
}

func evalStep(arr []interface{}, zoom float64) interface{} {
	if len(arr) < 4 {
		return nil
	}
	zoomExpr := arr[1]
	if ze, ok := zoomExpr.([]interface{}); !ok || len(ze) < 2 || ze[0] != "zoom" {
		return nil
	}
	output := arr[2]
	for i := 3; i < len(arr)-1; i += 2 {
		zf, _ := toFloat(arr[i])
		if zoom < zf {
			return output
		}
		output = arr[i+1]
	}
	return output
}

func toFloat(v interface{}) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case int:
		return float64(n), true
	case json.Number:
		f, err := n.Float64()
		return f, err == nil
	default:
		return 0, false
	}
}

func resolvePaintValue(val interface{}, zoom float64) interface{} {
	return evalExpr(val, zoom)
}

func getLayerByID(style *MapStyle, id string) *StyleLayer {
	for _, l := range style.Layers {
		if l.ID == id {
			return &l
		}
	}
	return nil
}

func evaluateFilter(filter []interface{}, props geojson.Properties, geom orb.Geometry) bool {
	if len(filter) == 0 {
		return true
	}
	result := evalFilterExpr(filter, props, geom)
	if b, ok := result.(bool); ok {
		return b
	}
	return true
}

func geometryTypeName(g orb.Geometry) string {
	switch g.(type) {
	case orb.Point:
		return "Point"
	case orb.MultiPoint:
		return "MultiPoint"
	case orb.LineString:
		return "LineString"
	case orb.MultiLineString:
		return "MultiLineString"
	case orb.Polygon:
		return "Polygon"
	case orb.MultiPolygon:
		return "MultiPolygon"
	case orb.Collection:
		return "GeometryCollection"
	default:
		return ""
	}
}

func evalFilterExpr(expr interface{}, props geojson.Properties, geom orb.Geometry) interface{} {
	arr, ok := expr.([]interface{})
	if !ok {
		return expr
	}
	if len(arr) == 0 {
		return expr
	}

	op, ok := arr[0].(string)
	if !ok {
		return true
	}

	switch op {
	case "get":
		if len(arr) == 2 {
			if key, ok := arr[1].(string); ok {
				if key == "geometry-type" {
					return geometryTypeName(geom)
				}
				if v, has := props[key]; has {
					return v
				}
			}
		}
		return nil

	case "geometry-type":
		return geometryTypeName(geom)

	case "match":
		return evalMatch(arr, props, geom)

	case "coalesce":
		for _, v := range arr[1:] {
			r := evalFilterExpr(v, props, geom)
			if r != nil {
				return r
			}
		}
		return nil

	case "==":
		if len(arr) == 3 {
			lhs := evalFilterExpr(arr[1], props, geom)
			rhs := evalFilterExpr(arr[2], props, geom)
			return fmt.Sprintf("%v", lhs) == fmt.Sprintf("%v", rhs)
		}
		return true

	case "!=":
		if len(arr) == 3 {
			lhs := evalFilterExpr(arr[1], props, geom)
			rhs := evalFilterExpr(arr[2], props, geom)
			return fmt.Sprintf("%v", lhs) != fmt.Sprintf("%v", rhs)
		}
		return true

	case ">":
		if len(arr) == 3 {
			lf, ok1 := toFloat(evalFilterExpr(arr[1], props, geom))
			rf, ok2 := toFloat(evalFilterExpr(arr[2], props, geom))
			if ok1 && ok2 {
				return lf > rf
			}
		}
		return true

	case ">=":
		if len(arr) == 3 {
			lf, ok1 := toFloat(evalFilterExpr(arr[1], props, geom))
			rf, ok2 := toFloat(evalFilterExpr(arr[2], props, geom))
			if ok1 && ok2 {
				return lf >= rf
			}
		}
		return true

	case "<":
		if len(arr) == 3 {
			lf, ok1 := toFloat(evalFilterExpr(arr[1], props, geom))
			rf, ok2 := toFloat(evalFilterExpr(arr[2], props, geom))
			if ok1 && ok2 {
				return lf < rf
			}
		}
		return true

	case "<=":
		if len(arr) == 3 {
			lf, ok1 := toFloat(evalFilterExpr(arr[1], props, geom))
			rf, ok2 := toFloat(evalFilterExpr(arr[2], props, geom))
			if ok1 && ok2 {
				return lf <= rf
			}
		}
		return true

	case "in":
		if len(arr) >= 3 {
			lhs := fmt.Sprintf("%v", evalFilterExpr(arr[1], props, geom))
			for _, v := range arr[2:] {
				if fmt.Sprintf("%v", evalFilterExpr(v, props, geom)) == lhs {
					return true
				}
			}
			return false
		}

	case "!in":
		if len(arr) >= 3 {
			lhs := fmt.Sprintf("%v", evalFilterExpr(arr[1], props, geom))
			for _, v := range arr[2:] {
				if fmt.Sprintf("%v", evalFilterExpr(v, props, geom)) == lhs {
					return false
				}
			}
			return true
		}

	case "has":
		if len(arr) == 2 {
			key := resolveGetKey(arr[1])
			_, ok := props[key]
			return ok
		}

	case "!has":
		if len(arr) == 2 {
			key := resolveGetKey(arr[1])
			_, ok := props[key]
			return !ok
		}

	case "all":
		for _, f := range arr[1:] {
			r := evalFilterExpr(f, props, geom)
			if b, ok := r.(bool); ok && !b {
				return false
			}
		}
		return true

	case "any":
		for _, f := range arr[1:] {
			r := evalFilterExpr(f, props, geom)
			if b, ok := r.(bool); ok && b {
				return true
			}
		}
		return false

	case "!":
		if len(arr) == 2 {
			r := evalFilterExpr(arr[1], props, geom)
			if b, ok := r.(bool); ok {
				return !b
			}
		}
	}

	return true
}

func evalMatch(arr []interface{}, props geojson.Properties, geom orb.Geometry) interface{} {
	if len(arr) < 4 {
		return arr[len(arr)-1]
	}
	input := evalFilterExpr(arr[1], props, geom)
	inputStr := fmt.Sprintf("%v", input)

	for i := 2; i < len(arr)-1; i += 2 {
		labels := arr[i]
		output := arr[i+1]

		switch l := labels.(type) {
		case []interface{}:
			for _, label := range l {
				if fmt.Sprintf("%v", evalFilterExpr(label, props, geom)) == inputStr {
					return output
				}
			}
		default:
			if fmt.Sprintf("%v", evalFilterExpr(labels, props, geom)) == inputStr {
				return output
			}
		}
	}
	return arr[len(arr)-1]
}

func resolveGetKey(key interface{}) string {
	if s, ok := key.(string); ok {
		return s
	}
	if arr, ok := key.([]interface{}); ok && len(arr) == 2 {
		if arr[0] == "get" {
			if s, ok := arr[1].(string); ok {
				return s
			}
		}
	}
	return ""
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
	if len(os.Getenv("DEBUG")) > 0 {
		debugEnabled = true
		f, err := tea.LogToFile("debug.log", "debug")
		if err != nil {
			fmt.Println("fatal:", err)
			os.Exit(1)
		}
		defer f.Close()
		debugln("=== Starting Map Viewer ===")
	}

	nycLat, nycLng := 40.7128, -74.0060

	// Initialize the Bubbletea map model
	model := NewMapModel(nycLat, nycLng, 14)

	// Set an optional marker at the exact center
	model.MarkerLat = &nycLat
	model.MarkerLng = &nycLng

	p := tea.NewProgram(model)
	if _, err := p.Run(); err != nil {
		fmt.Printf("Bubbletea Error: %v\n", err)
	}
}

package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/binary"
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

const (
	CellWidth  = 10
	CellHeight = 20
	TileSize   = 512

	DevicePixelRatio = 2
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

// --- MVT Parsing ---

type MVTLayer struct {
	Name     string
	Extent   uint32
	Features []MVTFeature
}

type MVTFeature struct {
	ID         uint64
	Type       int // 1=Point, 2=LineString, 3=Polygon
	Properties map[string]interface{}
	Geometry   geom.Geometry
}

func readVarint(buf []byte, offset int) (uint64, int, error) {
	var v uint64
	var shift uint
	for {
		if offset >= len(buf) {
			return 0, offset, io.ErrUnexpectedEOF
		}
		b := buf[offset]
		offset++
		v |= uint64(b&0x7F) << shift
		if b < 0x80 {
			break
		}
		shift += 7
	}
	return v, offset, nil
}

func decodeMVT(data []byte) ([]MVTLayer, error) {
	var layers []MVTLayer
	offset := 0
	for offset < len(data) {
		tag, off, err := readVarint(data, offset)
		if err != nil {
			return nil, err
		}
		offset = off
		wireType := int(tag & 7)
		fieldNum := int(tag >> 3)

		switch wireType {
		case 0:
			_, off, err = readVarint(data, offset)
			if err != nil {
				return nil, err
			}
			offset = off
		case 1:
			offset += 8
		case 2:
			length, off, err := readVarint(data, offset)
			if err != nil || int(length) < 0 || off+int(length) > len(data) {
				return nil, fmt.Errorf("invalid layer length")
			}
			offset = off

			if fieldNum == 3 { // Layer
				layer, err := decodeLayer(data[offset : offset+int(length)])
				if err != nil {
					return nil, err
				}
				layers = append(layers, layer)
			}
			offset += int(length)
		case 5:
			offset += 4
		default:
			return nil, fmt.Errorf("unknown wire type %d", wireType)
		}
	}
	return layers, nil
}

func decodeLayer(data []byte) (MVTLayer, error) {
	layer := MVTLayer{Extent: 4096}
	var keys []string
	var values []interface{}
	var featuresData [][]byte

	offset := 0
	for offset < len(data) {
		tag, off, err := readVarint(data, offset)
		if err != nil {
			return layer, err
		}
		offset = off
		wireType := int(tag & 7)
		fieldNum := int(tag >> 3)

		if wireType == 0 {
			v, off, err := readVarint(data, offset)
			if err != nil {
				return layer, err
			}
			offset = off
			if fieldNum == 5 { // extent
				layer.Extent = uint32(v)
			}
		} else if wireType == 1 {
			offset += 8
		} else if wireType == 5 {
			offset += 4
		} else if wireType == 2 {
			length, off, err := readVarint(data, offset)
			if err != nil || int(length) < 0 || off+int(length) > len(data) {
				return layer, fmt.Errorf("invalid field length in layer")
			}
			offset = off

			buf := data[offset : offset+int(length)]
			offset += int(length)

			if fieldNum == 1 { // name
				layer.Name = string(buf)
			} else if fieldNum == 2 { // feature
				featuresData = append(featuresData, buf)
			} else if fieldNum == 3 { // key
				keys = append(keys, string(buf))
			} else if fieldNum == 4 { // value
				values = append(values, decodeValue(buf))
			}
		}
	}

	for _, fd := range featuresData {
		feat, err := decodeFeature(fd, keys, values)
		if err != nil {
			return layer, err
		}
		layer.Features = append(layer.Features, feat)
	}

	return layer, nil
}

func decodeValue(data []byte) interface{} {
	offset := 0
	for offset < len(data) {
		tag, off, err := readVarint(data, offset)
		if err != nil {
			return nil
		}
		offset = off
		wireType := int(tag & 7)
		fieldNum := int(tag >> 3)

		if wireType == 0 {
			v, off, err := readVarint(data, offset)
			if err != nil {
				return nil
			}
			offset = off
			if fieldNum == 4 {
				return int64(v)
			} else if fieldNum == 5 {
				return v
			} else if fieldNum == 6 {
				return int64((v >> 1) ^ uint64((int64(v&1)<<63)>>63))
			} else if fieldNum == 7 {
				return v != 0
			}
		} else if wireType == 1 {
			if offset+8 > len(data) {
				return nil
			}
			v := binary.LittleEndian.Uint64(data[offset:])
			offset += 8
			if fieldNum == 3 {
				return math.Float64frombits(v)
			}
		} else if wireType == 5 {
			if offset+4 > len(data) {
				return nil
			}
			v := binary.LittleEndian.Uint32(data[offset:])
			offset += 4
			if fieldNum == 2 {
				return math.Float32frombits(v)
			}
		} else if wireType == 2 {
			length, off, err := readVarint(data, offset)
			if err != nil || int(length) < 0 || offset+int(length) > len(data) {
				return nil
			}
			offset = off
			buf := data[offset : offset+int(length)]
			offset += int(length)
			if fieldNum == 1 {
				return string(buf)
			}
		} else {
			return nil
		}
	}
	return nil
}

func decodeFeature(data []byte, keys []string, values []interface{}) (MVTFeature, error) {
	feat := MVTFeature{Properties: make(map[string]interface{})}
	var tags []uint32
	var geomData []uint32

	offset := 0
	for offset < len(data) {
		tag, off, err := readVarint(data, offset)
		if err != nil {
			return feat, err
		}
		offset = off
		wireType := int(tag & 7)
		fieldNum := int(tag >> 3)

		if wireType == 0 {
			v, off, err := readVarint(data, offset)
			if err != nil {
				return feat, err
			}
			offset = off
			if fieldNum == 1 { // id
				feat.ID = v
			} else if fieldNum == 3 { // type
				feat.Type = int(v)
			}
		} else if wireType == 2 {
			length, off, err := readVarint(data, offset)
			if err != nil || int(length) < 0 || off+int(length) > len(data) {
				return feat, fmt.Errorf("invalid length in feature")
			}
			offset = off
			buf := data[offset : offset+int(length)]
			offset += int(length)

			if fieldNum == 2 { // tags
				o := 0
				for o < len(buf) {
					v, nextO, err := readVarint(buf, o)
					if err != nil {
						break
					}
					tags = append(tags, uint32(v))
					o = nextO
				}
			} else if fieldNum == 4 { // geometry
				o := 0
				for o < len(buf) {
					v, nextO, err := readVarint(buf, o)
					if err != nil {
						break
					}
					geomData = append(geomData, uint32(v))
					o = nextO
				}
			}
		} else if wireType == 1 {
			offset += 8
		} else if wireType == 5 {
			offset += 4
		}
	}

	for i := 0; i+1 < len(tags); i += 2 {
		kIdx := int(tags[i])
		vIdx := int(tags[i+1])
		if kIdx < len(keys) && vIdx < len(values) {
			k := keys[kIdx]
			v := values[vIdx]
			feat.Properties[k] = v
		}
	}

	g, err := buildGeometry(feat.Type, geomData)
	if err == nil && !g.IsEmpty() {
		feat.Geometry = g
	}

	return feat, nil
}

func decodeZigZag(v uint32) int32 {
	return int32((v >> 1) ^ uint32((int32(v&1)<<31)>>31))
}

func flattenXY(pts []geom.XY) []float64 {
	res := make([]float64, 0, len(pts)*2)
	for _, p := range pts {
		res = append(res, p.X, p.Y)
	}
	return res
}

func isCW(ring []geom.XY) bool {
	var area float64
	for i := 0; i < len(ring)-1; i++ {
		area += ring[i].X*ring[i+1].Y - ring[i+1].X*ring[i].Y
	}
	// In MVT (Y-down) screen coordinates, area > 0 is CW.
	return area > 0
}

func buildGeometry(geomType int, geomData []uint32) (geom.Geometry, error) {
	var cx, cy int32
	var pts []geom.XY
	var rings [][]geom.XY
	var lines [][]geom.XY

	i := 0
	for i < len(geomData) {
		cmdInteger := geomData[i]
		i++
		cmd := cmdInteger & 7
		count := int(cmdInteger >> 3)

		switch cmd {
		case 1: // MoveTo
			if geomType == 3 && len(pts) > 0 {
				rings = append(rings, pts)
				pts = nil
			} else if geomType == 2 && len(pts) > 0 {
				lines = append(lines, pts)
				pts = nil
			}
			for j := 0; j < count; j++ {
				if i+1 >= len(geomData) {
					break
				}
				cx += decodeZigZag(geomData[i])
				i++
				cy += decodeZigZag(geomData[i])
				i++
				pts = append(pts, geom.XY{X: float64(cx), Y: float64(cy)})
			}
		case 2: // LineTo
			for j := 0; j < count; j++ {
				if i+1 >= len(geomData) {
					break
				}
				cx += decodeZigZag(geomData[i])
				i++
				cy += decodeZigZag(geomData[i])
				i++
				pts = append(pts, geom.XY{X: float64(cx), Y: float64(cy)})
			}
		case 7: // ClosePath
			if len(pts) > 0 {
				if pts[0] != pts[len(pts)-1] {
					pts = append(pts, pts[0])
				}
				rings = append(rings, pts)
				pts = nil
			}
		default:
			return geom.Geometry{}, fmt.Errorf("unknown MVT command %d", cmd)
		}
	}

	if len(pts) > 0 {
		if geomType == 2 {
			lines = append(lines, pts)
		}
	}

	switch geomType {
	case 1: // Point
		if len(pts) == 1 {
			return pts[0].AsPoint().AsGeometry(), nil
		} else if len(pts) > 1 {
			var mpts []geom.Point
			for _, p := range pts {
				mpts = append(mpts, p.AsPoint())
			}
			return geom.NewMultiPoint(mpts).AsGeometry(), nil
		}
	case 2: // LineString
		if len(lines) == 1 {
			if len(lines[0]) < 2 {
				return geom.Geometry{}, fmt.Errorf("LineString has < 2 points")
			}
			return geom.NewLineString(geom.NewSequence(flattenXY(lines[0]), geom.DimXY)).AsGeometry(), nil
		} else if len(lines) > 1 {
			var mls []geom.LineString
			for _, l := range lines {
				if len(l) >= 2 {
					mls = append(mls, geom.NewLineString(geom.NewSequence(flattenXY(l), geom.DimXY)))
				}
			}
			if len(mls) > 0 {
				return geom.NewMultiLineString(mls).AsGeometry(), nil
			}
		}
	case 3: // Polygon
		var polys []geom.Polygon
		var currentOuter []geom.LineString

		for _, r := range rings {
			if len(r) < 4 {
				continue
			}
			seq := geom.NewSequence(flattenXY(r), geom.DimXY)
			ls := geom.NewLineString(seq)

			if isCW(r) { // Outer ring in MVT
				if len(currentOuter) > 0 {
					polys = append(polys, geom.NewPolygon(currentOuter))
				}
				currentOuter = []geom.LineString{ls}
			} else { // Inner ring in MVT
				if len(currentOuter) == 0 {
					currentOuter = []geom.LineString{ls}
				} else {
					currentOuter = append(currentOuter, ls)
				}
			}
		}
		if len(currentOuter) > 0 {
			polys = append(polys, geom.NewPolygon(currentOuter))
		}

		if len(polys) == 1 {
			return polys[0].AsGeometry(), nil
		} else if len(polys) > 1 {
			return geom.NewMultiPolygon(polys).AsGeometry(), nil
		}
	}

	return geom.Geometry{}, fmt.Errorf("empty or invalid geometry")
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
		Layers  []StyleLayer `json:"layers"`
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

		logWidth := int(float64(m.Width) * float64(CellWidth) / float64(DevicePixelRatio))
		logHeight := int(float64(m.Height-1) * float64(CellHeight) / float64(DevicePixelRatio))
		physWidth := m.Width * CellWidth
		physHeight := (m.Height - 1) * CellHeight

		debugf("Starting map render: %dx%d logical (%dx%d physical), zoom %d, lat: %.4f, lng: %.4f", logWidth, logHeight, physWidth, physHeight, m.Zoom, m.Lat, m.Lng)

		wm := carto.NewWebMercator(m.Zoom)

		centerXY := wm.Forward(geom.XY{X: m.Lng, Y: m.Lat})
		globalPxX := centerXY.X * float64(TileSize)
		globalPxY := centerXY.Y * float64(TileSize)
		debugf("Center XY: (%.4f, %.4f) -> globalPx: (%.2f, %.2f)", centerXY.X, centerXY.Y, globalPxX, globalPxY)

		minPxX := globalPxX - float64(logWidth)/2
		minPxY := globalPxY - float64(logHeight)/2
		maxPxX := globalPxX + float64(logWidth)/2
		maxPxY := globalPxY + float64(logHeight)/2

		minTileX := int(math.Floor(minPxX / float64(TileSize)))
		minTileY := int(math.Floor(minPxY / float64(TileSize)))
		maxTileX := int(math.Floor(maxPxX / float64(TileSize)))
		maxTileY := int(math.Floor(maxPxY / float64(TileSize)))

		debugf("Pixel bounds: X[%.2f-%.2f], Y[%.2f-%.2f]", minPxX, maxPxX, minPxY, maxPxY)

		dc := gg.NewContext(physWidth, physHeight)
		dpr := float64(DevicePixelRatio)
		dc.Scale(dpr, dpr)

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

				collections, err := decodeMVT(tileData)
				if err != nil {
					debugf("Failed to decode MVT tile data: %v", err)
					continue
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

					var mvtLayer *MVTLayer
					for i, l := range collections {
						if l.Name == layerStyle.SourceLayer {
							mvtLayer = &collections[i]
							break
						}
					}
					if mvtLayer == nil {
						continue
					}

					scale := float64(TileSize) / float64(mvtLayer.Extent)
					renderedCount := 0
					filteredCount := 0

					for _, feature := range mvtLayer.Features {
						if !evaluateFilter(layerStyle.Filter, feature.Properties, feature.Geometry) {
							filteredCount++
							continue
						}

						drawFeature(dc, feature.Geometry, offsetX, offsetY, scale, dpr, &layerStyle, float64(m.Zoom))
						renderedCount++
					}

					if renderedCount > 0 {
						debugf("  -> Layer '%s' (source: %s): rendered %d features, filtered %d", layerStyle.ID, layerStyle.SourceLayer, renderedCount, filteredCount)
					}
				}
			}
		}

		// Optional: Draw Marker
		if m.MarkerLat != nil && m.MarkerLng != nil {
			markerXY := wm.Forward(geom.XY{X: *m.MarkerLng, Y: *m.MarkerLat})
			mx := markerXY.X*float64(TileSize) - minPxX
			my := markerXY.Y*float64(TileSize) - minPxY

			debugf("Drawing marker at screen px: (%.2f, %.2f)", mx, my)

			dpr := float64(DevicePixelRatio)

			dc.SetColor(color.RGBA{255, 0, 0, 255})
			dc.DrawCircle(mx, my, 6)
			dc.Fill()
			dc.SetColor(color.RGBA{255, 255, 255, 255})
			dc.DrawCircle(mx, my, 6)
			dc.SetLineWidth(2 * dpr)
			dc.Stroke()
		}

		img := dc.Image().(*image.RGBA)
		seq := encodeKittyGraphics(img, m.Width, m.Height-1)

		debugln("Render complete, sending mapRenderedMsg")
		return mapRenderedMsg{img: img, seq: seq}
	}
}

func drawFeature(dc *gg.Context, geometry geom.Geometry, offsetX, offsetY, scale, dpr float64, style *StyleLayer, zoom float64) {
	if geometry.IsEmpty() {
		return
	}
	if geometry.IsPolygon() {
		poly := geometry.MustAsPolygon()
		drawPolygon(dc, poly, offsetX, offsetY, scale, dpr, style, zoom)
	} else if geometry.IsMultiPolygon() {
		mp := geometry.MustAsMultiPolygon()
		for i := 0; i < mp.NumPolygons(); i++ {
			drawPolygon(dc, mp.PolygonN(i), offsetX, offsetY, scale, dpr, style, zoom)
		}
	} else if geometry.IsLineString() {
		ls := geometry.MustAsLineString()
		drawLineString(dc, ls, offsetX, offsetY, scale, dpr, style, zoom)
	} else if geometry.IsMultiLineString() {
		mls := geometry.MustAsMultiLineString()
		for i := 0; i < mls.NumLineStrings(); i++ {
			drawLineString(dc, mls.LineStringN(i), offsetX, offsetY, scale, dpr, style, zoom)
		}
	} else if geometry.IsPoint() {
		pt := geometry.MustAsPoint()
		drawPoint(dc, pt, offsetX, offsetY, scale, dpr, style, zoom)
	} else if geometry.IsMultiPoint() {
		mp := geometry.MustAsMultiPoint()
		for i := 0; i < mp.NumPoints(); i++ {
			drawPoint(dc, mp.PointN(i), offsetX, offsetY, scale, dpr, style, zoom)
		}
	}
}

func drawPolygon(dc *gg.Context, poly geom.Polygon, offsetX, offsetY, scale, dpr float64, style *StyleLayer, zoom float64) {
	c := resolvePaintValue(style.Paint.FillColor, zoom)
	if c == nil {
		return
	}
	dc.SetColor(parseColor(c))

	rings := poly.DumpRings()
	for _, ring := range rings {
		seq := ring.Coordinates()
		for i := 0; i < seq.Length(); i++ {
			xy := seq.GetXY(i)
			x := offsetX + xy.X*scale
			y := offsetY + xy.Y*scale
			if i == 0 {
				dc.MoveTo(x, y)
			} else {
				dc.LineTo(x, y)
			}
		}
		dc.ClosePath()
	}
	dc.Fill()
}

func drawLineString(dc *gg.Context, ls geom.LineString, offsetX, offsetY, scale, dpr float64, style *StyleLayer, zoom float64) {
	c := resolvePaintValue(style.Paint.LineColor, zoom)
	if c == nil {
		return
	}
	dc.SetColor(parseColor(c))
	lw := resolveLineWidth(style.Paint.LineWidth, zoom)
	dc.SetLineWidth(lw * dpr)

	seq := ls.Coordinates()
	for i := 0; i < seq.Length(); i++ {
		xy := seq.GetXY(i)
		x := offsetX + xy.X*scale
		y := offsetY + xy.Y*scale
		if i == 0 {
			dc.MoveTo(x, y)
		} else {
			dc.LineTo(x, y)
		}
	}
	dc.Stroke()
}

func drawPoint(dc *gg.Context, pt geom.Point, offsetX, offsetY, scale, dpr float64, style *StyleLayer, zoom float64) {
	c := resolvePaintValue(style.Paint.FillColor, zoom)
	if c == nil {
		return
	}
	dc.SetColor(parseColor(c))

	xy, ok := pt.XY()
	if !ok {
		return
	}
	x := offsetX + xy.X*scale
	y := offsetY + xy.Y*scale
	dc.DrawCircle(x, y, 3)
	dc.Fill()
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

	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	if len(bodyBytes) >= 2 && bodyBytes[0] == 0x1F && bodyBytes[1] == 0x8B {
		gz, err := gzip.NewReader(bytes.NewReader(bodyBytes))
		if err == nil {
			uncompressed, err := io.ReadAll(gz)
			gz.Close()
			if err == nil {
				return uncompressed, nil
			}
		}
	}

	return bodyBytes, nil
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

func evaluateFilter(filter []interface{}, props map[string]interface{}, geometry geom.Geometry) bool {
	if len(filter) == 0 {
		return true
	}
	result := evalFilterExpr(filter, props, geometry)
	if b, ok := result.(bool); ok {
		return b
	}
	return true
}

func geometryTypeName(g geom.Geometry) string {
	if g.IsPoint() {
		return "Point"
	}
	if g.IsMultiPoint() {
		return "MultiPoint"
	}
	if g.IsLineString() {
		return "LineString"
	}
	if g.IsMultiLineString() {
		return "MultiLineString"
	}
	if g.IsPolygon() {
		return "Polygon"
	}
	if g.IsMultiPolygon() {
		return "MultiPolygon"
	}
	if g.IsGeometryCollection() {
		return "GeometryCollection"
	}
	return ""
}

func evalFilterExpr(expr interface{}, props map[string]interface{}, geometry geom.Geometry) interface{} {
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
					return geometryTypeName(geometry)
				}
				if v, has := props[key]; has {
					return v
				}
			}
		}
		return nil

	case "geometry-type":
		return geometryTypeName(geometry)

	case "match":
		return evalMatch(arr, props, geometry)

	case "coalesce":
		for _, v := range arr[1:] {
			r := evalFilterExpr(v, props, geometry)
			if r != nil {
				return r
			}
		}
		return nil

	case "==":
		if len(arr) == 3 {
			lhs := evalFilterExpr(arr[1], props, geometry)
			rhs := evalFilterExpr(arr[2], props, geometry)
			return fmt.Sprintf("%v", lhs) == fmt.Sprintf("%v", rhs)
		}
		return true

	case "!=":
		if len(arr) == 3 {
			lhs := evalFilterExpr(arr[1], props, geometry)
			rhs := evalFilterExpr(arr[2], props, geometry)
			return fmt.Sprintf("%v", lhs) != fmt.Sprintf("%v", rhs)
		}
		return true

	case ">":
		if len(arr) == 3 {
			lf, ok1 := toFloat(evalFilterExpr(arr[1], props, geometry))
			rf, ok2 := toFloat(evalFilterExpr(arr[2], props, geometry))
			if ok1 && ok2 {
				return lf > rf
			}
		}
		return true

	case ">=":
		if len(arr) == 3 {
			lf, ok1 := toFloat(evalFilterExpr(arr[1], props, geometry))
			rf, ok2 := toFloat(evalFilterExpr(arr[2], props, geometry))
			if ok1 && ok2 {
				return lf >= rf
			}
		}
		return true

	case "<":
		if len(arr) == 3 {
			lf, ok1 := toFloat(evalFilterExpr(arr[1], props, geometry))
			rf, ok2 := toFloat(evalFilterExpr(arr[2], props, geometry))
			if ok1 && ok2 {
				return lf < rf
			}
		}
		return true

	case "<=":
		if len(arr) == 3 {
			lf, ok1 := toFloat(evalFilterExpr(arr[1], props, geometry))
			rf, ok2 := toFloat(evalFilterExpr(arr[2], props, geometry))
			if ok1 && ok2 {
				return lf <= rf
			}
		}
		return true

	case "in":
		if len(arr) >= 3 {
			lhs := fmt.Sprintf("%v", evalFilterExpr(arr[1], props, geometry))
			for _, v := range arr[2:] {
				if fmt.Sprintf("%v", evalFilterExpr(v, props, geometry)) == lhs {
					return true
				}
			}
			return false
		}

	case "!in":
		if len(arr) >= 3 {
			lhs := fmt.Sprintf("%v", evalFilterExpr(arr[1], props, geometry))
			for _, v := range arr[2:] {
				if fmt.Sprintf("%v", evalFilterExpr(v, props, geometry)) == lhs {
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
			r := evalFilterExpr(f, props, geometry)
			if b, ok := r.(bool); ok && !b {
				return false
			}
		}
		return true

	case "any":
		for _, f := range arr[1:] {
			r := evalFilterExpr(f, props, geometry)
			if b, ok := r.(bool); ok && b {
				return true
			}
		}
		return false

	case "!":
		if len(arr) == 2 {
			r := evalFilterExpr(arr[1], props, geometry)
			if b, ok := r.(bool); ok {
				return !b
			}
		}
	}

	return true
}

func evalMatch(arr []interface{}, props map[string]interface{}, geometry geom.Geometry) interface{} {
	if len(arr) < 4 {
		return arr[len(arr)-1]
	}
	input := evalFilterExpr(arr[1], props, geometry)
	inputStr := fmt.Sprintf("%v", input)

	for i := 2; i < len(arr)-1; i += 2 {
		labels := arr[i]
		output := arr[i+1]

		switch l := labels.(type) {
		case []interface{}:
			for _, label := range l {
				if fmt.Sprintf("%v", evalFilterExpr(label, props, geometry)) == inputStr {
					return output
				}
			}
		default:
			if fmt.Sprintf("%v", evalFilterExpr(labels, props, geometry)) == inputStr {
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

	model := NewMapModel(nycLat, nycLng, 14)

	model.MarkerLat = &nycLat
	model.MarkerLng = &nycLng

	p := tea.NewProgram(model)
	if _, err := p.Run(); err != nil {
		fmt.Printf("Bubbletea Error: %v\n", err)
	}
}

package lagdrift

// The simplified world map (lag.dev/DRIFTTEST.md G2, I5): Natural Earth's
// Admin 0 at 1:50m, simplified and quantized, as compact JSON for the SDK's
// display and the demo token's local verdict. See the package documentation
// for the layout.

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"strconv"
)

// The world map's format field.
const WorldFormat = "lagdrift-world/1"

var ErrInvalidWorld = errors.New("lagdrift: invalid world map")

// The world map as JSON.
type worldFile struct {
	Format     string         `json:"format"`
	Version    string         `json:"version"`
	SimplifyKm float64        `json:"simplifyKm"`
	Scale      float64        `json:"scale"`
	Countries  []worldCountry `json:"countries"`
}

type worldCountry struct {
	Code string `json:"code"`
	// each ring a flat list of longitude, latitude pairs, in 1 / scale degree
	Polygons [][][]float64 `json:"polygons"`
}

// Writes countries as the world map: sorted by code, one country to a line,
// each point rounded to 1 / scale degree. A ring's closing point, and a point
// equal to the one before it, are dropped; a ring left with fewer than 3
// points is dropped, and a polygon whose outline is dropped goes with it.
// simplifyKm records the tolerance the countries were simplified at.
func WriteWorld(w io.Writer, version string, simplifyKm float64, scale int, countries []Country) error {
	if scale <= 0 {
		return fmt.Errorf("%w: scale %d", ErrInvalidWorld, scale)
	}
	borders, err := NewBorders(version, countries)
	if err != nil {
		return err
	}
	header, err := json.Marshal(struct {
		Format     string  `json:"format"`
		Version    string  `json:"version"`
		SimplifyKm float64 `json:"simplifyKm"`
		Scale      int     `json:"scale"`
	}{
		Format:     WorldFormat,
		Version:    version,
		SimplifyKm: simplifyKm,
		Scale:      scale,
	})
	if err != nil {
		return err
	}
	out := append([]byte{}, header[:len(header)-1]...)
	out = append(out, `,"countries":[`...)
	first := true
	for _, country := range borders.countries {
		polygons := [][][][2]int64{}
		for _, polygon := range country.Polygons {
			rings := [][][2]int64{}
			for i, ring := range polygon {
				points, ok := scaleRing(ring, scale)
				if !ok {
					if i == 0 {
						break
					}
					continue
				}
				rings = append(rings, points)
			}
			if 0 < len(rings) {
				polygons = append(polygons, rings)
			}
		}
		if len(polygons) == 0 {
			continue
		}
		if !first {
			out = append(out, ',')
		}
		first = false
		out = append(out, "\n{\"code\":\""...)
		out = append(out, country.Code...)
		out = append(out, `","polygons":[`...)
		for i, polygon := range polygons {
			if 0 < i {
				out = append(out, ',')
			}
			out = append(out, '[')
			for j, ring := range polygon {
				if 0 < j {
					out = append(out, ',')
				}
				out = append(out, '[')
				for k, point := range ring {
					if 0 < k {
						out = append(out, ',')
					}
					out = strconv.AppendInt(out, point[1], 10)
					out = append(out, ',')
					out = strconv.AppendInt(out, point[0], 10)
				}
				out = append(out, ']')
			}
			out = append(out, ']')
		}
		out = append(out, "]}"...)
	}
	out = append(out, "\n]}\n"...)
	_, err = w.Write(out)
	return err
}

// A ring as latitude, longitude pairs in 1 / scale degree, without its closing
// point or repeated points; false when fewer than 3 remain or a point is
// invalid.
func scaleRing(ring []Point, scale int) ([][2]int64, bool) {
	s := float64(scale)
	points := make([][2]int64, 0, len(ring))
	for _, p := range ring {
		if !p.Valid() {
			return nil, false
		}
		q := [2]int64{int64(math.Round(p.Lat * s)), int64(math.Round(p.Lon * s))}
		if 0 < len(points) && points[len(points)-1] == q {
			continue
		}
		points = append(points, q)
	}
	for 1 < len(points) && points[len(points)-1] == points[0] {
		points = points[:len(points)-1]
	}
	if len(points) < minRingPoints {
		return nil, false
	}
	return points, true
}

// Reads the world map into borders. Its points are the integers divided by
// the scale.
func ReadWorld(r io.Reader) (*Borders, error) {
	var file worldFile
	decoder := json.NewDecoder(r)
	if err := decoder.Decode(&file); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidWorld, err)
	}
	if file.Format != WorldFormat {
		return nil, fmt.Errorf("%w: format %q, not %q", ErrInvalidWorld, file.Format, WorldFormat)
	}
	if !(0 < file.Scale) || math.IsInf(file.Scale, 1) {
		return nil, fmt.Errorf("%w: scale %v", ErrInvalidWorld, file.Scale)
	}
	countries := make([]Country, 0, len(file.Countries))
	for _, wc := range file.Countries {
		country := Country{Code: wc.Code, Polygons: make([][][]Point, 0, len(wc.Polygons))}
		for _, wp := range wc.Polygons {
			if len(wp) == 0 {
				return nil, fmt.Errorf("%w: a polygon of %s has no rings", ErrInvalidWorld, wc.Code)
			}
			polygon := make([][]Point, 0, len(wp))
			for _, flat := range wp {
				if len(flat)%2 != 0 || len(flat) < 2*minRingPoints {
					return nil, fmt.Errorf("%w: a ring of %s has %d numbers", ErrInvalidWorld, wc.Code, len(flat))
				}
				ring := make([]Point, 0, len(flat)/2)
				for i := 0; i < len(flat); i += 2 {
					p := Point{Lat: flat[i+1] / file.Scale, Lon: flat[i] / file.Scale}
					if !p.Valid() {
						return nil, fmt.Errorf("%w: a point of %s is out of range", ErrInvalidWorld, wc.Code)
					}
					ring = append(ring, p)
				}
				polygon = append(polygon, ring)
			}
			country.Polygons = append(country.Polygons, polygon)
		}
		countries = append(countries, country)
	}
	return NewBorders(file.Version, countries)
}

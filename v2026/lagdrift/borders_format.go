package lagdrift

// The borders file (lag.dev/DRIFTTEST.md G1, I5): Natural Earth's Admin 0 at
// 1:10m, keyed by GeoLite2's country codes, for the api's verdicts. See the
// package documentation for the layout.

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"math"
	"slices"
)

const (
	// the file's first bytes
	BordersMagic = "lagdrift-borders"
	// the layout WriteBorders writes and ReadBorders reads
	BordersFormatVersion = 1

	microdegreesPerDegree = 1000000
	maxBordersVersionLen  = 1024
	// a ring has at least this many points
	minRingPoints = 3
)

var ErrInvalidBorders = errors.New("lagdrift: invalid borders file")

// Writes countries in the borders format, as microdegrees. Countries are
// written sorted by code, and a code given more than once is merged. Each
// point is rounded to the nearest microdegree; a ring's closing point, and a
// point equal to the one before it, are dropped. A ring left with fewer than
// 3 points is dropped, and a polygon whose outline is dropped goes with it.
func WriteBorders(w io.Writer, version string, countries []Country) error {
	if maxBordersVersionLen < len(version) {
		return fmt.Errorf("%w: the version is longer than %d bytes", ErrInvalidBorders, maxBordersVersionLen)
	}
	borders, err := NewBorders(version, countries)
	if err != nil {
		return err
	}

	out := []byte{}
	out = append(out, BordersMagic...)
	out = binary.AppendUvarint(out, BordersFormatVersion)
	out = binary.AppendUvarint(out, uint64(len(version)))
	out = append(out, version...)

	type quantized struct {
		code     string
		box      [4]int64
		polygons [][][][2]int64
	}
	written := []quantized{}
	for _, country := range borders.countries {
		q := quantized{code: country.Code}
		for _, polygon := range country.Polygons {
			rings := [][][2]int64{}
			for i, ring := range polygon {
				points, ok := quantizeRing(ring)
				if !ok {
					if i == 0 {
						break
					}
					continue
				}
				rings = append(rings, points)
			}
			if 0 < len(rings) {
				q.polygons = append(q.polygons, rings)
			}
		}
		if len(q.polygons) == 0 {
			continue
		}
		q.box = [4]int64{math.MaxInt64, math.MaxInt64, math.MinInt64, math.MinInt64}
		for _, polygon := range q.polygons {
			for _, ring := range polygon {
				for _, point := range ring {
					q.box[0] = min(q.box[0], point[0])
					q.box[1] = min(q.box[1], point[1])
					q.box[2] = max(q.box[2], point[0])
					q.box[3] = max(q.box[3], point[1])
				}
			}
		}
		written = append(written, q)
	}

	out = binary.AppendUvarint(out, uint64(len(written)))
	for _, q := range written {
		out = append(out, q.code...)
		for _, v := range q.box {
			out = binary.AppendVarint(out, v)
		}
		out = binary.AppendUvarint(out, uint64(len(q.polygons)))
		for _, polygon := range q.polygons {
			out = binary.AppendUvarint(out, uint64(len(polygon)))
			for _, ring := range polygon {
				out = binary.AppendUvarint(out, uint64(len(ring)))
				var lat, lon int64
				for _, point := range ring {
					out = binary.AppendVarint(out, point[0]-lat)
					out = binary.AppendVarint(out, point[1]-lon)
					lat, lon = point[0], point[1]
				}
			}
		}
	}
	out = binary.LittleEndian.AppendUint32(out, crc32.ChecksumIEEE(out))
	_, err = w.Write(out)
	return err
}

// A ring as microdegrees, without its closing point or repeated points; false
// when fewer than 3 remain or a point is invalid.
func quantizeRing(ring []Point) ([][2]int64, bool) {
	points := make([][2]int64, 0, len(ring))
	for _, p := range ring {
		if !p.Valid() {
			return nil, false
		}
		q := [2]int64{
			int64(math.Round(p.Lat * microdegreesPerDegree)),
			int64(math.Round(p.Lon * microdegreesPerDegree)),
		}
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

// Reads a borders file. Every count, code, coordinate and box is checked,
// and the checksum, so a truncated or damaged file is an error rather than
// wrong borders.
func ReadBorders(r io.Reader) (*Borders, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}
	invalid := func(format string, a ...any) error {
		return fmt.Errorf("%w: %s", ErrInvalidBorders, fmt.Sprintf(format, a...))
	}
	if len(data) < len(BordersMagic)+4 || !bytes.Equal(data[:len(BordersMagic)], []byte(BordersMagic)) {
		return nil, invalid("not a borders file")
	}
	body := data[:len(data)-4]
	if binary.LittleEndian.Uint32(data[len(data)-4:]) != crc32.ChecksumIEEE(body) {
		return nil, invalid("the checksum does not match")
	}
	reader := &bordersReader{data: body, offset: len(BordersMagic)}

	formatVersion, err := reader.uvarint()
	if err != nil {
		return nil, err
	}
	if formatVersion != BordersFormatVersion {
		return nil, invalid("format version %d, not %d", formatVersion, BordersFormatVersion)
	}
	versionLen, err := reader.count(maxBordersVersionLen)
	if err != nil {
		return nil, err
	}
	versionBytes, err := reader.bytes(versionLen)
	if err != nil {
		return nil, err
	}
	countryCount, err := reader.count(reader.remaining())
	if err != nil {
		return nil, err
	}
	countries := make([]Country, 0, countryCount)
	for i := 0; i < countryCount; i++ {
		codeBytes, err := reader.bytes(2)
		if err != nil {
			return nil, err
		}
		code := string(codeBytes)
		if !validCode(code) {
			return nil, invalid("country code %q", code)
		}
		if 0 < len(countries) && code <= countries[len(countries)-1].Code {
			return nil, invalid("country %s is out of order", code)
		}
		var box [4]int64
		for j := range box {
			if box[j], err = reader.varint(); err != nil {
				return nil, err
			}
		}
		polygonCount, err := reader.count(reader.remaining())
		if err != nil {
			return nil, err
		}
		if polygonCount == 0 {
			return nil, invalid("country %s has no polygons", code)
		}
		seen := [4]int64{math.MaxInt64, math.MaxInt64, math.MinInt64, math.MinInt64}
		country := Country{Code: code, Polygons: make([][][]Point, 0, polygonCount)}
		for j := 0; j < polygonCount; j++ {
			ringCount, err := reader.count(reader.remaining())
			if err != nil {
				return nil, err
			}
			if ringCount == 0 {
				return nil, invalid("a polygon of %s has no rings", code)
			}
			polygon := make([][]Point, 0, ringCount)
			for k := 0; k < ringCount; k++ {
				// each point takes at least two bytes
				pointCount, err := reader.count(reader.remaining() / 2)
				if err != nil {
					return nil, err
				}
				if pointCount < minRingPoints {
					return nil, invalid("a ring of %s has %d points", code, pointCount)
				}
				ring := make([]Point, pointCount)
				var lat, lon int64
				for m := range ring {
					dLat, err := reader.varint()
					if err != nil {
						return nil, err
					}
					dLon, err := reader.varint()
					if err != nil {
						return nil, err
					}
					lat += dLat
					lon += dLon
					if lat < -90*microdegreesPerDegree || 90*microdegreesPerDegree < lat || lon < -180*microdegreesPerDegree || 180*microdegreesPerDegree < lon {
						return nil, invalid("a point of %s is out of range", code)
					}
					seen[0] = min(seen[0], lat)
					seen[1] = min(seen[1], lon)
					seen[2] = max(seen[2], lat)
					seen[3] = max(seen[3], lon)
					ring[m] = Point{
						Lat: float64(lat) / microdegreesPerDegree,
						Lon: float64(lon) / microdegreesPerDegree,
					}
				}
				polygon = append(polygon, ring)
			}
			country.Polygons = append(country.Polygons, polygon)
		}
		if seen != box {
			return nil, invalid("the box of %s does not match its points", code)
		}
		countries = append(countries, country)
	}
	if reader.offset != len(reader.data) {
		return nil, invalid("%d bytes follow the last country", len(reader.data)-reader.offset)
	}
	return NewBorders(string(versionBytes), countries)
}

type bordersReader struct {
	data   []byte
	offset int
}

func (self *bordersReader) remaining() int {
	return len(self.data) - self.offset
}

func (self *bordersReader) uvarint() (uint64, error) {
	v, n := binary.Uvarint(self.data[self.offset:])
	if n <= 0 {
		return 0, fmt.Errorf("%w: a number at byte %d", ErrInvalidBorders, self.offset)
	}
	self.offset += n
	return v, nil
}

func (self *bordersReader) varint() (int64, error) {
	v, n := binary.Varint(self.data[self.offset:])
	if n <= 0 {
		return 0, fmt.Errorf("%w: a number at byte %d", ErrInvalidBorders, self.offset)
	}
	self.offset += n
	return v, nil
}

// A count no larger than limit, so a damaged count can't allocate the earth.
func (self *bordersReader) count(limit int) (int, error) {
	v, err := self.uvarint()
	if err != nil {
		return 0, err
	}
	if uint64(max(limit, 0)) < v {
		return 0, fmt.Errorf("%w: a count of %d at byte %d", ErrInvalidBorders, v, self.offset)
	}
	return int(v), nil
}

func (self *bordersReader) bytes(n int) ([]byte, error) {
	if self.remaining() < n {
		return nil, fmt.Errorf("%w: the file ends early", ErrInvalidBorders)
	}
	b := self.data[self.offset : self.offset+n]
	self.offset += n
	return b, nil
}

// The countries of borders, for writing them again. The slices are the
// borders' own and must not be changed.
func (self *Borders) Countries() []Country {
	return slices.Clone(self.countries)
}

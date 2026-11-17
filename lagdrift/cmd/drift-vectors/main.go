// Command drift-vectors writes the drift model's parity vectors
// (lag.dev/DRIFTTEST.md V9, I4): inputs and lagdrift's outputs for every
// public function, which the SDK's TypeScript model must reproduce to 10⁻⁶.
//
//	go run ./lagdrift/cmd/drift-vectors -out <sdk>/js/test/lag/drift-vectors.json
//
// Every input is fixed: hand-chosen, or from arithmetic on a golden-ratio
// sequence, with no random generator, and rounded to 6 decimals. Outputs are
// rounded to 12 significant digits, so the file is the same on every machine
// although a last bit of a sine may differ. The TypeScript reads the rounded
// inputs, and the outputs are computed from those.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"math"
	"os"
	"strconv"

	"github.com/urnetwork/connect/lagdrift"
)

func main() {
	out := flag.String("out", "", "write the vectors here, else to stdout")
	flag.Parse()
	vectors := build()
	data, err := json.MarshalIndent(vectors, "", " ")
	if err != nil {
		fmt.Fprintf(os.Stderr, "drift-vectors: %v\n", err)
		os.Exit(1)
	}
	data = append(data, '\n')
	if *out == "" {
		os.Stdout.Write(data)
		return
	}
	if err := os.WriteFile(*out, data, 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "drift-vectors: %v\n", err)
		os.Exit(1)
	}
}

// The vectors file.
type vectorsFile struct {
	Format             string               `json:"format"`
	Generator          string               `json:"generator"`
	Tolerance          float64              `json:"tolerance"`
	Constants          constants            `json:"constants"`
	Geometry           []geometryVector     `json:"geometry"`
	MoveKm             []moveVector         `json:"moveKm"`
	HeldPlaceErrorKm   []heldPlaceVector    `json:"heldPlaceErrorKm"`
	EstimateRoundTrip  []roundTripVector    `json:"estimateRoundTrip"`
	KForConfidence     []kVector            `json:"kForConfidence"`
	CovEllipse         []ellipseVector      `json:"covEllipse"`
	FitPosition        []fitVector          `json:"fitPosition"`
	RefitPosition      []fitVector          `json:"refitPosition"`
	Judge              []judgeVector        `json:"judge"`
	PhysicallyPossible []physicalVector     `json:"physicallyPossible"`
	EvaluateStop       []stopVector         `json:"evaluateStop"`
	CountryAt          []countryAtVector    `json:"countryAt"`
	Decide             []decideVector       `json:"decide"`
	Borders            []bordersCodesVector `json:"borders"`
}

type constants struct {
	SpeedKmPerMs         float64                `json:"speedKmPerMs"`
	VacuumKmPerMs        float64                `json:"vacuumKmPerMs"`
	RoundTripQuantile    float64                `json:"roundTripQuantile"`
	DefaultConfidence    float64                `json:"defaultConfidence"`
	EarlyK               float64                `json:"earlyK"`
	EarthRadiusKm        float64                `json:"earthRadiusKm"`
	MsPerKm              float64                `json:"msPerKm"`
	HuberC               float64                `json:"huberC"`
	MaxStepKm            float64                `json:"maxStepKm"`
	MinFitExtenders      int                    `json:"minFitExtenders"`
	MinRoundTripSamples  int                    `json:"minRoundTripSamples"`
	MinPlaceErrorKm      float64                `json:"minPlaceErrorKm"`
	MaxPlaceErrorKm      float64                `json:"maxPlaceErrorKm"`
	DerivedPlaceErrorKm  float64                `json:"derivedPlaceErrorKm"`
	DriftCountryWithinKm float64                `json:"driftCountryWithinKm"`
	DefaultMaxSigmaKm    float64                `json:"defaultMaxSigmaKm"`
	DefaultModel         lagdrift.Model         `json:"defaultModel"`
	StopEarliestMs       int64                  `json:"stopEarliestMs"`
	StopWindowMs         int64                  `json:"stopWindowMs"`
	StopInterruptedMs    int64                  `json:"stopInterruptedMs"`
	StopCapMs            int64                  `json:"stopCapMs"`
	StopExtenders        int                    `json:"stopExtenders"`
	StopRoundTrips       int                    `json:"stopRoundTrips"`
	BrowserStopTolerance lagdrift.StopTolerance `json:"browserStopTolerance"`
	ServerStopTolerance  lagdrift.StopTolerance `json:"serverStopTolerance"`
}

type geometryVector struct {
	A           lagdrift.Point `json:"a"`
	B           lagdrift.Point `json:"b"`
	DistanceKm  float64        `json:"distanceKm"`
	BearingDeg  float64        `json:"bearingDeg"`
	AzimuthalKm [2]float64     `json:"azimuthalKm"`
}

type moveVector struct {
	P       lagdrift.Point `json:"p"`
	EastKm  float64        `json:"eastKm"`
	NorthKm float64        `json:"northKm"`
	Result  lagdrift.Point `json:"result"`
}

type heldPlaceVector struct {
	PlaceErrorKm float64 `json:"placeErrorKm"`
	Result       float64 `json:"result"`
}

type roundTripVector struct {
	Name      string             `json:"name"`
	SamplesMs []float64          `json:"samplesMs"`
	Ok        bool               `json:"ok"`
	RoundTrip lagdrift.RoundTrip `json:"roundTrip"`
}

type kVector struct {
	Confidence float64 `json:"confidence"`
	K          float64 `json:"k"`
}

type ellipseVector struct {
	Cov        [2][2]float64 `json:"cov"`
	MajorKm    float64       `json:"majorKm"`
	MinorKm    float64       `json:"minorKm"`
	BearingDeg float64       `json:"bearingDeg"`
}

type fitVector struct {
	Name         string                 `json:"name"`
	Model        lagdrift.Model         `json:"model"`
	Expected     lagdrift.Point         `json:"expected"`
	Observations []lagdrift.Observation `json:"observations"`
	Warm         *lagdrift.Fit          `json:"warm"`
	Fit          *lagdrift.Fit          `json:"fit"`
	// the error's text, when there is no fit
	Error string `json:"error"`
}

type judgeVector struct {
	Name       string             `json:"name"`
	Fit        *lagdrift.Fit      `json:"fit"`
	Country    *lagdrift.Country  `json:"country"`
	Confidence float64            `json:"confidence"`
	Judgement  lagdrift.Judgement `json:"judgement"`
}

type physicalVector struct {
	Name         string                 `json:"name"`
	Observations []lagdrift.Observation `json:"observations"`
	Country      *lagdrift.Country      `json:"country"`
	Possible     bool                   `json:"possible"`
}

type stopVector struct {
	Name       string                 `json:"name"`
	History    []lagdrift.Snapshot    `json:"history"`
	OpenedMs   int64                  `json:"openedMs"`
	NowMs      int64                  `json:"nowMs"`
	LastPongMs int64                  `json:"lastPongMs"`
	Country    *lagdrift.Country      `json:"country"`
	Tolerance  lagdrift.StopTolerance `json:"tolerance"`
	Stop       bool                   `json:"stop"`
	Reason     lagdrift.StopReason    `json:"reason"`
	// StopHolds for each reason
	Holds map[lagdrift.StopReason]bool `json:"holds"`
}

type countryAtVector struct {
	Name      string         `json:"name"`
	Countries string         `json:"countries"`
	Point     lagdrift.Point `json:"point"`
	WithinKm  float64        `json:"withinKm"`
	Code      string         `json:"code"`
}

type decideVector struct {
	Name         string                 `json:"name"`
	Fit          *lagdrift.Fit          `json:"fit"`
	Country      *lagdrift.Country      `json:"country"`
	Observations []lagdrift.Observation `json:"observations"`
	Confidence   float64                `json:"confidence"`
	MaxSigmaKm   float64                `json:"maxSigmaKm"`
	Verdict      lagdrift.Verdict       `json:"verdict"`
	Reason       lagdrift.Reason        `json:"reason"`
	Judgement    lagdrift.Judgement     `json:"judgement"`
}

// A set of countries the countryAt vectors name.
type bordersCodesVector struct {
	Name      string             `json:"name"`
	Countries []lagdrift.Country `json:"countries"`
	// the codes, sorted, merged as NewBorders merges them
	Codes []string `json:"codes"`
}

// ---- fixed inputs

// A value rounded to 6 decimals.
func r6(x float64) float64 {
	return math.Round(x*1e6) / 1e6
}

func p6(lat float64, lon float64) lagdrift.Point {
	return lagdrift.Point{Lat: r6(lat), Lon: r6(lon)}
}

// An output rounded to 12 significant digits.
func r12(x float64) float64 {
	if x == 0 || math.IsNaN(x) || math.IsInf(x, 0) {
		return x
	}
	v, _ := strconv.ParseFloat(strconv.FormatFloat(x, 'g', 12, 64), 64)
	return v
}

// The i-th term of a golden-ratio sequence in [0, 1): spread evenly, and
// arithmetic alone, so every machine computes the same.
func golden(i int, offset float64) float64 {
	x := 0.6180339887498949*float64(i) + offset
	return x - math.Floor(x)
}

// A destination km away at a bearing, rounded to 6 decimals.
func destination(p lagdrift.Point, bearingDeg float64, km float64) lagdrift.Point {
	lat1 := p.Lat * math.Pi / 180
	lon1 := p.Lon * math.Pi / 180
	b := bearingDeg * math.Pi / 180
	a := km / lagdrift.EarthRadiusKm
	lat2 := math.Asin(math.Sin(lat1)*math.Cos(a) + math.Cos(lat1)*math.Sin(a)*math.Cos(b))
	lon2 := lon1 + math.Atan2(math.Sin(b)*math.Sin(a)*math.Cos(lat1), math.Cos(a)-math.Sin(lat1)*math.Sin(lat2))
	lon := math.Mod(math.Mod(lon2*180/math.Pi+180, 360)+360, 360) - 180
	return p6(lat2*180/math.Pi, lon)
}

func box(minLat float64, minLon float64, maxLat float64, maxLon float64) []lagdrift.Point {
	return []lagdrift.Point{
		{Lat: minLat, Lon: minLon},
		{Lat: minLat, Lon: maxLon},
		{Lat: maxLat, Lon: maxLon},
		{Lat: maxLat, Lon: minLon},
	}
}

// Extender cities around Chicago, Madrid and Girona (lag.dev's drift test
// mock-up), each a place and an accuracy radius.
type city struct {
	at      lagdrift.Point
	placeKm float64
}

var (
	chicago = p6(41.8781, -87.6298)
	madrid  = p6(40.4168, -3.7038)
	girona  = p6(41.9794, 2.8214)

	chicagoA = []city{
		{at: p6(43.0389, -87.9065), placeKm: 20}, {at: p6(39.7684, -86.1581), placeKm: 50},
		{at: p6(42.3314, -83.0458), placeKm: 10}, {at: p6(38.627, -90.1994), placeKm: 100},
		{at: p6(43.6532, -79.3832), placeKm: 20}, {at: p6(33.749, -84.388), placeKm: 50},
		{at: p6(32.7767, -96.797), placeKm: 20}, {at: p6(39.7392, -104.9903), placeKm: 200},
		{at: p6(47.6062, -122.3321), placeKm: 20}, {at: p6(34.0522, -118.2437), placeKm: 5},
		{at: p6(51.5074, -0.1278), placeKm: 20}, {at: p6(-23.5505, -46.6333), placeKm: 0},
	}
	chicagoB = []city{
		{at: p6(43.0731, -89.4012), placeKm: 20}, {at: p6(42.9634, -85.6681), placeKm: 20},
		{at: p6(38.2527, -85.7585), placeKm: 50}, {at: p6(44.9778, -93.265), placeKm: 20},
		{at: p6(45.5017, -73.5673), placeKm: 20}, {at: p6(25.7617, -80.1918), placeKm: 20},
		{at: p6(29.7604, -95.3698), placeKm: 20}, {at: p6(40.7608, -111.891), placeKm: 50},
		{at: p6(49.2827, -123.1207), placeKm: 20}, {at: p6(33.4484, -112.074), placeKm: 20},
		{at: p6(50.1109, 8.6821), placeKm: 20}, {at: p6(19.4326, -99.1332), placeKm: 20},
	}
	madridA = []city{
		{at: p6(41.3874, 2.1686), placeKm: 20}, {at: p6(39.4699, -0.3763), placeKm: 20},
		{at: p6(38.7223, -9.1393), placeKm: 20}, {at: p6(43.263, -2.935), placeKm: 50},
		{at: p6(48.8566, 2.3522), placeKm: 10}, {at: p6(45.4642, 9.19), placeKm: 20},
		{at: p6(51.5074, -0.1278), placeKm: 10}, {at: p6(33.5731, -7.5898), placeKm: 100},
		{at: p6(59.3293, 18.0686), placeKm: 20}, {at: p6(41.0082, 28.9784), placeKm: 50},
		{at: p6(40.7128, -74.006), placeKm: 10}, {at: p6(-23.5505, -46.6333), placeKm: 20},
	}
)

// A run's observations of a visitor at truth: each extender's 10th
// percentile is the last mile, the distance at v, a route stretch from the
// sequence (−0.1 to +0.35 of the distance), and some queueing; its error and
// lowest sample follow from the sequence too.
func runObservations(truth lagdrift.Point, cities []city, prefix string, run int, lastMileMs float64, offset float64) []lagdrift.Observation {
	obs := []lagdrift.Observation{}
	for i, c := range cities {
		d := lagdrift.DistanceKm(truth, c.at)
		stretch := -0.1 + 0.45*golden(i, offset)
		ms := lastMileMs + lagdrift.MsPerKm*d*(1+stretch) + 0.4*golden(i+17, offset)
		obs = append(obs, lagdrift.Observation{
			ExtenderId:   fmt.Sprintf("%s-%d", prefix, i),
			At:           c.at,
			PlaceErrorKm: c.placeKm,
			Run:          run,
			RoundTrip: lagdrift.RoundTrip{
				Ms:      r6(ms),
				SigmaMs: r6(0.05 + 0.6*golden(i+31, offset)),
				Count:   12 + i,
				MinMs:   r6(ms - 0.3*golden(i+47, offset)),
			},
		})
	}
	return obs
}

// ---- outputs

func roundPoint(p lagdrift.Point) lagdrift.Point {
	return lagdrift.Point{Lat: r12(p.Lat), Lon: r12(p.Lon)}
}

func roundFit(f *lagdrift.Fit) *lagdrift.Fit {
	if f == nil {
		return nil
	}
	g := *f
	g.At = roundPoint(f.At)
	g.EastKm = r12(f.EastKm)
	g.NorthKm = r12(f.NorthKm)
	g.LastMileMs = make([]float64, len(f.LastMileMs))
	for i, v := range f.LastMileMs {
		g.LastMileMs[i] = r12(v)
	}
	for i := range g.Cov {
		for j := range g.Cov[i] {
			g.Cov[i][j] = r12(f.Cov[i][j])
		}
	}
	g.Chi2PerDof = r12(f.Chi2PerDof)
	g.MajorKm = r12(f.MajorKm)
	g.MinorKm = r12(f.MinorKm)
	g.BearingDeg = r12(f.BearingDeg)
	return &g
}

func roundJudgement(j lagdrift.Judgement) lagdrift.Judgement {
	j.D = r12(j.D)
	j.DistanceKm = r12(j.DistanceKm)
	j.SignificanceKm = r12(j.SignificanceKm)
	j.MarginKm = r12(j.MarginKm)
	j.K = r12(j.K)
	return j
}

// A fit the region test and the stop rules read: a place, a covariance, its
// ellipse, and an offset from the expected place.
func covFit(at lagdrift.Point, cov [2][2]float64, eastKm float64, northKm float64) *lagdrift.Fit {
	major, minor, bearing := lagdrift.CovEllipse(cov)
	return &lagdrift.Fit{
		At:         at,
		EastKm:     eastKm,
		NorthKm:    northKm,
		LastMileMs: []float64{5},
		Cov:        cov,
		Chi2PerDof: 1,
		Extenders:  12,
		MajorKm:    r6(major),
		MinorKm:    r6(minor),
		BearingDeg: r6(bearing),
		Runs:       []int{1},
	}
}

func circle(sigmaKm float64) [2][2]float64 {
	return [2][2]float64{{sigmaKm * sigmaKm, 0}, {0, sigmaKm * sigmaKm}}
}

// A covariance with axes major and minor km, the major at a bearing, rounded.
func tilted(majorKm float64, minorKm float64, bearingDeg float64) [2][2]float64 {
	e := math.Sin(bearingDeg * math.Pi / 180)
	n := math.Cos(bearingDeg * math.Pi / 180)
	a := majorKm * majorKm
	b := minorKm * minorKm
	off := r6((a - b) * e * n)
	return [2][2]float64{{r6(a*e*e + b*n*n), off}, {off, r6(a*n*n + b*e*e)}}
}

var (
	square = &lagdrift.Country{Code: "zz", Polygons: [][][]lagdrift.Point{{box(0, 0, 2, 2)}}}
	u      = &lagdrift.Country{Code: "zu", Polygons: [][][]lagdrift.Point{{{
		{Lat: 0, Lon: 0}, {Lat: 0, Lon: 3}, {Lat: 3, Lon: 3}, {Lat: 3, Lon: 2},
		{Lat: 1, Lon: 2}, {Lat: 1, Lon: 1}, {Lat: 3, Lon: 1}, {Lat: 3, Lon: 0},
	}}}}
	islands = &lagdrift.Country{Code: "zi", Polygons: [][][]lagdrift.Point{
		{box(10, 10, 14, 14)},
		{box(0, 0.4, 0.02, 0.42)},
	}}
	holed       = &lagdrift.Country{Code: "zh", Polygons: [][][]lagdrift.Point{{box(0, 0, 10, 10), box(4, 4, 6, 6)}}}
	antimeridan = &lagdrift.Country{Code: "zf", Polygons: [][][]lagdrift.Point{
		{box(-1, 179, 1, 180)},
		{box(-1, -180, 1, -179)},
	}}
	antipodal = &lagdrift.Country{Code: "za", Polygons: [][][]lagdrift.Point{{box(-35, -62, -27, -55)}}}
	// closed along the pole, as Natural Earth draws Antarctica
	polar = &lagdrift.Country{Code: "zq", Polygons: [][][]lagdrift.Point{{{
		{Lat: -70, Lon: -180}, {Lat: -68, Lon: -90}, {Lat: -72, Lon: 0}, {Lat: -66, Lon: 90},
		{Lat: -70, Lon: 180}, {Lat: -90, Lon: 180}, {Lat: -90, Lon: -180},
	}}}}
	large  = &lagdrift.Country{Code: "zl", Polygons: [][][]lagdrift.Point{{box(-20, -20, 20, 20)}}}
	beyond = &lagdrift.Country{Code: "zb", Polygons: [][][]lagdrift.Point{{box(5, 5, 7, 7)}}}
)

func build() vectorsFile {
	v := vectorsFile{
		Format:    "lagdrift-vectors/1",
		Generator: "go run ./lagdrift/cmd/drift-vectors",
		Tolerance: 1e-6,
		Constants: constants{
			SpeedKmPerMs:         lagdrift.SpeedKmPerMs,
			VacuumKmPerMs:        lagdrift.VacuumKmPerMs,
			RoundTripQuantile:    lagdrift.RoundTripQuantile,
			DefaultConfidence:    lagdrift.DefaultConfidence,
			EarlyK:               lagdrift.EarlyK,
			EarthRadiusKm:        lagdrift.EarthRadiusKm,
			MsPerKm:              lagdrift.MsPerKm,
			HuberC:               lagdrift.HuberC,
			MaxStepKm:            lagdrift.MaxStepKm,
			MinFitExtenders:      lagdrift.MinFitExtenders,
			MinRoundTripSamples:  lagdrift.MinRoundTripSamples,
			MinPlaceErrorKm:      lagdrift.MinPlaceErrorKm,
			MaxPlaceErrorKm:      lagdrift.MaxPlaceErrorKm,
			DerivedPlaceErrorKm:  lagdrift.DerivedPlaceErrorKm,
			DriftCountryWithinKm: lagdrift.DriftCountryWithinKm,
			DefaultMaxSigmaKm:    lagdrift.DefaultMaxSigmaKm,
			DefaultModel:         lagdrift.DefaultModel(),
			StopEarliestMs:       lagdrift.StopEarliestMs,
			StopWindowMs:         lagdrift.StopWindowMs,
			StopInterruptedMs:    lagdrift.StopInterruptedMs,
			StopCapMs:            lagdrift.StopCapMs,
			StopExtenders:        lagdrift.StopExtenders,
			StopRoundTrips:       lagdrift.StopRoundTrips,
			BrowserStopTolerance: lagdrift.BrowserStopTolerance,
			ServerStopTolerance:  lagdrift.ServerStopTolerance,
		},
	}
	buildGeometry(&v)
	buildRoundTrips(&v)
	buildFits(&v)
	buildRefits(&v)
	buildJudge(&v)
	buildPhysical(&v)
	buildStops(&v)
	buildCountryAt(&v)
	buildDecide(&v)
	return v
}

func buildGeometry(v *vectorsFile) {
	pairs := [][2]lagdrift.Point{
		{chicago, madrid},
		{madrid, girona},
		{p6(0, 179.5), p6(0.5, -179.5)},
		{p6(89.5, 10), p6(89.5, -170)},
		{p6(-33.8688, 151.2093), p6(51.5074, -0.1278)},
		{p6(31.23, 121.47), p6(-31.23, -58.53)},
		{p6(10, 10), p6(10, 10)},
	}
	for _, pair := range pairs {
		e, n := lagdrift.AzimuthalKm(pair[0], pair[1])
		v.Geometry = append(v.Geometry, geometryVector{
			A:           pair[0],
			B:           pair[1],
			DistanceKm:  r12(lagdrift.DistanceKm(pair[0], pair[1])),
			BearingDeg:  r12(lagdrift.BearingDeg(pair[0], pair[1])),
			AzimuthalKm: [2]float64{r12(e), r12(n)},
		})
	}
	moves := []struct {
		p lagdrift.Point
		e float64
		n float64
	}{
		{p: chicago, e: 120.5, n: -33.25},
		{p: p6(0, 179.9), e: 50, n: 0},
		{p: p6(89.8, 0), e: 10, n: 50},
		{p: p6(-45, -170), e: -400, n: 400},
	}
	for _, m := range moves {
		v.MoveKm = append(v.MoveKm, moveVector{P: m.p, EastKm: m.e, NorthKm: m.n, Result: roundPoint(lagdrift.MoveKm(m.p, m.e, m.n))})
	}
	for _, km := range []float64{-5, 0, 3, 10, 42.5, 100, 250, math.Copysign(0, -1)} {
		v.HeldPlaceErrorKm = append(v.HeldPlaceErrorKm, heldPlaceVector{PlaceErrorKm: km, Result: lagdrift.HeldPlaceErrorKm(km)})
	}
	for _, c := range []float64{0.5, 0.9, 0.95, 0.99, 0.999} {
		v.KForConfidence = append(v.KForConfidence, kVector{Confidence: c, K: r12(lagdrift.KForConfidence(c))})
	}
	covs := [][2][2]float64{
		circle(30),
		{{400, 0}, {0, 100}},
		{{100, 0}, {0, 400}},
		{{250, 150}, {150, 250}},
		{{250, -150}, {-150, 250}},
		tilted(140, 35, 72),
		tilted(80, 79, 10),
	}
	for _, cov := range covs {
		major, minor, bearing := lagdrift.CovEllipse(cov)
		v.CovEllipse = append(v.CovEllipse, ellipseVector{Cov: cov, MajorKm: r12(major), MinorKm: r12(minor), BearingDeg: r12(bearing)})
	}
}

func buildRoundTrips(v *vectorsFile) {
	samples := map[string][]float64{}
	names := []string{}
	add := func(name string, xs []float64) {
		names = append(names, name)
		samples[name] = xs
	}
	add("three samples", []float64{10, 11, 12})
	add("four samples", []float64{12.5, 10.25, 11, 13})
	climb := []float64{}
	for i := 1; i <= 20; i++ {
		climb = append(climb, float64(i))
	}
	add("a climb: tau held at 8, the lower rank below the first", climb)
	alternating := []float64{}
	for i := 1; i <= 50; i++ {
		alternating = append(alternating, float64(i), float64(100+i))
	}
	add("alternating: tau 1", alternating)
	for k, n := range []int{12, 30, 48} {
		xs := []float64{}
		for i := 0; i < n; i++ {
			// queueing as a square of the sequence, and a spike now and then
			q := golden(i, 0.1*float64(k+1))
			x := 18 + 6*q*q
			if golden(i, 0.77) < 0.06 {
				x += 25
			}
			xs = append(xs, r6(x))
		}
		add(fmt.Sprintf("queued, %d pings", n), xs)
	}
	// a lost ping is a non-positive value here, since JSON has no NaN
	add("lost pings left out", []float64{20.5, -1, 21, 0, 22.25, 20.75})
	add("constant", []float64{7, 7, 7, 7, 7})
	for _, name := range names {
		xs := samples[name]
		rt, ok := lagdrift.EstimateRoundTrip(xs)
		v.EstimateRoundTrip = append(v.EstimateRoundTrip, roundTripVector{
			Name:      name,
			SamplesMs: xs,
			Ok:        ok,
			RoundTrip: lagdrift.RoundTrip{Ms: r12(rt.Ms), SigmaMs: r12(rt.SigmaMs), Count: rt.Count, MinMs: r12(rt.MinMs)},
		})
	}
}

func buildFits(v *vectorsFile) {
	m := lagdrift.DefaultModel()
	truth := p6(41.862, -87.842)
	add := func(name string, model lagdrift.Model, expected lagdrift.Point, obs []lagdrift.Observation, warm *lagdrift.Fit) *lagdrift.Fit {
		f, err := lagdrift.FitPosition(model, expected, obs, warm)
		vector := fitVector{Name: name, Model: model, Expected: expected, Observations: obs, Warm: roundFit(warm), Fit: roundFit(f)}
		if err != nil {
			vector.Error = err.Error()
		}
		v.FitPosition = append(v.FitPosition, vector)
		return f
	}
	single := runObservations(truth, chicagoA, "chi-a", 1, 7.5, 0.13)
	f1 := add("one run around Chicago", m, chicago, single, nil)

	detoured := cloneObservations(single)
	detoured[2].RoundTrip.Ms = r6(detoured[2].RoundTrip.Ms + 25)
	add("one run with a detoured path", m, chicago, detoured, nil)

	// two runs on distinct extenders, the second's access link slower
	pooled := append(cloneObservations(single), runObservations(truth, chicagoB, "chi-b", 2, 12.5, 0.41)...)
	add("two runs on distinct extenders", m, chicago, pooled, nil)

	// three runs, the second and third repeating some of the first's extenders
	repeated := cloneObservations(single)
	second := runObservations(truth, chicagoA[:8], "chi-a", 2, 9, 0.29)
	third := runObservations(truth, append(append([]city{}, chicagoA[:4]...), chicagoB[:6]...), "chi-r", 3, 6.5, 0.57)
	for i := range third[:4] {
		third[i].ExtenderId = fmt.Sprintf("chi-a-%d", i)
	}
	repeated = append(append(repeated, second...), third...)
	add("three runs repeating extenders", m, chicago, repeated, nil)

	// round trips a little short of the distance: b held at 0
	short := cloneObservations(single)
	for i := range short {
		short[i].RoundTrip.Ms = r6(lagdrift.MsPerKm*lagdrift.DistanceKm(truth, short[i].At) - 0.6)
		short[i].RoundTrip.MinMs = r6(short[i].RoundTrip.Ms - 0.1)
	}
	add("the offset at its bound", m, chicago, short, nil)

	// an address in Madrid, a visitor in Frankfurt
	frankfurt := p6(50.105, 8.66)
	add("an address 1,400 km from the visitor", m, madrid, runObservations(frankfurt, madridA, "mad-a", 1, 21, 0.71), nil)

	// a warm refit of the same run, and of the run a few pings on
	add("a warm refit", m, chicago, single, f1)
	later := cloneObservations(single)
	for i := range later {
		later[i].RoundTrip.Ms = r6(later[i].RoundTrip.Ms - 0.05*golden(i, 0.9))
	}
	add("a warm fit a few pings on", m, chicago, later, f1)
	// a warm fit without runs matches offsets by index
	bare := *f1
	bare.Runs = nil
	add("a warm fit without runs", m, chicago, later, &bare)

	add("a calibrated model", lagdrift.Model{Kappa: 0.18, Sigma0Ms: 0.7}, chicago, single, nil)
	add("two extenders", m, chicago, single[:2], nil)
	add("no valid expected place", m, lagdrift.Point{Lat: 91, Lon: 0}, single, nil)
}

// The frame's refits (V10): from the warm fit alone.
func buildRefits(v *vectorsFile) {
	m := lagdrift.DefaultModel()
	truth := p6(41.862, -87.842)
	add := func(name string, expected lagdrift.Point, obs []lagdrift.Observation, warm *lagdrift.Fit) {
		f, err := lagdrift.RefitPosition(m, expected, obs, warm)
		vector := fitVector{Name: name, Model: m, Expected: expected, Observations: obs, Warm: roundFit(warm), Fit: roundFit(f)}
		if err != nil {
			vector.Error = err.Error()
		}
		v.RefitPosition = append(v.RefitPosition, vector)
	}
	single := runObservations(truth, chicagoA, "chi-a", 1, 7.5, 0.13)
	full, err := lagdrift.FitPosition(m, chicago, single, nil)
	if err != nil {
		panic(err)
	}
	// the warm fit as the TypeScript reads it from these vectors
	warm := roundFit(full)
	add("the same round trips", chicago, single, warm)
	later := cloneObservations(single)
	for i := range later {
		later[i].RoundTrip.Ms = r6(later[i].RoundTrip.Ms - 0.05*golden(i, 0.9))
		later[i].RoundTrip.Count += 1
	}
	add("a tick later", chicago, later, warm)
	add("no warm fit", chicago, later, nil)
	// a warm fit 500 km off, as after the extenders changed
	far := *warm
	far.At = destination(warm.At, 210, 500)
	add("a warm fit 500 km off", chicago, later, &far)
	pooled := append(cloneObservations(single), runObservations(truth, chicagoA[:8], "chi-a", 2, 9, 0.29)...)
	pooledFull, err := lagdrift.FitPosition(m, chicago, pooled, nil)
	if err != nil {
		panic(err)
	}
	pooledLater := cloneObservations(pooled)
	for i := range pooledLater {
		pooledLater[i].RoundTrip.Ms = r6(pooledLater[i].RoundTrip.Ms + 0.04*(golden(i, 0.3)-0.5))
	}
	add("two runs, a tick later", chicago, pooledLater, roundFit(pooledFull))
	add("two extenders", chicago, single[:2], warm)
}

func cloneObservations(obs []lagdrift.Observation) []lagdrift.Observation {
	return append([]lagdrift.Observation{}, obs...)
}

func buildJudge(v *vectorsFile) {
	add := func(name string, f *lagdrift.Fit, c *lagdrift.Country, confidence float64) {
		v.Judge = append(v.Judge, judgeVector{Name: name, Fit: f, Country: c, Confidence: confidence, Judgement: roundJudgement(lagdrift.Judge(f, c, confidence))})
	}
	add("inside", covFit(p6(1, 1), circle(30), 0, 0), square, 0.99)
	add("overlapping", covFit(p6(1, -0.5), circle(30), 0, 0), square, 0.99)
	add("outside", covFit(p6(1, -3), circle(30), 0, 0), square, 0.99)
	add("outside at 95%", covFit(p6(1, -0.9), circle(30), 0, 0), square, 0.95)
	add("outside at 99.9%", covFit(p6(1, -0.9), circle(30), 0, 0), square, 0.999)
	add("a long ellipse along the border", covFit(p6(1, -1.5), tilted(300, 20, 0), 0, 0), square, 0.99)
	add("a long ellipse at the border", covFit(p6(1, -1.5), tilted(300, 20, 90), 0, 0), square, 0.99)
	for i := 0; i < 6; i++ {
		at := p6(-2+6*golden(i, 0.21), -2+6*golden(i, 0.63))
		cov := tilted(10+110*golden(i, 0.35), 5+40*golden(i, 0.85), 180*golden(i, 0.05))
		add(fmt.Sprintf("tilted %d", i), covFit(at, cov, 0, 0), square, 0.99)
	}
	add("a concave border's notch", covFit(p6(2, 1.5), circle(25), 0, 0), u, 0.99)
	add("a tight fit in the notch", covFit(p6(2, 1.5), circle(10), 0, 0), u, 0.99)
	add("an island inside the region", covFit(p6(0, 0), circle(30), 0, 0), islands, 0.99)
	add("a tight fit in a hole", covFit(p6(5, 5), circle(20), 0, 0), holed, 0.99)
	add("a loose fit in a hole", covFit(p6(5, 5), circle(60), 0, 0), holed, 0.99)
	add("east of the antimeridian", covFit(p6(0, -178.5), circle(25), 0, 0), antimeridan, 0.99)
	add("west of the antimeridian", covFit(p6(0, 178.5), circle(25), 0, 0), antimeridan, 0.99)
	add("inside across the antimeridian", covFit(p6(0.5, -179.5), circle(25), 0, 0), antimeridan, 0.99)
	// the cut along the antimeridian is no border (judge.go's isCut)
	add("inside beside the cut, west", covFit(p6(0, 179.9), circle(5), 0, 0), antimeridan, 0.99)
	add("inside beside the cut, east", covFit(p6(0, -179.9), circle(5), 0, 0), antimeridan, 0.99)
	add("the fit's antipode", covFit(p6(31.23, 121.47), circle(50), 0, 0), antipodal, 0.99)
	add("near the pole, inside", covFit(p6(-85, 30), circle(100), 0, 0), polar, 0.99)
	add("off the polar coast", covFit(p6(-62, 30), circle(100), 0, 0), polar, 0.99)
	add("off the polar coast, far", covFit(p6(-50, -120), circle(100), 0, 0), polar, 0.99)
	add("a closed ring", covFit(p6(1, -0.7), tilted(60, 25, 70), 0, 0), &lagdrift.Country{Code: "zc", Polygons: [][][]lagdrift.Point{{append(box(0, 0, 2, 2), lagdrift.Point{Lat: 0, Lon: 0})}}}, 0.99)
	add("on the boundary", covFit(p6(1, 0), circle(30), 0, 0), square, 0.99)
}

func buildPhysical(v *vectorsFile) {
	extenders := []lagdrift.Point{
		p6(1, 3), p6(3, 1), p6(-1, 1), p6(1, -1),
		p6(10, 10), p6(-10, 10), p6(10, -10), p6(-10, -10),
	}
	observation := func(id string, at lagdrift.Point, minMs float64) lagdrift.Observation {
		return lagdrift.Observation{
			ExtenderId:   id,
			At:           at,
			PlaceErrorKm: 10,
			RoundTrip:    lagdrift.RoundTrip{Ms: r6(minMs + 1), SigmaMs: 0.2, Count: 40, MinMs: r6(minMs)},
		}
	}
	honest := []lagdrift.Observation{}
	for i, at := range extenders {
		honest = append(honest, observation(fmt.Sprintf("e%d", i), at, 2+lagdrift.MsPerKm*lagdrift.DistanceKm(p6(1, 1), at)))
	}
	oneBad := cloneObservations(honest)
	oneBad[4] = observation("e4", p6(30, 30), 2)
	twoBad := cloneObservations(oneBad)
	twoBad[5] = observation("e5", p6(-30, 30), 2)
	near := []lagdrift.Observation{}
	for i, at := range []lagdrift.Point{p6(0.5, 0), p6(-0.5, 0), p6(0, 0.5), p6(0, -0.5)} {
		near = append(near, observation(fmt.Sprintf("n%d", i), at, 1))
	}
	slow := cloneObservations(honest)
	for i := range slow {
		slow[i].Run = 1
		slow[i].RoundTrip.Ms = r6(slow[i].RoundTrip.Ms + 200)
		slow[i].RoundTrip.MinMs = r6(slow[i].RoundTrip.MinMs + 200)
	}
	far := &lagdrift.Country{Code: "zx", Polygons: [][][]lagdrift.Point{{box(-30, 40, -28, 42)}}}
	cases := []struct {
		name string
		obs  []lagdrift.Observation
		c    *lagdrift.Country
	}{
		{name: "honest", obs: honest, c: square},
		{name: "a country out of reach", obs: honest, c: &lagdrift.Country{Code: "zx", Polygons: [][][]lagdrift.Point{{box(20, 20, 22, 22)}}}},
		{name: "one bad disc", obs: oneBad, c: square},
		{name: "two bad discs", obs: twoBad, c: square},
		{name: "discs inside a large country", obs: near, c: large},
		{name: "discs short of a country", obs: near, c: beyond},
		{name: "slow runs reach", obs: slow, c: far},
		{name: "the fast run decides", obs: append(cloneObservations(slow), honest...), c: far},
		{name: "no country", obs: honest, c: nil},
	}
	for _, c := range cases {
		v.PhysicallyPossible = append(v.PhysicallyPossible, physicalVector{Name: c.name, Observations: c.obs, Country: c.c, Possible: lagdrift.PhysicallyPossible(c.obs, c.c)})
	}
}

func buildStops(v *vectorsFile) {
	const opened = 1_000_000
	ready := func(n int) []int {
		counts := make([]int, 12)
		for i := range counts {
			counts[i] = 5
			if i < n {
				counts[i] = 12 + i
			}
		}
		return counts
	}
	history := func(until int64, fit func(atMs int64) *lagdrift.Fit, n int) []lagdrift.Snapshot {
		snapshots := []lagdrift.Snapshot{}
		for t := int64(opened + 1000); t <= until; t += 250 {
			snapshots = append(snapshots, lagdrift.Snapshot{AtMs: t, Fit: fit(t), RoundTrips: ready(n)})
		}
		return snapshots
	}
	steady := func(int64) *lagdrift.Fit { return covFit(p6(1, 0.5), circle(40), 20, 10) }
	far := func(int64) *lagdrift.Fit { return covFit(p6(1, -10), circle(30), 0, 0) }
	none := func(int64) *lagdrift.Fit { return nil }
	shrinking := func(now int64, ratio float64) func(int64) *lagdrift.Fit {
		return func(atMs int64) *lagdrift.Fit {
			f := steady(atMs)
			if atMs < now-1000 {
				f.MajorKm = r6(f.MajorKm * ratio)
			}
			return f
		}
	}
	moving := func(now int64) func(int64) *lagdrift.Fit {
		return func(atMs int64) *lagdrift.Fit {
			f := steady(atMs)
			if atMs < now-1000 {
				f.EastKm += 12
			}
			return f
		}
	}
	add := func(name string, h []lagdrift.Snapshot, now int64, lastPong int64, c *lagdrift.Country, tol lagdrift.StopTolerance) {
		reason, ok := lagdrift.EvaluateStop(h, opened, now, lastPong, c, tol)
		holds := map[lagdrift.StopReason]bool{}
		for _, r := range []lagdrift.StopReason{lagdrift.StopInterrupted, lagdrift.StopClear, lagdrift.StopConverged, lagdrift.StopCap} {
			holds[r] = lagdrift.StopHolds(r, h, opened, now, lastPong, c, tol)
		}
		v.EvaluateStop = append(v.EvaluateStop, stopVector{Name: name, History: h, OpenedMs: opened, NowMs: now, LastPongMs: lastPong, Country: c, Tolerance: tol, Stop: ok, Reason: reason, Holds: holds})
	}
	browser := lagdrift.BrowserStopTolerance
	server := lagdrift.ServerStopTolerance
	add("clear, but before 3 s", history(opened+3000, far, 12), opened+2999, opened+2999, square, browser)
	add("clear at 3 s", history(opened+3000, far, 12), opened+3000, opened+3000, square, browser)
	add("before the cap", history(opened+12000, none, 0), opened+11999, opened+11900, square, browser)
	add("the cap", history(opened+12000, none, 0), opened+12000, opened+11900, square, browser)
	add("3.999 s without a pong", history(opened+6000, none, 0), opened+4999, opened+1000, square, browser)
	add("4 s without a pong", history(opened+6000, none, 0), opened+5000, opened+1000, square, browser)
	add("no pong at all", nil, opened+4000, 0, nil, browser)
	now := int64(opened + 3500)
	add("steady", history(now, steady, 8), now, now, square, browser)
	add("steady with 7 extenders ready", history(now, steady, 7), now, now, square, browser)
	short := history(now, steady, 8)
	add("steady for 1.75 s", short[len(short)-8:], now, now, square, browser)
	gap := history(now, steady, 8)
	gap[len(gap)-3].Fit = nil
	add("a gap in the window", gap, now, now, square, browser)
	add("moving", history(now, moving(now), 8), now, now, square, browser)
	add("area ×1.10, the browser", history(now, shrinking(now, 1.10), 8), now, now, square, browser)
	add("area ×1.11, the browser", history(now, shrinking(now, 1.11), 8), now, now, square, browser)
	add("area ×1.15, the server", history(now, shrinking(now, 1.15), 8), now, now, square, server)
	add("area ×1.18, the server", history(now, shrinking(now, 1.18), 8), now, now, square, server)
	clearAt := int64(opened + 3000)
	one := func(f *lagdrift.Fit, n int) []lagdrift.Snapshot {
		return []lagdrift.Snapshot{{AtMs: clearAt, Fit: f, RoundTrips: ready(n)}}
	}
	add("clear: far outside", one(covFit(p6(1, -10), circle(30), 0, 0), 8), clearAt, clearAt, square, browser)
	add("clear: deep inside", one(covFit(p6(0, 0), circle(30), 0, 0), 8), clearAt, clearAt, large, browser)
	add("not clear: straddling", one(covFit(p6(1, -0.3), circle(30), 0, 0), 8), clearAt, clearAt, square, browser)
	add("not clear: inside near the edge", one(covFit(p6(1, 0.3), circle(30), 0, 0), 8), clearAt, clearAt, square, browser)
	add("not clear: too few ready", one(covFit(p6(1, -10), circle(30), 0, 0), 7), clearAt, clearAt, square, browser)
	add("not clear: no country", one(covFit(p6(1, -10), circle(30), 0, 0), 8), clearAt, clearAt, nil, browser)
	add("not clear: a country of no polygons", one(covFit(p6(1, -10), circle(30), 0, 0), 8), clearAt, clearAt, &lagdrift.Country{Code: "zn", Polygons: [][][]lagdrift.Point{}}, browser)
	end := int64(opened + 12000)
	add("interrupted before clear", history(end, far, 12), end, opened+5000, square, browser)
	add("clear before converged and the cap", history(end, far, 12), end, end, square, browser)
	add("converged before the cap", history(end, steady, 8), end, end, square, browser)
	future := append(history(now, steady, 8), lagdrift.Snapshot{AtMs: opened + 9000})
	add("a snapshot after now", future, now, now, square, browser)
}

func buildCountryAt(v *vectorsFile) {
	overlapping := []lagdrift.Country{
		*square,
		*polar,
		{Code: "zy", Polygons: [][][]lagdrift.Point{{box(10, 10, 14, 14), box(11, 11, 12, 12)}}},
		{Code: "zy", Polygons: [][][]lagdrift.Point{{box(-1, 179, 1, 180)}}},
		{Code: "aa", Polygons: [][][]lagdrift.Point{{box(1, 1, 3, 3)}}},
	}
	b, err := lagdrift.NewBorders("", overlapping)
	if err != nil {
		panic(err)
	}
	v.Borders = append(v.Borders, bordersCodesVector{Name: "synthetic", Countries: overlapping, Codes: b.Codes()})
	cases := []struct {
		name     string
		p        lagdrift.Point
		withinKm float64
	}{
		{name: "inside", p: p6(0.5, 0.5), withinKm: 300},
		{name: "in two countries: the first code", p: p6(1.5, 1.5), withinKm: 300},
		{name: "in a hole, near its edge", p: p6(11.5, 11.5), withinKm: 300},
		{name: "in a hole, too far", p: p6(11.5, 11.5), withinKm: 10},
		{name: "111 km away, within 300", p: p6(-1, 1), withinKm: 300},
		{name: "111 km away, within 100", p: p6(-1, 1), withinKm: 100},
		{name: "across the antimeridian", p: p6(0, -179.5), withinKm: 150},
		{name: "across the antimeridian, whose cut is no border", p: p6(0, -179.5), withinKm: 100},
		{name: "at sea", p: p6(40, -40), withinKm: 300},
		{name: "near the pole", p: p6(-89.5, 77), withinKm: 300},
		{name: "off the polar coast", p: p6(-66, 0), withinKm: 300},
		{name: "not a place", p: lagdrift.Point{Lat: 95, Lon: 0}, withinKm: 300},
	}
	for _, c := range cases {
		v.CountryAt = append(v.CountryAt, countryAtVector{Name: c.name, Countries: "synthetic", Point: c.p, WithinKm: c.withinKm, Code: b.CountryAt(c.p, c.withinKm)})
	}
}

func buildDecide(v *vectorsFile) {
	extenders := []lagdrift.Point{
		p6(1, 3), p6(3, 1), p6(-1, 1), p6(1, -1),
		p6(10, 10), p6(-10, 10), p6(10, -10), p6(-10, -10),
	}
	observation := func(id string, at lagdrift.Point, minMs float64) lagdrift.Observation {
		return lagdrift.Observation{
			ExtenderId:   id,
			At:           at,
			PlaceErrorKm: 10,
			RoundTrip:    lagdrift.RoundTrip{Ms: r6(minMs + 1), SigmaMs: 0.2, Count: 40, MinMs: r6(minMs)},
		}
	}
	honest := []lagdrift.Observation{}
	for i, at := range extenders {
		honest = append(honest, observation(fmt.Sprintf("e%d", i), at, 2+lagdrift.MsPerKm*lagdrift.DistanceKm(p6(1, 1), at)))
	}
	impossible := cloneObservations(honest)
	impossible[4] = observation("e4", p6(30, 30), 2)
	impossible[5] = observation("e5", p6(-30, 30), 2)
	cases := []struct {
		name string
		fit  *lagdrift.Fit
		c    *lagdrift.Country
		obs  []lagdrift.Observation
	}{
		{name: "no country", fit: covFit(p6(1, 1), circle(30), 0, 0), c: nil, obs: honest},
		// a country with no boundary is unknown too (review F-04)
		{name: "a country of no polygons", fit: covFit(p6(1, 1), circle(30), 0, 0), c: &lagdrift.Country{Code: "zn", Polygons: [][][]lagdrift.Point{}}, obs: honest},
		{name: "a country of empty rings", fit: covFit(p6(1, -3), circle(30), 0, 0), c: &lagdrift.Country{Code: "zn", Polygons: [][][]lagdrift.Point{{{}}}}, obs: honest},
		{name: "too wide", fit: covFit(p6(1, 1), circle(2000), 0, 0), c: square, obs: honest},
		{name: "inside", fit: covFit(p6(1, 1), circle(30), 0, 0), c: square, obs: honest},
		{name: "overlapping", fit: covFit(p6(1, -0.5), circle(30), 0, 0), c: square, obs: honest},
		{name: "outside", fit: covFit(p6(1, -3), circle(30), 0, 0), c: square, obs: honest},
		{name: "inconsistent", fit: covFit(p6(1, 1), circle(30), 0, 0), c: square, obs: impossible},
		{name: "outside and impossible", fit: covFit(p6(1, -3), circle(30), 0, 0), c: square, obs: impossible},
	}
	for _, c := range cases {
		verdict, reason, j := lagdrift.Decide(c.fit, c.c, c.obs, lagdrift.DefaultConfidence, lagdrift.DefaultMaxSigmaKm)
		v.Decide = append(v.Decide, decideVector{Name: c.name, Fit: c.fit, Country: c.c, Observations: c.obs, Confidence: lagdrift.DefaultConfidence, MaxSigmaKm: lagdrift.DefaultMaxSigmaKm, Verdict: verdict, Reason: reason, Judgement: roundJudgement(j)})
	}
}

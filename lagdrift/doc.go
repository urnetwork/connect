// Package lagdrift is lag.dev's drift model (lag.dev/DRIFTTEST.md V1–V7,
// V10, G1, G2): where a visitor's round trips to extenders at known places
// put them, how sure that is, and whether that agrees with the country their
// address says. It is the authoritative model (V9). The api decides drift
// test verdicts with it, and the SDK's TypeScript model
// (`@urnetwork/sdk/lag`, js/src/lag/drift) mirrors it for the radar and the
// demo token, pinned to it by vectors from cmd/drift-vectors (I4). It uses the
// standard library only, so both can import it.
//
// # The model
//
//   - V1 (EstimateRoundTrip): each extender's round trip is the 10th
//     percentile of the run's pings to it, with a distribution-free standard
//     error from order statistics at the effective sample size.
//   - V2, V3 (Model): y ≈ b + K · d(p, e) with K = 2 / 133.24 ms/km, and each
//     term's variance σ_meas² + σ₀² + (κ K d)² + (K s_ext)².
//   - V5, V7 (FitPosition): one position and one last-mile offset per run,
//     by generalized least squares whose covariance shares each extender's
//     route and place error across its runs, with Huber weights per extender.
//     The 1σ ellipse comes from the position's covariance (CovEllipse).
//     RefitPosition is the frame's cheaper refit on each tick (V10), from the
//     warm fit alone.
//   - V6, V11 (Judge, Decide, KForConfidence, DefaultK, KEarly): the visitor
//     is outside the IP country only when the whole uncertainty region is,
//     the region's k the larger of the textbook k and the continent's
//     calibrated k.
//   - V5 (PhysicallyPossible): the speed of light's limit, leaving one
//     extender out.
//   - V10 (EvaluateStop, StopHolds): when a run can stop.
//   - G1, G2 (Borders, ReadBorders, ReadWorld): country borders by GeoLite2's
//     codes, and the drift country (CountryAt).
//
// # Conventions
//
// Places are degrees (Point). Positions and covariances are east and north
// km: a fit's offset from the expected place is in the azimuthal equidistant
// projection centered on the expected place, and its covariance is in the
// plane at the fit. Bearings are degrees clockwise from north. Round trips are
// ms. Country codes are GeoLite2's, lower case.
//
// The TypeScript model evaluates every formula in this package's order, so
// the two agree to 10⁻⁶ (V9): sums run in the order of their inputs, squares
// are x*x, and the iterations stop by the same rules.
//
// # The borders file
//
// For verdicts: Natural Earth's Admin 0 at 1:10m (G1), written by
// cmd/drift-borders as ne-admin0-10m.bin and read by ReadBorders. The api
// reads it from its config with server.Config.ResourcePath("lag/ne-admin0-10m.bin"),
// resolved like mmdb/: config's all/lag/<version>/ne-admin0-10m.bin, or
// local/lag/ne-admin0-10m.bin. The name is stable; the Natural Earth release,
// and the revision of how claims are drawn, are in the file's version
// (Borders.Version, e.g. "ne-admin0-10m-v5.1.2-claims2"), which a verdict
// records as geometry_version. Integers are Go's varints
// (encoding/binary): uvarint, and zigzag varint for signed ones.
//
//	magic          16 bytes, "lagdrift-borders"
//	format         uvarint, 1
//	version        uvarint length, then that many bytes of UTF-8: the
//	               geometry's version, e.g. "ne-admin0-10m-v5.1.2-claims2",
//	               which a verdict records as geometry_version (P4)
//	countries      uvarint count, then each country, sorted by code:
//	  code         2 bytes, two lower-case ASCII letters
//	  box          4 varints: the lowest latitude, lowest longitude, highest
//	               latitude and highest longitude of its points, microdegrees
//	  polygons     uvarint count (at least 1), then each polygon:
//	    rings      uvarint count (at least 1), then each ring, the first the
//	               outline and the rest holes:
//	      points   uvarint count (at least 3), then each point as two
//	               varints, its latitude and longitude in microdegrees, less
//	               the previous point's in the ring (the first point's less 0)
//	checksum       4 bytes, little-endian: the CRC-32 (IEEE) of every byte
//	               before it
//
// Rings are closed implicitly: a ring's last point does not repeat its first.
// Edges are straight in degrees and never cross the antimeridian; a polygon
// that does is split there, as Natural Earth splits them. An edge along the
// antimeridian (both ends at longitude 180, or both at −180) or along a pole
// (both ends at latitude 90, or both at −90) is such a cut, not a border: the
// region test and CountryAt skip it. A point's degrees are its
// microdegrees / 1e6.
//
// # The world map
//
// For display and the demo token (G2): Natural Earth's Admin 0 at 1:50m,
// simplified by Douglas–Peucker at about 5 km and quantized, written by
// cmd/drift-borders as JSON (the SDK's js/src/lag/drift/world.generated.json)
// and read by ReadWorld:
//
//	{"format": "lagdrift-world/1",
//	 "version": "ne-admin0-50m-v5.1.2-claims2", the geometry's version
//	 "simplifyKm": 5,                      the simplification's tolerance, km
//	 "scale": 1000,                        units per degree
//	 "countries": [                        sorted by code, one to a line
//	   {"code": "ad",
//	    "polygons": [                      each polygon a list of rings, the
//	      [[lon, lat, lon, lat, …], …]     first the outline and the rest
//	    ]}, …]}                            holes
//
// Each ring is a flat list of integers, longitude then latitude for each
// point, in 1 / scale degree, at least 3 points, closed implicitly. A point's
// degrees are its integers / scale. A country too small to draw at 1:50m is
// taken from 1:10m, simplified the same way, so the map has every code the
// borders file has, and it carries the borders file's claims (below).
//
// # Codes
//
// Countries are keyed by GeoLite2's codes: Natural Earth's ISO_A2_EH, lower
// case. cmd/drift-borders maps the features whose ISO_A2_EH is -99 (Somaliland
// to so, Northern Cyprus to cy, the sovereign base areas to cy and gb, …, and
// France, Norway and Kosovo should a release lack them) and splits out the
// territories GeoLite2 codes apart that Natural Earth draws inside another
// feature (French Guiana, Guadeloupe, Martinique, Réunion and Mayotte from
// France; Svalbard and Jan Mayen and Bouvet Island from Norway; the
// Caribbean Netherlands; the Cocos and Christmas Islands; Tokelau).
//
// # Claims
//
// A claim counts only inside a piece of Natural Earth's disputed-areas layer
// (G3, the user's decisions of 2026-11-17), and there each piece belongs to
// every country with a claim to it, so that no claimant's visitors are
// rejected for standing in it; everywhere else the default layer, which draws
// de facto lines, alone decides. Each piece goes to its de facto owner (its
// admin and its sovereign: Gibraltar is gi and gb, the West Bank and Gaza ps
// and il), to the owner each of Natural Earth's points of view gives it, and
// to each claimant its note names: Crimea is ru and ua, Kashmir in and pk,
// Western Sahara ma and eh, the Golan il and sy, Kosovo xk and rs, the
// Falklands fk and ar, Halaib eg and sd, Abyei sd and ss, Essequibo gy and ve,
// Arunachal Pradesh in, cn and tw, the Spratlys every claimant, and so on. Two
// pieces stay their owner's alone, Taiwan's and Israel's own, which some views
// give to China and to Palestine, so Tel Aviv is il alone. Taiwan and China
// are kept apart both ways, since an IP database never labels one side with
// the other's code: Taiwan and Kinmen are tw alone, and nothing China holds
// goes to tw, so the Paracels are cn and vn and Beijing is cn alone.
//
// A piece a country's default polygons already hold isn't added to it again;
// one they don't is added whole. Countries therefore overlap, and a country's
// polygons can overlap each other. That changes no verdict, since a point
// inside any polygon is inside, and an edge inside the country is never the
// nearest to a fit outside it. It can be the nearest to a fit inside, near a
// claim's line within the claimant's own land, such as the line of control
// for pk, which only delays V10's clear stop there. cmd/drift-borders checks
// G3's list and every claim on both files it writes.
//
// # The commands
//
//	go run ./lagdrift/cmd/drift-borders -borders ne-admin0-10m.bin -world world.generated.json [-source dir]
//	go run ./lagdrift/cmd/drift-vectors -out drift-vectors.json
//
// drift-borders downloads Natural Earth's GeoJSON at the release it pins, or
// reads it from -source, and checks it against the pinned SHA-256. drift-vectors
// writes the TypeScript model's parity vectors.
//
// # Beyond lag.dev/DRIFTTEST.md I3
//
// The package extends I3's API, narrowing none of it: RoundTrip.MinMs for the
// physical check; Fit.Runs, the run numbers Fit.LastMileMs follows, and
// Fit.Observations; Judgement.K; StopHolds, for the server's check of a
// reported stop; Decide, V6's verdict over a fit; KEarlyScale, V11's 1.32;
// CovEllipse, HeldPlaceErrorKm, Country.Contains, DistanceKm, BearingDeg,
// MoveKm and AzimuthalKm; and NewBorders, Borders.Codes and
// Borders.Countries, WriteBorders, ReadWorld and WriteWorld. Judge and Decide
// take V11's k where they took a confidence, and StopTolerance carries the
// run's k_early, as I3 now says. FitPosition recomputes V3's variances at
// every pass rather than every iteration; fit.go says why.
package lagdrift

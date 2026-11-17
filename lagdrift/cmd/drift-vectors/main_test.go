package main

import (
	"bytes"
	"encoding/json"
	"testing"
)

// The vectors are the same every time they are built, and cover every public
// function.
func TestVectorsAreDeterministic(t *testing.T) {
	a, err := json.Marshal(build())
	if err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(build())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(a, b) {
		t.Fatal("two builds differ")
	}
	v := build()
	sections := map[string]int{
		"geometry":           len(v.Geometry),
		"moveKm":             len(v.MoveKm),
		"heldPlaceErrorKm":   len(v.HeldPlaceErrorKm),
		"estimateRoundTrip":  len(v.EstimateRoundTrip),
		"kForConfidence":     len(v.KForConfidence),
		"covEllipse":         len(v.CovEllipse),
		"fitPosition":        len(v.FitPosition),
		"judge":              len(v.Judge),
		"physicallyPossible": len(v.PhysicallyPossible),
		"evaluateStop":       len(v.EvaluateStop),
		"countryAt":          len(v.CountryAt),
		"decide":             len(v.Decide),
	}
	for name, n := range sections {
		if n == 0 {
			t.Errorf("no %s vectors", name)
		}
	}
	// the fits include a pooled one repeating an extender, and both outcomes
	repeats := false
	for _, f := range v.FitPosition {
		if f.Fit != nil && 3 <= len(f.Fit.Runs) && f.Fit.Extenders < f.Fit.Observations {
			repeats = true
		}
	}
	if !repeats {
		t.Error("no pooled fit repeats an extender")
	}
}

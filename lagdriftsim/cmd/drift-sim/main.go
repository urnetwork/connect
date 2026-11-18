// drift-sim runs the drift test's calibration study (lagdriftsim,
// lag.dev/DRIFTTEST.md T3) and writes its tables into REPORT.md, between the
// report's markers.
//
//	go run ./lagdriftsim/cmd/drift-sim                  # the full study
//	go run ./lagdriftsim/cmd/drift-sim -scale 0.1       # a quick one
//	go run ./lagdriftsim/cmd/drift-sim -parts main,"free b"
//
// The borders file is found under the config repository's lag/ directory, a
// sibling of connect, unless -borders names one; without one the study runs
// synthetic countries only.
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/urnetwork/connect/lagdriftsim"
)

func main() {
	seed := flag.Uint64("seed", 1, "the study's seed")
	scale := flag.Float64("scale", 1, "each scenario's visitors per region, times this")
	borders := flag.String("borders", "", "the borders file; empty finds ne-admin0-10m*.bin under ../config/{all,local}/lag")
	parts := flag.String("parts", "", "the parts to run, comma separated; empty runs every part")
	workers := flag.Int("workers", 0, "parallel visitors; 0 is GOMAXPROCS")
	report := flag.String("report", "lagdriftsim/REPORT.md", "the report whose generated section to replace")
	flag.Parse()

	bordersPath := *borders
	if bordersPath == "" {
		for _, dir := range []string{"../config/all/lag", "../config/local/lag"} {
			matches, _ := filepath.Glob(filepath.Join(dir, "ne-admin0-10m*.bin"))
			if 0 < len(matches) {
				bordersPath = matches[len(matches)-1]
				break
			}
		}
	}
	partNames := []string{}
	for _, name := range strings.Split(*parts, ",") {
		if name = strings.TrimSpace(name); name != "" {
			partNames = append(partNames, name)
		}
	}
	commandLine := "go run ./lagdriftsim/cmd/drift-sim"
	flag.Visit(func(f *flag.Flag) {
		if f.Name != "workers" {
			commandLine += fmt.Sprintf(" -%s=%q", f.Name, f.Value.String())
		}
	})

	generated, err := lagdriftsim.RunStudy(lagdriftsim.Options{
		Seed:        *seed,
		Scale:       *scale,
		BordersPath: bordersPath,
		Parts:       partNames,
		Workers:     *workers,
		CommandLine: commandLine,
		Log:         os.Stderr,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if err := lagdriftsim.WriteReport(*report, generated); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

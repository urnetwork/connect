# The drift test's error rates (lag.dev/DRIFTTEST.md T3)

A simulation study of the drift test against its authoritative model, `lagdrift`: how often an honest visitor is
rejected, how often a false claim is caught, how long runs take, and where to set V10's early-stop threshold k_early
(`lagdrift.EarlyK`). drift-sim writes this file; edit the program, not the file.

## Summary

- **k_early: 3.** It is the smallest of the sweep (3, 3.25, 3.5, 3.75, 4, 4.25, 4.5, 5) that keeps the false-reject rate within 1 − confidence at 95% and 99%, after 1, 2 and 3 runs, in every honest scenario where a run with no clear stop is within it, and adds no false reject where even that isn't. Each value is k_early at the textbook 99% k, scaled with the run's k as KEarly scales EarlyK (V11), so EarlyK's value is KEarly(k). The doc's 4 holds too.
  - honest visitors' runs: 1.41% stop clear at k_early 3 and 0.77% at 4; the median run is 6.50 s and 6.50 s.
  - claims' runs: 62.9% stop clear at k_early 3 and 50.9% at 4; the median run is 5.00 s and 5.25 s.
- **False rejects** at k_early 3, judged at V11's k for the address's continent, after 1, 2 and 3 runs:
  - tiny country, 20 km: 2.27%, 2.27%, 2.52% at 95%; 0.68%, 0.64%, 0.71% at 99%.
  - small country, 5 km in: 1.93%, 1.14%, 1.57% at 95%; 0.43%, 0.36%, 0.64% at 99%.
  - large country, 5 km in: 0.79%, 0.79%, 1.14% at 95%; 0.57%, 0.29%, 0.36% at 99%.
  - small country, center: 2.36%, 1.86%, 1.79% at 95%; 0.71%, 0.50%, 0.71% at 99%.
  - large country, 100 km in: 0.64%, 0.64%, 0.50% at 95%; 0.21%, 0.29%, 0.14% at 99%.
  - real country: 0.46%, 0.29%, 0.29% at 95%; 0.12%, 0.086%, 0.12% at 99%.
- **Calibration:** the 99% region at V11's k for the address's continent misses the truth in 0.75% (0.55–1.02) of single runs (nominal 1%), most in South America at 1.18%. A country's false-reject rate is at most its region's miss rate, which the tiny country reaches.
- **Power** at 99% after 1, 2 and 3 runs, all regions: claim 300 km: 7.17%, 18.9%, 29.0%; claim 1,000 km: 57.1%, 72.8%, 78.0%; claim 3,000 km: 68.9%, 84.6%, 89.3%; real claim 300 km: 11.8%, 28.0%, 40.6%; real claim 1,000 km: 52.1%, 70.8%, 77.2%; real claim 3,000 km: 62.8%, 83.6%, 87.0%; by region in the tables.
- **Stop times** of honest visitors' first runs at 99%: median 7.50 s, p90 8.75 s, p99 12.00 s; 1.13% at the 12 s cap.
- **Stop times** of claims' first runs at 99%: median 5.50 s, p90 8.00 s, p99 11.00 s; 0.60% at the 12 s cap.

## How the study works

**The model under test** is `github.com/urnetwork/connect/lagdrift`, called as the frame and the api call it:
- `EstimateRoundTrip` for each extender's 10th percentile (V1);
- `FitPosition` for the frame's fit after every ping round, pooled with the runs in the verdict (V9), and for the
  server's fit of the pooled runs (V5, V7);
- `Judge` and `PhysicallyPossible` for the verdict (V5, V6), and V10's rules for the stop.

The study varies k_early and V10's other constants, which are constants in `lagdrift`. So it evaluates V10's rules
itself, with lagdrift's region test, and decides the verdict at every confidence from one region test. Every seventh
visitor is checked against `lagdrift.Decide` at every verdict, and, in the part that runs the full multi-start at
every ping round, against `lagdrift.EvaluateStop` at every ping round (the checks table).

**Extenders.** lag.dev's e2e stack (`lag.dev/e2e/stack/dots.json`) has 12 real cities, all chosen around one visitor in
Austin: too few for E2's bands from anywhere else. So the fleet is a plausible global one, weighted toward Europe and
North America: 182 extenders in 111 datacenter cities (81 in Europe, 58 in North America, 21 in Asia, 2 in the Middle
East, 8 in South America, 6 in Africa, 6 in Oceania), with dots.json's 12 among them (`places.go`).
- **Places.** An extender's place is its city's point, as GeoLite2 gives a datacenter address. Its accuracy radius is
  10 km (30%), 20 (25%), 50 (25%), 100 (15%) or 200 km (5%), or the place is derived (10%, held at 50 km). The radius
  is about a 67% radius, so the true place is off by a normal draw with a per-axis σ of radius / 1.5. A radius of
  200 km, which the model holds to 100 km, is a place the model trusts too much.
- **Fleets.** Each of 16 fleets draws every extender's place error once, and visitors are spread over the fleets, so
  the rates average over place errors rather than resting on one draw.
- **Serving.** Each extender serves with a 90% chance per visitor (A4).

**Visitors** are in 150 cities (Europe 43, North America 29, Eastern Europe and the Middle East 17, Asia 20, South
America 15, Africa 18, Oceania 8; `places.go`): big cities inland and on coasts, cities on land borders (Strasbourg,
Basel, Geneva, Aachen, Detroit, El Paso, Tijuana, Kinshasa, Asunción, …) and in small countries (Luxembourg,
Singapore, Hong Kong). A visitor is within 15 km of their city's point, which is the address's place.
- **Honest visitors** are in their address's country: either a synthetic disk around them at a controlled distance
  from its edge, or their city's real country from Natural Earth's Admin 0 at 1:10m (borders: ne-admin0-10m-v5.1.2-claims3). The worst case is
  the tiny country, a 20 km disk with the visitor anywhere in it (Luxembourg, Andorra, Singapore): there a false reject
  is exactly the region missing the truth, the rate every other country's is below.
- **Claims** have an address that claims a country 300, 1,000 or 3,000 km from where they are, placed at that country's
  capital or main VPN city, and E2 chooses the extenders around that place. The claimed country is either a disk around
  a real city whose edge is exactly at the distance, or a real country whose nearest point is 0.8 to 1.25 times the
  distance away.

**Runs.** A visitor makes 3 runs in the main part, and the verdict after each pools the counting runs so far (V7).
- **Extenders (E2):** the 4 nearest, preferring 2 in the IP country; 4 between 300 and 2,500 km chosen by the
  information they give, by the model's variances, over the address's place and a ring of places 800 and 2,000 km
  around it; 4 between 2,500 and 9,000 km spread by bearing (the largest smallest gap); 4 spares. A short mid band's
  slots pass to the far band, and a short far band is filled from beyond it. A retest prefers extenders the earlier
  runs didn't use. The VPN parts compare this choice with E2's earlier bands and other variants.
- **Sessions:** each opens 60 ms + 3 round trips + up to 250 ms after the run starts, and 3 s × min(1, d / 7,500 km)
  later still, d the extender's distance, since far sessions open 2–3 s late on the real stack (e2e); one part
  compares sessions staggered by their round trips alone. 3% never open, and a spare replaces each at 4 s. The
  extender sends one ping at a time, 250 ms apart or at the last pong, and a ping with no pong in 5 s is lost and holds
  the next for 5 s (M4).
- **Round trips** are b_r + K · d(truth, the extender's true place) · (1 + s) + queue(t) + jitter + spikes, plus a TCP
  retransmission (200 ms or more) for a dropped packet (`network.go`):
  - **s, the route stretch,** is drawn once per path: mean 0 and standard deviation κ, the model's route error, but
    floored at −0.3, since no path is faster than light in fiber along the great circle (K's speed is 2/3 of fiber's),
    and so skewed, with a tail of detours: a Gamma shifted to the floor. κ is 0.25 unless a part says otherwise.
  - **The access link** is fiber (25%: b 2–6 ms), cable or DSL (25%: 6–20 ms), Wi-Fi (35%: 5–24 ms) or mobile (15%:
    20–50 ms), and b changes by up to ±15% between runs. Its queue is shared by every path: an Ornstein–Uhlenbeck
    process with a 0.8 s correlation time and a scale of 0.3 ms (fiber) to 5 ms (mobile), plus each ping's own jitter.
    Spike episodes (0.02 a second on fiber, 0.3 on Wi-Fi and mobile, about 0.3 s long, 3 to 150 ms high) delay every
    ping sent in them. Retransmissions are 0.1% (fiber) to 1.5% (mobile) of pings, 5 times as many in a spike.
  - **The run's link** drops for good at a random moment in 1% of runs, which ends them as `interrupted`.
- **The frame** fits every 250 ms over its usable extenders, those with 8 round trips (V10, E5), once 6 have them,
  pooled with the counting runs: a warm refit, and the full multi-start when its extenders change, every 2 s and before
  any stop, which is withdrawn when that fit lands in another basin (V10's cadence). From 3 s it stops converged or
  clear with 8 usable extenders, clear at the run's k_early.
- **The server** takes each extender with 8 round trips measured by the moment the sessions closed (E5), counts the
  run with 6 such (an interrupted run only if converged or clear held when the silence began, V10), pools the last 3
  counting runs (V7), and judges at the k for the run's confidence and the address's continent (V11): at 95% AF 2.50, OC 2.79, others 2.448; at
  99% NA 3.25, SA 3.50, AF 3.95, OC 7.88, AS 3.35, AN 3.49, unknown 3.49, others 3.035; k_early KEarly(k) = max(4, 1.3180 × k). The parts that diagnose the textbook region judge at its k
  (2.448, 3.035 and 3.717 at 95%, 99% and 99.9%, which V11 refuses) with k_early 4. Inconclusive for a σ_major
  above 1500 km or a region that touches the country while the physical check fails, else agree when the region
  touches the country, else disagree (V6).

**Parts.** Every part runs the contract as it stands unless it says otherwise: E2's choice, V10's cadence and stops on
the usable extenders, the interrupted rule, V11's k with KEarly(k), and far sessions opening late. The main part
sweeps k_early with 3 runs per visitor, each value scaled with the run's k as KEarly scales EarlyK, so EarlyK's value
is V11's KEarly(k). The others compare sessions staggered by their round trips alone, stopping early with not, the
cadence with full fits, κ in the world and the model, V10's other constants, other worlds (misplaced extenders, a
normal route stretch), V11's calibration, the stops' evidence, E2's variants against a VPN, and the interrupted rule;
and, at the textbook k, where the region's misses come from, the verdict's starts, and Oceania.

**Rates** are shown with their Wilson 95% intervals where it matters, so a difference can be told from noise. A
"worst cell" is the highest rate over the honest scenarios and the number of runs.

**Reproducing.** Every draw comes from a PCG generator keyed by the seed and by what it is for (`seed.go`), so a part
reproduces exactly, whatever the machine or the number of workers. This report came from:

```
go run ./lagdrift/cmd/drift-borders -borders /tmp/ne-admin0-10m.bin
go run ./lagdriftsim/cmd/drift-sim -borders="/Users/brien/urnetwork-drifttest/config/local/lag/ne-admin0-10m.bin"
```

Without `-borders`, drift-sim looks for the file in the config repository (`../config/{all,local}/lag/ne-admin0-10m*.bin`),
and without one it runs the synthetic countries only. `-scale 0.1` runs a tenth of the visitors, and `-parts` some of
the parts.

## Findings

**The k_early sweep.** Each value of the sweep against the rule:

- 3: holds.
- 3.25: holds.
- 3.5: holds.
- 3.75: holds.
- 4: holds.
- 4.25: holds.
- 4.5: holds.
- 5: holds.

With no clear stop at all, every honest cell is within 1 − confidence.

**Stopping early** (one run, at V11's k for the address's continent, k_early KEarly(k)): V10 rejects 0.43% of tiny-country visitors at 99%, against 0.29% for runs that all last to the cap; its median run is 7.50 s against 12 s, and it rejects 54.5% of 1,000 km claims against 54.8%.

**κ** (one run, 99% at V11's k for the address's continent, k_early KEarly(k)):

- κ 0.15 in the world, 0.15 in the model, one run: the region misses 0.78%, false rejects 0.67% (tiny) and 0.43% (large country's edge), power 19.2% at 300 km and 80.9% at 1,000 km.
- κ 0.15 in the world, 0.25 in the model, one run: the region misses 0.50%, false rejects 0.38% (tiny) and 0.14% (large country's edge), power 5.06% at 300 km and 57.6% at 1,000 km.
- κ 0.25 in the world, 0.15 in the model, one run: the region misses 1.99%, false rejects 1.52% (tiny) and 0.14% (large country's edge), power 14.8% at 300 km and 69.8% at 1,000 km.
- κ 0.25 in the world, 0.25 in the model, one run: the region misses 0.91%, false rejects 0.76% (tiny) and 0% (large country's edge), power 4.40% at 300 km and 55.0% at 1,000 km.
- κ 0.25 in the world, 0.35 in the model, one run: the region misses 0.31%, false rejects 0.29% (tiny) and 0% (large country's edge), power 1.63% at 300 km and 39.2% at 1,000 km.
- κ 0.35 in the world, 0.25 in the model, one run: the region misses 0.51%, false rejects 0.19% (tiny) and 0.14% (large country's edge), power 6.04% at 300 km and 52.6% at 1,000 km.
- κ 0.35 in the world, 0.35 in the model, one run: the region misses 0.21%, false rejects 0.095% (tiny) and 0% (large country's edge), power 1.79% at 300 km and 38.3% at 1,000 km.

**Oceania** (one run, 99% at the textbook k, k_early 4; V11's 99% k for Oceania is 7.88), each line taking one source of error away or trying one remedy:

- base (the study's world): the region misses 3.10% (2.12–4.50); false rejects 0.75% in real countries, 2.00% in the tiny one; power 2.29% at 300 km and 62.0% at 1,000 km.
- normal stretch (the route stretch normal and unfloored: no long tail of detours): the region misses 3.76% (2.66–5.29); false rejects 0.75% in real countries, 2.42% in the tiny one; power 2.29% at 300 km and 65.0% at 1,000 km.
- no long-path error (no route error on paths of 2,500 km or more): the region misses 2.88% (1.97–4.19); false rejects 0.50% in real countries, 1.67% in the tiny one; power 4.00% at 300 km and 68.0% at 1,000 km.
- no short-path error (no route error on paths under 2,500 km): the region misses 4.02% (2.89–5.57); false rejects 1.00% in real countries, 2.58% in the tiny one; power 2.29% at 300 km and 59.0% at 1,000 km.
- no place error (every extender exactly at its place): the region misses 3.58% (2.52–5.06); false rejects 0.75% in real countries, 2.33% in the tiny one; power 2.29% at 300 km and 66.0% at 1,000 km.
- ideal pings (no queue, jitter, spike or loss): the region misses 3.19% (2.20–4.60); false rejects 0.75% in real countries, 2.00% in the tiny one; power 1.71% at 300 km and 65.0% at 1,000 km.
- denser fleet (9 more extenders: Adelaide, Canberra, Hobart, Darwin, Wellington, Christchurch, Port Moresby, Suva, Nouméa): the region misses 0.35% (0.14–0.90); false rejects 0% in real countries, 0.25% in the tiny one; power 20.6% at 300 km and 96.0% at 1,000 km.
- model κ 0.30 (the model's κ 0.30 for Oceania (a κ calibrated per continent, V4)): the region misses 1.53% (0.88–2.66); false rejects 0.75% in real countries, 0.92% in the tiny one; power 0.57% at 300 km and 46.0% at 1,000 km.
- model κ 0.35 (the model's κ 0.35 for Oceania): the region misses 1.38% (0.75–2.52); false rejects 0.75% in real countries, 0.83% in the tiny one; power 0.57% at 300 km and 43.0% at 1,000 km.
- model κ 0.45 (the model's κ 0.45 for Oceania): the region misses 1.48% (0.78–2.78); false rejects 0.75% in real countries, 0.67% in the tiny one; power 0% at 300 km and 29.0% at 1,000 km.
- s_ext by distance (each extender's place error at least 5% of its distance from the address's place, within lagdrift's [10, 100] km hold): the region misses 3.11% (2.13–4.52); false rejects 0.75% in real countries, 2.00% in the tiny one; power 1.71% at 300 km and 62.0% at 1,000 km.

**V10's other constants** (one run, 99% at V11's k for the address's continent, k_early KEarly(k)):

- k_early 4: median run 7.50 s, p90 8.75 s; false rejects 0.57% (tiny); power 5.98% at 300 km.
- round trips 12: median run 8.50 s, p90 9.75 s; false rejects 0.51% (tiny); power 5.76% at 300 km.
- round trips 16: median run 9.50 s, p90 10.50 s; false rejects 0.51% (tiny); power 5.76% at 300 km.
- window 1000 ms: median run 5.25 s, p90 7.00 s; false rejects 0.46% (tiny); power 5.76% at 300 km.
- window 3000 ms: median run 8.75 s, p90 10.75 s; false rejects 0.51% (tiny); power 6.09% at 300 km.
- area ratio 1.05: median run 7.75 s, p90 9.50 s; false rejects 0.57% (tiny); power 5.87% at 300 km.
- area ratio 1.2: median run 7.25 s, p90 8.25 s; false rejects 0.51% (tiny); power 5.65% at 300 km.
- extenders 6: median run 7.50 s, p90 8.50 s; false rejects 0.57% (tiny); power 6.09% at 300 km.
- extenders 10: median run 7.50 s, p90 8.75 s; false rejects 0.51% (tiny); power 5.98% at 300 km.

**A visitor far from the address's place (VPN)**, at 99% at V11's k for the address's continent, k_early KEarly(k):

- dense: E2: rejected after 1/3 runs: Madrid/DE 10.0%/60.0%, Paris/NL 2.50%/65.0%, Warsaw/DE 0%/12.5%, Toronto/US 0%/0%, Mexico City/US 0%/30.0%, Singapore/JP 75.0%/100.0%, Lagos/GB 22.5%/95.0%; tiny-country false rejects 0.60%/0.83%.
- dense: joint spread: rejected after 1/3 runs: Madrid/DE 12.5%/50.0%, Paris/NL 2.50%/35.0%, Warsaw/DE 0%/32.5%, Toronto/US 0%/0%, Mexico City/US 0%/0%, Singapore/JP 70.0%/100.0%, Lagos/GB 25.0%/92.5%; tiny-country false rejects 0.48%/0.48%.
- dense: VPN hubs: rejected after 1/3 runs: Madrid/DE 0%/15.0%, Paris/NL 12.5%/20.0%, Warsaw/DE 0%/0%, Toronto/US 0%/0%, Mexico City/US 0%/30.0%, Singapore/JP 47.5%/75.0%, Lagos/GB 2.50%/60.0%; tiny-country false rejects 0.36%/1.43%.
- dense: information: rejected after 1/3 runs: Madrid/DE 2.50%/17.5%, Paris/NL 10.0%/45.0%, Warsaw/DE 2.50%/2.50%, Toronto/US 0%/0%, Mexico City/US 0%/0%, Singapore/JP 67.5%/97.5%, Lagos/GB 0%/17.5%; tiny-country false rejects 0.83%/0.48%.
- dense: information ring: rejected after 1/3 runs: Madrid/DE 77.5%/97.5%, Paris/NL 5.00%/65.0%, Warsaw/DE 27.5%/85.0%, Toronto/US 0%/0%, Mexico City/US 0%/0%, Singapore/JP 20.0%/100.0%, Lagos/GB 5.00%/20.0%; tiny-country false rejects 0.60%/0.95%.
- dense: ring mid band: rejected after 1/3 runs: Madrid/DE 37.5%/100.0%, Paris/NL 2.50%/55.0%, Warsaw/DE 0%/25.0%, Toronto/US 0%/0%, Mexico City/US 0%/30.0%, Singapore/JP 22.5%/100.0%, Lagos/GB 20.0%/92.5%; tiny-country false rejects 0.24%/0.95%.
- dense: hybrid: rejected after 1/3 runs: Madrid/DE 52.5%/100.0%, Paris/NL 2.50%/65.0%, Warsaw/DE 5.00%/77.5%, Toronto/US 0%/0%, Mexico City/US 0%/32.5%, Singapore/JP 75.0%/100.0%, Lagos/GB 25.0%/92.5%; tiny-country false rejects 0.48%/0.83%.
- sparse: E2: rejected after 1/3 runs: Madrid/DE 57.5%/85.0%, Paris/NL 30.0%/45.0%, Warsaw/DE 15.0%/50.0%, Toronto/US 0%/0%, Mexico City/US 12.5%/20.0%, Singapore/JP 95.0%/100.0%, Lagos/GB 5.00%/47.5%; tiny-country false rejects 1.79%/1.79%.
- sparse: joint spread: rejected after 1/3 runs: Madrid/DE 45.0%/80.0%, Paris/NL 22.5%/40.0%, Warsaw/DE 25.0%/57.5%, Toronto/US 0%/0%, Mexico City/US 2.50%/17.5%, Singapore/JP 97.5%/100.0%, Lagos/GB 20.0%/35.0%; tiny-country false rejects 1.43%/1.79%.
- sparse: VPN hubs: rejected after 1/3 runs: Madrid/DE 52.5%/70.0%, Paris/NL 17.5%/42.5%, Warsaw/DE 2.50%/42.5%, Toronto/US 0%/0%, Mexico City/US 12.5%/22.5%, Singapore/JP 92.5%/100.0%, Lagos/GB 2.50%/30.0%; tiny-country false rejects 1.55%/1.43%.
- sparse: information: rejected after 1/3 runs: Madrid/DE 17.5%/92.5%, Paris/NL 15.0%/32.5%, Warsaw/DE 15.0%/62.5%, Toronto/US 0%/0%, Mexico City/US 2.50%/22.5%, Singapore/JP 87.5%/100.0%, Lagos/GB 5.00%/95.0%; tiny-country false rejects 0.83%/1.07%.
- sparse: information ring: rejected after 1/3 runs: Madrid/DE 47.5%/95.0%, Paris/NL 22.5%/37.5%, Warsaw/DE 27.5%/52.5%, Toronto/US 0%/0%, Mexico City/US 0%/20.0%, Singapore/JP 82.5%/100.0%, Lagos/GB 10.0%/100.0%; tiny-country false rejects 1.07%/1.31%.
- sparse: ring mid band: rejected after 1/3 runs: Madrid/DE 67.5%/95.0%, Paris/NL 25.0%/42.5%, Warsaw/DE 15.0%/60.0%, Toronto/US 0%/0%, Mexico City/US 12.5%/20.0%, Singapore/JP 87.5%/100.0%, Lagos/GB 12.5%/97.5%; tiny-country false rejects 1.19%/0.95%.
- sparse: hybrid: rejected after 1/3 runs: Madrid/DE 65.0%/87.5%, Paris/NL 30.0%/45.0%, Warsaw/DE 22.5%/55.0%, Toronto/US 0%/0%, Mexico City/US 12.5%/20.0%, Singapore/JP 95.0%/100.0%, Lagos/GB 2.50%/47.5%; tiny-country false rejects 1.79%/1.79%.

**The interrupted stop** (one run, 99% at V11's k for the address's continent, k_early KEarly(k)):

- counted: the silent client is agreed with in 80.0% of 300 km claims and 41.7% of 1,000 km ones; an honest client's runs that don't count: 0.27%.
- review's rule: the silent client is agreed with in 0.16% of 300 km claims and 0.31% of 1,000 km ones; an honest client's runs that don't count: 0.37%.

**Where the region's misses come from** (tiny country, one run; nominal 5%, 1% and 0.1%, k 2.448, 3.035 and 3.717):

- base (the study's world): the region misses 3.04%, 2.05% and 1.10% at 95%, 99% and 99.9%; the k that would hold each is 2.04, 4.02 and 9.41.
- ideal pings (no queue, jitter, spike or loss): the region misses 3.04%, 1.94% and 1.10% at 95%, 99% and 99.9%; the k that would hold each is 2.05, 3.84 and 9.52.
- no place error (every extender exactly at its place): the region misses 3.14%, 2.04% and 1.05% at 95%, 99% and 99.9%; the k that would hold each is 2.02, 3.73 and 10.76.
- normal stretch (the route stretch normal and unfloored): the region misses 6.35%, 3.00% and 1.45% at 95%, 99% and 99.9%; the k that would hold each is 2.62, 4.26 and 10.36.
- model's world (all three: the route stretch normal, no place error, ideal pings): the region misses 5.95%, 2.95% and 1.50% at 95%, 99% and 99.9%; the k that would hold each is 2.61, 4.08 and 10.28.

**The verdict's starts** (one run): lagdrift's against the lowest χ² of a wider grid of starts:

- lagdrift: false rejects 1.62% (tiny) and 0.48% (real countries) at 99%; VPN pairs rejected 26.8%.
- wide: false rejects 1.52% (tiny) and 0.48% (real countries) at 99%; VPN pairs rejected 26.4%.

**The fit** (the checks table): 0% of 114,217 warm refits of a verdict's fit moved it 1 km or more, and 163 of 17,257 grids of starts found a fixed point with a χ² lower by more than 1. Of those, 31 lie nearer the truth than the fit. One at a time, by CPU time: the frame's full fit, one run, p50 0.40 ms and p95 1.21 ms; the frame's full fit, pooled runs, p50 1.05 ms and p95 3.00 ms; the server's fit, one run, p50 0.42 ms and p95 1.08 ms; the server's fit, pooled runs, p50 1.16 ms and p95 3.00 ms; the frame's warm refit, one run, p50 0.03 ms and p95 0.14 ms; the frame's warm refit, pooled runs, p50 0.09 ms and p95 0.39 ms; against V10's budget of a warm refit at p95 ≤ 1 ms and a full fit at p95 ≤ 10 ms.

## Tables

Generated 2026-11-18 in 53m3s: seed 1, scale 1, go1.27.4, borders ne-admin0-10m-v5.1.2-claims3, lagdrift's EarlyK 4.

### Parts

| Part | What it varies | Visitors | Policies |
|---|--:|--:|--:|
| main | the contract as it stands (V10, V11, E2's hybrid choice, far sessions opening late), κ 0.25 in the world and the model, 3 runs, the k_early sweep | 19,841 | 9 |
| staggered sessions | the main part's visitors with each session opening after its own round trips alone, as before the e2e found far sessions 2–3 s late; V10, 3 runs | 19,841 | 1 |
| no early stop | one run under V10, V10 without clear, and no early stop at all (the cap) | 3350 | 3 |
| cadence: refits | V10's cadence: a warm refit at each ping round, the full multi-start when the extenders change, every 2 s and before any stop | 4746 | 9 |
| cadence: full fits | the same visitors with the full multi-start at every ping round | 4746 | 9 |
| κ 0.15/0.15 | κ 0.15 in the world, 0.15 in the model, one run | 3665 | 1 |
| κ 0.15/0.25 | κ 0.15 in the world, 0.25 in the model, one run | 3665 | 1 |
| κ 0.25/0.15 | κ 0.25 in the world, 0.15 in the model, one run | 3665 | 1 |
| κ 0.25/0.25 | κ 0.25 in the world, 0.25 in the model, one run | 3665 | 1 |
| κ 0.25/0.35 | κ 0.25 in the world, 0.35 in the model, one run | 3665 | 1 |
| κ 0.35/0.25 | κ 0.35 in the world, 0.25 in the model, one run | 3665 | 1 |
| κ 0.35/0.35 | κ 0.35 in the world, 0.35 in the model, one run | 3665 | 1 |
| stop rule | V10's constants one at a time: round trips, the steadiness window, the area ratio, the extenders; one run | 4671 | 9 |
| misplaced | 3% of extenders misplaced by 300 to 1,500 km, beyond their accuracy radius; one run | 4671 | 1 |
| normal stretch | the route stretch normal, the model's own assumption, rather than skewed and floored; one run | 4671 | 1 |
| V11: lag.dev tracks | lag.dev's own choice of 8 extenders, one run to the cap, the fit measured from the visitor's city point: V11's calibration tracks | 70,000 | 1 |
| V11 drift: default k | the hybrid choice, 3 runs, judged at V11's default k for the address's continent, k_early KEarly(k) | 10,472 | 1 |
| V11 drift: lag k | the same visitors judged at the k calibrated from the lag.dev tracks by V11's upper-bound rule | 10,472 | 1 |
| V11 table | the hybrid choice, 3 runs, judged at V11's fixed 99% table for the address's continent, k_early KEarly(k) | 23,217 | 1 |
| late sessions: stops on 4 | far sessions 3 s × min(1, d / 7,500 km) late; the frame's stops on every extender with 4 round trips | 12,223 | 2 |
| late sessions: stops on usable | far sessions 3 s × min(1, d / 7,500 km) late; the frame's stops on the usable extenders only (V10) | 12,223 | 2 |
| calibration: base | the study's world | 2800 | 1 |
| calibration: ideal pings | no queue, jitter, spike or loss | 2800 | 1 |
| calibration: no place error | every extender exactly at its place | 2800 | 1 |
| calibration: normal stretch | the route stretch normal and unfloored | 2800 | 1 |
| calibration: model's world | all three: the route stretch normal, no place error, ideal pings | 2800 | 1 |
| starts: lagdrift | the server's verdict from lagdrift's fit, one run | 3302 | 1 |
| starts: wide | the server's verdict from the lowest χ² of lagdrift's fit and a grid of starts around it and the address's place, one run | 3302 | 1 |
| VPN dense: E2 | the dense fleet, extenders chosen by E2; 3 runs | 3092 | 9 |
| VPN dense: joint spread | the dense fleet, extenders chosen by joint spread; 3 runs | 3092 | 9 |
| VPN dense: VPN hubs | the dense fleet, extenders chosen by VPN hubs; 3 runs | 3092 | 9 |
| VPN dense: information | the dense fleet, extenders chosen by information; 3 runs | 3092 | 9 |
| VPN dense: information ring | the dense fleet, extenders chosen by information ring; 3 runs | 3092 | 9 |
| VPN dense: ring mid band | the dense fleet, extenders chosen by ring mid band; 3 runs | 3092 | 9 |
| VPN dense: hybrid | the dense fleet, extenders chosen by hybrid; 3 runs | 3092 | 9 |
| VPN sparse: E2 | the sparse fleet, extenders chosen by E2; 3 runs | 3092 | 9 |
| VPN sparse: joint spread | the sparse fleet, extenders chosen by joint spread; 3 runs | 3092 | 9 |
| VPN sparse: VPN hubs | the sparse fleet, extenders chosen by VPN hubs; 3 runs | 3092 | 9 |
| VPN sparse: information | the sparse fleet, extenders chosen by information; 3 runs | 3092 | 9 |
| VPN sparse: information ring | the sparse fleet, extenders chosen by information ring; 3 runs | 3092 | 9 |
| VPN sparse: ring mid band | the sparse fleet, extenders chosen by ring mid band; 3 runs | 3092 | 9 |
| VPN sparse: hybrid | the sparse fleet, extenders chosen by hybrid; 3 runs | 3092 | 9 |
| interrupted: counted | an interrupted run counts with 6 usable extenders, as before the review; one run | 4018 | 2 |
| interrupted: review's rule | an interrupted run counts only if converged or clear held when the silence began; one run | 4018 | 2 |
| Oceania: base | the study's world | 2175 | 1 |
| Oceania: normal stretch | the route stretch normal and unfloored: no long tail of detours | 2175 | 1 |
| Oceania: no long-path error | no route error on paths of 2,500 km or more | 2175 | 1 |
| Oceania: no short-path error | no route error on paths under 2,500 km | 2175 | 1 |
| Oceania: no place error | every extender exactly at its place | 2175 | 1 |
| Oceania: ideal pings | no queue, jitter, spike or loss | 2175 | 1 |
| Oceania: denser fleet | 9 more extenders: Adelaide, Canberra, Hobart, Darwin, Wellington, Christchurch, Port Moresby, Suva, Nouméa | 2175 | 1 |
| Oceania: model κ 0.30 | the model's κ 0.30 for Oceania (a κ calibrated per continent, V4) | 2175 | 1 |
| Oceania: model κ 0.35 | the model's κ 0.35 for Oceania | 2175 | 1 |
| Oceania: model κ 0.45 | the model's κ 0.45 for Oceania | 2175 | 1 |
| Oceania: s_ext by distance | each extender's place error at least 5% of its distance from the address's place, within lagdrift's [10, 100] km hold | 2175 | 1 |

### Scenarios

| Scenario | Address | Country | Visitors | Couldn't place |
|---|--:|--:|--:|--:|
| tiny country, 20 km | their own country | a disk of 20 km radius, the visitor anywhere in it | 5600 | 0 |
| small country, 5 km in | their own country | a disk of 150 km radius, the visitor 5 km inside its edge | 1400 | 0 |
| large country, 5 km in | their own country | a disk of 600 km radius, the visitor 5 km inside its edge | 1400 | 0 |
| small country, center | their own country | a disk of 150 km radius, the visitor 150 km inside its edge | 1400 | 0 |
| large country, 100 km in | their own country | a disk of 600 km radius, the visitor 100 km inside its edge | 1400 | 0 |
| real country | their own country | Natural Earth 1:10m | 3472 | 28 |
| claim 300 km | claims a country 300 km away | a disk around a real city, its edge exactly at the distance | 920 | 130 |
| claim 1,000 km | claims a country 1000 km away | a disk around a real city, its edge exactly at the distance | 951 | 99 |
| claim 3,000 km | claims a country 3000 km away | a disk around a real city, its edge exactly at the distance | 1001 | 49 |
| real claim 300 km | claims a country 300 km away | Natural Earth 1:10m, its nearest point 0.8 to 1.25 times the distance away | 493 | 557 |
| real claim 1,000 km | claims a country 1000 km away | Natural Earth 1:10m, its nearest point 0.8 to 1.25 times the distance away | 754 | 296 |
| real claim 3,000 km | claims a country 3000 km away | Natural Earth 1:10m, its nearest point 0.8 to 1.25 times the distance away | 1050 | 0 |

### Coverage and calibration: lagdrift

The fleet around each region's visitors, and the single-run fits of the tiny-country visitors: their σ_major and how often a run's b is held at its bound of 0. V10, at V11's k for the address's continent, k_early KEarly(k).

| Region | Cities | Extenders within 1,000 km | 4th nearest extender, km | σ_major, 1 run, km | b held at 0 |
|---|--:|--:|--:|--:|--:|
| Europe | 43 | 52 | 251 | 149 | 0.88% |
| North America | 29 | 10 | 352 | 208 | 4.12% |
| Eastern Europe, Middle East | 17 | 7 | 689 | 202 | 1.50% |
| Asia | 20 | 3 | 1149 | 375 | 5.75% |
| South America | 15 | 1 | 1968 | 466 | 6.12% |
| Africa | 18 | 1 | 3457 | 559 | 8.00% |
| Oceania | 8 | 1 | 2289 | 561 | 19.9% |

How often the region at each confidence, at V11's k for the address's continent, k_early KEarly(k), misses the truth, for tiny-country visitors (V6's coverage, the false-reject rate's ceiling for any country). Nominal: 5% and 1%.

| Region | 1 run: 95% | 99% | 3 runs: 95% | 99% |
|---|--:|--:|--:|--:|
| Europe | 0.63% | 0.50% | 1.12% | 0.12% |
| North America | 1.63% | 0.63% | 2.12% | 0.88% |
| Eastern Europe, Middle East | 1.01% | 0.25% | 1.62% | 0.62% |
| Asia | 1.76% | 1.01% | 2.75% | 0.50% |
| South America | 2.23% | 0.92% | 2.77% | 0.76% |
| Africa | 5.16% | 0.38% | 5.00% | 1.50% |
| Oceania | 4.57% | 0.53% | 5.14% | 0.48% |
| All | 2.34% (1.96–2.78) | 0.60% (0.43–0.85) | 2.86% (2.45–3.34) | 0.70% (0.51–0.96) |
| k that would hold it, every region (V11's is per continent) | 1.96 | 3.27 | 2.13 | 3.54 |

### The k the region needs: lagdrift

The empirical k per region, from honest visitors' single runs: the quantile of the truth's Mahalanobis distance from the fit at each confidence, where the textbook k is 2.448, 3.035 and 3.717. V11's table rests on these quantiles per continent (its own section).

| Region | Fits | k at 95% | 99% | 99.9% |
|---|--:|--:|--:|--:|
| Europe | 2085 | 1.73 | 2.43 | 4.05 |
| North America | 2074 | 1.80 | 2.94 | 4.01 |
| Eastern Europe, Middle East | 2090 | 1.84 | 2.81 | 6.27 |
| Asia | 2091 | 1.83 | 3.14 | 5.26 |
| South America | 2013 | 2.07 | 3.23 | 5.13 |
| Africa | 2088 | 2.50 | 3.75 | 5.53 |
| Oceania | 1483 | 2.87 | 8.08 | 15.26 |
| All | 13,924 | 1.99 | 3.46 | 9.39 |

V4 estimating k from tracks (honest single runs): measured from the truth, from a city point off it by about 30 km (a normal error of 24 km per axis), and from the city point with its variance added to the fit's covariance, which takes the city's error back out. Then how far the quantiles of a sample of tracks spread, over 300 samples.

| Measured from | Tracks | k at 99%: estimate | 5–95% of estimates | k at 99.9%: estimate | 5–95% of estimates |
|---|--:|--:|--:|--:|--:|
| the truth | 13,924 | 3.46 |  | 9.39 |  |
| the city point | 13,924 | 3.47 |  | 9.33 |  |
| the city point, its variance added | 13,924 | 3.44 |  | 9.21 |  |
| the city point, its variance added: a sample of | 1000 | 3.38 | 2.92–3.95 | 6.24 | 4.25–13.65 |
| the city point, its variance added: a sample of | 3000 | 3.43 | 3.17–3.72 | 7.85 | 5.34–12.41 |
| the city point, its variance added: a sample of | 10,000 | 3.44 | 3.24–3.55 | 8.00 | 6.30–11.30 |
| the city point, its variance added: a sample of | 30,000 | 3.44 | 3.34–3.47 | 9.14 | 7.52–10.36 |

### False rejects: lagdrift

Honest visitors rejected (disagree), after 1, 2 and 3 pooled runs, V10 at V11's k for the address's continent, k_early KEarly(k). Bold exceeds 1 − confidence.

| Scenario | Runs | 95% (limit 5%) | 99% (limit 1%) | Inconclusive at 99% |
|---|--:|--:|--:|--:|
| tiny country, 20 km | 1 | 2.05% (1.71–2.46) | 0.52% (0.36–0.74) | 5.20% |
| tiny country, 20 km | 2 | 2.07% (1.73–2.48) | 0.48% (0.33–0.70) | 3.48% |
| tiny country, 20 km | 3 | 2.32% (1.96–2.75) | 0.64% (0.46–0.89) | 3.30% |
| small country, 5 km in | 1 | 1.57% (1.04–2.37) | 0.21% (0.073–0.63) | 5.14% |
| small country, 5 km in | 2 | 1.00% (0.60–1.67) | 0.29% (0.11–0.73) | 3.93% |
| small country, 5 km in | 3 | 1.36% (0.87–2.11) | 0.36% (0.15–0.83) | 3.71% |
| large country, 5 km in | 1 | 0.71% (0.39–1.31) | 0.43% (0.20–0.93) | 4.43% |
| large country, 5 km in | 2 | 0.93% (0.54–1.58) | 0.29% (0.11–0.73) | 3.71% |
| large country, 5 km in | 3 | 1.00% (0.60–1.67) | 0.071% (0.013–0.40) | 3.43% |
| small country, center | 1 | 2.07% (1.45–2.96) | 0.64% (0.34–1.22) | 4.36% |
| small country, center | 2 | 1.64% (1.10–2.45) | 0.50% (0.24–1.03) | 3.14% |
| small country, center | 3 | 1.43% (0.93–2.20) | 0.57% (0.29–1.12) | 2.86% |
| large country, 100 km in | 1 | 0.64% (0.34–1.22) | 0.29% (0.11–0.73) | 5.43% |
| large country, 100 km in | 2 | 0.64% (0.34–1.22) | 0.14% (0.039–0.52) | 3.71% |
| large country, 100 km in | 3 | 0.43% (0.20–0.93) | 0.14% (0.039–0.52) | 3.71% |
| real country | 1 | 0.35% (0.20–0.60) | 0.029% (0.005–0.16) | 5.36% |
| real country | 2 | 0.26% (0.14–0.49) | 0.086% (0.029–0.25) | 3.69% |
| real country | 3 | 0.17% (0.079–0.38) | 0.029% (0.005–0.16) | 3.40% |

The tiny country per region:

| Region | 1 run: 95% | 99% | 3 runs: 95% | 99% |
|---|--:|--:|--:|--:|
| Europe | 0.50% | 0.50% | 0.50% | 0.12% |
| North America | 1.25% | 0.38% | 1.62% | 0.88% |
| Eastern Europe, Middle East | 0.62% | 0.25% | 1.12% | 0.50% |
| Asia | 1.62% | 0.88% | 2.25% | 0.50% |
| South America | 1.75% | 0.88% | 2.12% | 0.75% |
| Africa | 5.00% | 0.38% | 4.75% | **1.38%** |
| Oceania | 3.62% | 0.38% | 3.88% | 0.38% |

Real countries per region:

| Region | 1 run: 95% | 99% | 3 runs: 95% | 99% |
|---|--:|--:|--:|--:|
| Europe | 0% | 0% | 0% | 0% |
| North America | 0.41% | 0% | 0.21% | 0.21% |
| Eastern Europe, Middle East | 0% | 0% | 0.20% | 0% |
| Asia | 0% | 0% | 0% | 0% |
| South America | 0.20% | 0% | 0.20% | 0% |
| Africa | 1.00% | 0.20% | 0.20% | 0% |
| Oceania | 0.80% | 0% | 0.40% | 0% |

### k_early: lagdrift

Per policy and confidence: the false-reject rate of the worst honest scenario and number of runs (bold above 1 − confidence), and the false rejects the clear stop adds over no clear stop at all (k_early ∞), per 10,000 honest verdicts. The cap-only policy never stops early. Each policy's k_early is its value at the textbook 99% k, scaled with the run's k as KEarly scales EarlyK (V11): EarlyK's row is V11's KEarly(k), and a run at 95% stops at the value itself.

| Policy | 95%: worst cell | added per 10,000 | 99%: worst cell | added per 10,000 | Honest: median stop, s | stopped clear | Claims: median stop, s | stopped clear |
|---|--:|--:|--:|--:|--:|--:|--:|--:|
| k_early 3 | 2.52% (tiny country, 20 km, 3) | +25.9 | 0.71% (tiny country, 20 km, 3) | +16.1 | 6.50 | 1.41% | 5.00 | 62.9% |
| k_early 3.25 | 2.50% (tiny country, 20 km, 3) | +17.9 | 0.71% (small country, center, 1) | +10.2 | 6.50 | 1.19% | 5.00 | 59.7% |
| k_early 3.5 | 2.46% (tiny country, 20 km, 3) | +14.5 | 0.68% (tiny country, 20 km, 3) | +8.4 | 6.50 | 1.00% | 5.25 | 56.5% |
| k_early 3.75 | 2.34% (tiny country, 20 km, 3) | +11.1 | 0.64% (tiny country, 20 km, 3) | +5.5 | 6.50 | 0.86% | 5.25 | 53.7% |
| k_early 4 | 2.32% (tiny country, 20 km, 3) | +10.7 | 0.64% (tiny country, 20 km, 3) | +5.7 | 6.50 | 0.77% | 5.25 | 50.9% |
| k_early 4.25 | 2.27% (tiny country, 20 km, 3) | +8.9 | 0.64% (tiny country, 20 km, 3) | +4.5 | 6.50 | 0.70% | 5.50 | 48.5% |
| k_early 4.5 | 2.25% (tiny country, 20 km, 3) | +8.2 | 0.61% (tiny country, 20 km, 3) | +2.7 | 6.50 | 0.62% | 5.50 | 46.3% |
| k_early 5 | 2.23% (tiny country, 20 km, 3) | +5.5 | 0.62% (tiny country, 20 km, 3) | +2.0 | 6.50 | 0.52% | 5.50 | 42.4% |
| k_early ∞ | 2.18% (tiny country, 20 km, 3) | +0.0 | 0.59% (tiny country, 20 km, 3) | +0.0 | 6.50 | 0% | 7.25 | 0% |

### Power: lagdrift

Claims rejected (disagree) at 99%, after 1, 2 and 3 pooled runs, V10 at V11's k for the address's continent, k_early KEarly(k). Synthetic claims: the claimed country's nearest point is exactly the distance away.

| Region | Claim | 1 run | 2 runs | 3 runs | Inconclusive, 1 run | Distance to the country, km |
|---|--:|--:|--:|--:|--:|--:|
| Europe | 300 km | 13.3% | 34.7% | 58.7% | 0.67% | 300 |
| Europe | 1000 km | 80.7% | 96.7% | 98.7% | 0% | 1000 |
| Europe | 3000 km | 64.7% | 73.3% | 88.7% | 26.7% | 3000 |
| North America | 300 km | 6.00% | 30.7% | 52.0% | 2.67% | 300 |
| North America | 1000 km | 62.9% | 81.4% | 88.6% | 3.57% | 1000 |
| North America | 3000 km | 73.3% | 91.3% | 96.0% | 22.0% | 3000 |
| Eastern Europe, Middle East | 300 km | 10.0% | 36.7% | 50.0% | 0.67% | 300 |
| Eastern Europe, Middle East | 1000 km | 74.7% | 92.7% | 94.7% | 0% | 1000 |
| Eastern Europe, Middle East | 3000 km | 77.3% | 91.3% | 94.0% | 15.3% | 3000 |
| Asia | 300 km | 8.89% | 14.4% | 14.4% | 0% | 300 |
| Asia | 1000 km | 65.2% | 78.5% | 88.9% | 2.22% | 1000 |
| Asia | 3000 km | 76.8% | 93.0% | 96.5% | 16.9% | 3000 |
| South America | 300 km | 0% | 2.80% | 5.61% | 2.80% | 300 |
| South America | 1000 km | 56.7% | 78.7% | 82.7% | 4.67% | 1000 |
| South America | 3000 km | 82.3% | 96.9% | 97.7% | 16.2% | 3000 |
| Africa | 300 km | 0% | 1.41% | 0% | 0% | 300 |
| Africa | 1000 km | 16.7% | 34.7% | 42.7% | 2.67% | 1000 |
| Africa | 3000 km | 75.3% | 92.7% | 97.3% | 8.67% | 3000 |
| Oceania | 300 km | 2.29% | 2.29% | 1.53% | 32.8% | 300 |
| Oceania | 1000 km | 10.5% | 14.5% | 14.5% | 9.21% | 1000 |
| Oceania | 3000 km | 41.1% | 49.6% | 53.5% | 39.5% | 3000 |
| All | 300 km | 5.98% (4.62–7.70) | 18.9% (16.5–21.6) | 28.5% (25.7–31.5) | 5.65% |  |
| All | 1000 km | 55.4% (52.2–58.5) | 72.0% (69.1–74.8) | 77.1% (74.3–79.6) | 2.73% |  |
| All | 3000 km | 70.4% (67.5–73.2) | 84.4% (82.0–86.5) | 89.6% (87.6–91.4) | 20.5% |  |

Real countries, their nearest point 0.8 to 1.25 times the distance away:

| Region | Claim | 1 run | 2 runs | 3 runs | Inconclusive, 1 run | Distance to the country, km |
|---|--:|--:|--:|--:|--:|--:|
| Europe | 300 km | 15.9% | 42.5% | 72.6% | 0.88% | 309 |
| Europe | 1000 km | 78.0% | 95.3% | 98.0% | 0% | 1051 |
| Europe | 3000 km | 70.0% | 92.0% | 96.0% | 20.0% | 3087 |
| North America | 300 km | 7.41% | 33.3% | 48.1% | 0% | 308 |
| North America | 1000 km | 58.1% | 77.4% | 93.5% | 16.1% | 1033 |
| North America | 3000 km | 70.0% | 84.0% | 88.0% | 26.7% | 3094 |
| Eastern Europe, Middle East | 300 km | 20.9% | 53.9% | 68.7% | 0.87% | 312 |
| Eastern Europe, Middle East | 1000 km | 66.0% | 86.0% | 91.3% | 0.67% | 1043 |
| Eastern Europe, Middle East | 3000 km | 72.7% | 91.3% | 94.0% | 14.7% | 3077 |
| Asia | 300 km | 6.56% | 14.8% | 16.4% | 1.64% | 345 |
| Asia | 1000 km | 49.7% | 70.6% | 80.4% | 4.90% | 1044 |
| Asia | 3000 km | 69.3% | 89.3% | 92.7% | 20.0% | 3112 |
| South America | 300 km | 0% | 0% | 0% | 1.67% | 310 |
| South America | 1000 km | 37.7% | 57.7% | 63.1% | 3.08% | 1041 |
| South America | 3000 km | 64.0% | 87.3% | 89.3% | 30.7% | 3249 |
| Africa | 300 km | 4.27% | 5.13% | 8.55% | 0.85% | 294 |
| Africa | 1000 km | 23.3% | 37.3% | 46.0% | 4.00% | 1062 |
| Africa | 3000 km | 68.0% | 94.0% | 95.3% | 18.7% | 3131 |
| Oceania | 3000 km | 29.3% | 52.0% | 54.0% | 47.3% | 3031 |
| All | 300 km | 10.8% (8.31–13.8) | 27.2% (23.4–31.3) | 39.4% (35.1–43.7) | 1.01% |  |
| All | 1000 km | 51.6% (48.0–55.1) | 70.0% (66.7–73.2) | 76.8% (73.6–79.7) | 3.05% |  |
| All | 3000 km | 63.3% (60.4–66.2) | 84.3% (82.0–86.4) | 87.0% (84.9–88.9) | 25.4% |  |

At each confidence, all regions:

| Claim | Runs | 95% | 99% |
|---|--:|--:|--:|
| 300 km | 1 | 17.3% | 5.98% |
| 300 km | 2 | 36.6% | 18.9% |
| 300 km | 3 | 44.5% | 28.5% |
| 1000 km | 1 | 77.0% | 55.4% |
| 1000 km | 2 | 90.1% | 72.0% |
| 1000 km | 3 | 93.3% | 77.1% |
| 3000 km | 1 | 77.7% | 70.4% |
| 3000 km | 2 | 88.8% | 84.4% |
| 3000 km | 3 | 93.2% | 89.6% |
| 300 km | 1 | 22.1% | 10.8% |
| 300 km | 2 | 46.5% | 27.2% |
| 300 km | 3 | 52.9% | 39.4% |
| 1000 km | 1 | 69.2% | 51.6% |
| 1000 km | 2 | 87.5% | 70.0% |
| 1000 km | 3 | 90.2% | 76.8% |
| 3000 km | 1 | 70.9% | 63.3% |
| 3000 km | 2 | 87.6% | 84.3% |
| 3000 km | 3 | 90.6% | 87.0% |

### Stop times: lagdrift

Run 1, V10 at 99% at V11's k for the address's continent, k_early KEarly(k): seconds from the sessions opening.

| Visitors | Region | p10, s | p50, s | p90, s | p99, s | converged | clear | interrupted | cap | run counted |
|---|--:|--:|--:|--:|--:|--:|--:|--:|--:|--:|
| honest | Europe | 4.75 | 5.50 | 8.50 | 12.00 | 98.1% | 0.048% | 0.19% | 1.63% | 99.8% |
| honest | North America | 5.00 | 5.75 | 8.50 | 12.00 | 95.9% | 2.26% | 0.14% | 1.73% | 99.9% |
| honest | Eastern Europe, Middle East | 5.00 | 6.25 | 8.50 | 12.00 | 98.5% | 0.19% | 0.29% | 1.05% | 99.7% |
| honest | Asia | 5.25 | 7.50 | 9.50 | 12.00 | 98.4% | 0.29% | 0.33% | 1.00% | 99.7% |
| honest | South America | 6.75 | 7.75 | 9.50 | 12.00 | 98.5% | 0.29% | 0.24% | 1.00% | 99.8% |
| honest | Africa | 6.25 | 7.75 | 8.50 | 10.25 | 99.2% | 0.24% | 0.19% | 0.38% | 99.8% |
| honest | Oceania | 7.50 | 7.75 | 8.75 | 12.00 | 97.7% | 0.62% | 0.48% | 1.24% | 99.5% |
| honest | All | 5.25 | 7.50 | 8.75 | 12.00 | 98.0% | 0.56% | 0.27% | 1.15% | 99.7% |
| claim | Europe | 3.25 | 5.25 | 8.00 | 10.00 | 54.6% | 44.8% | 0.23% | 0.35% | 99.8% |
| claim | North America | 3.50 | 5.25 | 8.50 | 11.50 | 53.4% | 45.5% | 0.15% | 0.93% | 99.8% |
| claim | Eastern Europe, Middle East | 3.25 | 5.25 | 8.00 | 11.25 | 54.3% | 45.1% | 0.12% | 0.46% | 99.9% |
| claim | Asia | 3.75 | 6.25 | 8.25 | 11.25 | 52.8% | 45.8% | 0.55% | 0.83% | 99.4% |
| claim | South America | 4.50 | 7.25 | 8.25 | 10.50 | 58.9% | 40.3% | 0.55% | 0.28% | 99.4% |
| claim | Africa | 4.75 | 7.50 | 8.25 | 11.25 | 73.2% | 26.0% | 0.23% | 0.58% | 99.8% |
| claim | Oceania | 5.25 | 7.75 | 9.00 | 12.00 | 74.1% | 21.6% | 1.44% | 2.88% | 98.6% |
| claim | All | 3.75 | 6.25 | 8.25 | 11.50 | 59.7% | 39.1% | 0.41% | 0.77% | 99.6% |

Run 2, V10 at 99% at V11's k for the address's continent, k_early KEarly(k): seconds from the sessions opening.

| Visitors | Region | p10, s | p50, s | p90, s | p99, s | converged | clear | interrupted | cap | run counted |
|---|--:|--:|--:|--:|--:|--:|--:|--:|--:|--:|
| honest | Europe | 4.50 | 5.00 | 7.00 | 9.75 | 99.3% | 0.14% | 0.14% | 0.43% | 99.9% |
| honest | North America | 4.50 | 5.25 | 7.50 | 9.50 | 95.3% | 4.08% | 0.34% | 0.29% | 99.7% |
| honest | Eastern Europe, Middle East | 4.75 | 5.25 | 7.50 | 9.25 | 99.1% | 0.52% | 0.14% | 0.19% | 99.9% |
| honest | Asia | 4.25 | 6.25 | 7.75 | 9.75 | 99.0% | 0.33% | 0.29% | 0.43% | 99.7% |
| honest | South America | 6.00 | 7.50 | 8.00 | 9.50 | 99.1% | 0.19% | 0.33% | 0.33% | 99.7% |
| honest | Africa | 5.50 | 7.50 | 8.00 | 9.75 | 98.6% | 0.29% | 0.62% | 0.48% | 99.4% |
| honest | Oceania | 5.50 | 7.50 | 8.00 | 9.75 | 98.4% | 0.33% | 0.67% | 0.57% | 99.3% |
| honest | All | 4.75 | 6.50 | 8.00 | 9.75 | 98.4% | 0.84% | 0.36% | 0.39% | 99.6% |
| claim | Europe | 3.00 | 4.75 | 6.25 | 8.25 | 38.1% | 61.6% | 0.12% | 0.12% | 99.9% |
| claim | North America | 3.00 | 4.00 | 7.25 | 10.00 | 38.3% | 61.1% | 0.31% | 0.31% | 99.7% |
| claim | Eastern Europe, Middle East | 3.00 | 4.50 | 7.00 | 9.00 | 34.9% | 64.2% | 0.69% | 0.23% | 99.3% |
| claim | Asia | 3.50 | 4.75 | 7.50 | 8.50 | 39.4% | 59.9% | 0.55% | 0.14% | 99.4% |
| claim | South America | 4.25 | 5.75 | 8.00 | 9.00 | 46.6% | 53.0% | 0.28% | 0.14% | 99.7% |
| claim | Africa | 4.25 | 6.75 | 8.00 | 9.50 | 60.3% | 39.2% | 0.35% | 0.12% | 99.7% |
| claim | Oceania | 5.25 | 7.50 | 8.25 | 12.00 | 68.3% | 30.2% | 0.82% | 0.62% | 99.2% |
| claim | All | 3.25 | 5.25 | 7.75 | 9.25 | 45.5% | 53.9% | 0.43% | 0.21% | 99.6% |

Run 3, V10 at 99% at V11's k for the address's continent, k_early KEarly(k): seconds from the sessions opening.

| Visitors | Region | p10, s | p50, s | p90, s | p99, s | converged | clear | interrupted | cap | run counted |
|---|--:|--:|--:|--:|--:|--:|--:|--:|--:|--:|
| honest | Europe | 4.25 | 4.75 | 5.75 | 8.75 | 99.5% | 0.096% | 0.19% | 0.19% | 99.8% |
| honest | North America | 4.25 | 5.25 | 7.00 | 10.00 | 94.2% | 4.90% | 0.34% | 0.53% | 99.7% |
| honest | Eastern Europe, Middle East | 3.50 | 5.00 | 7.25 | 8.25 | 99.3% | 0.48% | 0.19% | 0% | 99.8% |
| honest | Asia | 3.25 | 5.50 | 7.75 | 9.25 | 99.1% | 0.19% | 0.24% | 0.43% | 99.8% |
| honest | South America | 5.50 | 7.50 | 8.00 | 9.50 | 98.4% | 0.19% | 0.86% | 0.52% | 99.1% |
| honest | Africa | 4.75 | 7.25 | 8.00 | 9.00 | 99.0% | 0.43% | 0.43% | 0.19% | 99.6% |
| honest | Oceania | 5.00 | 5.50 | 7.75 | 9.25 | 99.0% | 0.24% | 0.52% | 0.29% | 99.5% |
| honest | All | 4.25 | 5.50 | 7.75 | 9.25 | 98.4% | 0.93% | 0.40% | 0.31% | 99.6% |
| claim | Europe | 3.00 | 3.75 | 5.50 | 7.75 | 27.6% | 72.1% | 0.23% | 0.12% | 99.8% |
| claim | North America | 3.00 | 3.75 | 6.00 | 8.25 | 27.2% | 72.2% | 0.46% | 0.15% | 99.5% |
| claim | Eastern Europe, Middle East | 3.00 | 4.00 | 5.75 | 8.00 | 28.6% | 71.2% | 0.12% | 0.12% | 99.9% |
| claim | Asia | 3.50 | 4.50 | 7.00 | 8.00 | 35.4% | 64.2% | 0.14% | 0.28% | 99.9% |
| claim | South America | 4.25 | 5.75 | 8.00 | 8.75 | 43.1% | 56.5% | 0.41% | 0% | 99.6% |
| claim | Africa | 4.00 | 5.75 | 8.00 | 8.75 | 57.0% | 42.5% | 0.47% | 0% | 99.5% |
| claim | Oceania | 5.00 | 5.50 | 8.00 | 10.50 | 67.1% | 30.2% | 2.06% | 0.62% | 97.9% |
| claim | All | 3.25 | 4.75 | 7.50 | 8.75 | 39.6% | 59.8% | 0.46% | 0.15% | 99.5% |

### Session openings

The main part's visitors under V10 (at V11's k for the address's continent, k_early KEarly(k)), with far sessions opening 3 s × min(1, d / 7,500 km) late, as on the real stack (e2e), and with each session opening after its own round trips alone, as the study had them before. Stop times over every visitor, seconds from the sessions opening; false rejects, misses and power at 99% unless named.

| Sessions | Run | Stop p50, s | p90, s | p99, s | converged | clear | cap | Runs counted | False rejects, tiny: 95% | 99% | real countries | Region misses (tiny) | Power: 300 km | 1,000 km | 3,000 km | low_precision: claims |
|---|--:|--:|--:|--:|--:|--:|--:|--:|--:|--:|--:|--:|--:|--:|--:|--:|
| far ones late (main) | 1 | 7.50 | 8.75 | 12.00 | 88.0% | 10.6% | 1.05% | 99.7% | 2.05% | 0.52% | 0.029% | 0.60% | 5.98% | 55.4% | 70.4% | 10.5% |
| far ones late (main) | 3 | 5.50 | 7.75 | 9.00 | 83.1% | 16.3% | 0.27% | 99.6% | 2.32% | 0.64% | 0.029% | 0.70% | 28.5% | 77.1% | 89.6% | 3.75% |
| staggered by round trips | 1 | 4.75 | 5.50 | 10.50 | 89.5% | 9.75% | 0.70% | 99.9% | 1.77% | 0.48% | 0.029% | 0.53% | 6.09% | 54.0% | 72.2% | 9.63% |
| staggered by round trips | 3 | 4.50 | 5.00 | 6.75 | 83.4% | 16.1% | 0.33% | 99.8% | 2.29% | 0.59% | 0.029% | 0.66% | 28.5% | 77.7% | 90.9% | 3.37% |

### What stopping early does

One run per visitor under V10, under V10 without the clear stop (k_early ∞), and with no early stop at all, so every run lasts to the 12 s cap. At 99% at V11's k for the address's continent, k_early KEarly(k).

|  | False rejects: tiny country | large country, 5 km in | Region misses the truth (tiny) | Power: 300 km | 1,000 km | 3,000 km | Median stop, s | p90 stop, s | At the cap |
|---|--:|--:|--:|--:|--:|--:|--:|--:|--:|
| k_early 4 | 0.43% | 0% | 0.53% | 5.53% | 54.5% | – | 7.50 | 8.75 | 0.81% |
| k_early ∞ | 0.29% | 0% | 0.38% | 5.37% | 53.2% | – | 7.50 | 9.00 | 0.87% |
| cap only | 0.29% | 0% | 0.38% | 5.20% | 54.8% | – | 12.00 | 12.00 | 99.2% |

### V10's cadence

The same visitors, 3 runs each, at V11's k for the address's continent, k_early KEarly(k): the frame's fit by V10's cadence (a warm refit at each ping round, the full multi-start when the extenders change, every 2 s and before any stop) and by the full multi-start at every ping round. Under the cadence, 1471 stops were withdrawn because the full fit before them landed in another basin.

| k_early | Rule: cadence | full fits | Worst cell at 99%: cadence | full fits | at 95%: cadence | full fits | Median stop, s: cadence | full fits | Runs that stop elsewhere | Verdicts at 99% that differ |
|---|--:|--:|--:|--:|--:|--:|--:|--:|--:|--:|
| 3 | holds | holds | 0.90% | 0.90% | 2.95% | 2.90% | 6.00 | 6.00 | 1.29% | 0.028% |
| 3.25 | holds | holds | 0.71% | 0.71% | 2.81% | 2.76% | 6.00 | 6.00 | 1.30% | 0.028% |
| 3.5 | holds | holds | 0.67% | 0.62% | 2.81% | 2.76% | 6.25 | 6.00 | 1.33% | 0.021% |
| 3.75 | holds | holds | 0.67% | 0.62% | 2.81% | 2.76% | 6.25 | 6.25 | 1.37% | 0.021% |
| 4 | holds | holds | 0.62% | 0.57% | 2.71% | 2.67% | 6.25 | 6.25 | 1.38% | 0.028% |
| 4.25 | holds | holds | 0.57% | 0.52% | 2.71% | 2.67% | 6.25 | 6.25 | 1.40% | 0.021% |
| 4.5 | holds | holds | 0.57% | 0.52% | 2.71% | 2.67% | 6.25 | 6.25 | 1.40% | 0.021% |
| 5 | holds | holds | 0.57% | 0.52% | 2.71% | 2.67% | 6.50 | 6.50 | 1.42% | 0.021% |
| ∞ | – | – | 0.52% | 0.57% | 2.62% | 2.62% | 6.50 | 6.50 | 1.52% | 0.035% |

### V11's fixed table on drift verdicts

The contract's choice and stops, 3 runs, each visitor judged at V11's 99% k for the address's continent (NA 3.25, SA 3.50, AF 3.95, OC 7.88, AS 3.35, AN 3.49, unknown 3.49, others 3.035), k_early KEarly(k). Per continent of the address, after 1, 2 and 3 runs: the region's misses for tiny-country visitors with their Wilson interval, false rejects, and power. Where the misses run over 1%, the k they'd need: their 99% quantile, rounded up to 0.05.

| Continent | k | Runs | Tiny visitors | Region misses | False rejects: tiny | real countries | Power: 300 km | 1,000 km | 3,000 km | Over 1%: k needed |
|---|--:|--:|--:|--:|--:|--:|--:|--:|--:|--:|
| EU | 3.035 | 1 | 3167 | 0.60% (0.38–0.94) | 0.57% | 0.13% | 12.6% | 75.3% | 54.5% |  |
| EU | 3.035 | 2 | 3178 | 0.57% (0.36–0.89) | 0.47% | 0% | 38.4% | 94.6% | 87.5% |  |
| EU | 3.035 | 3 | 3179 | 0.38% (0.22–0.66) | 0.25% | 0% | 57.7% | 98.7% | 97.7% |  |
| NA | 3.250 | 1 | 1987 | 0.35% (0.17–0.73) | 0.30% | 0% | 8.76% | 62.0% | 68.0% |  |
| NA | 3.250 | 2 | 2000 | 0.55% (0.31–0.98) | 0.45% | 0% | 34.1% | 80.4% | 90.6% |  |
| NA | 3.250 | 3 | 2000 | 0.70% (0.42–1.17) | 0.55% | 0% | 44.4% | 89.2% | 95.3% |  |
| AS | 3.350 | 1 | 2814 | 0.78% (0.52–1.18) | 0.78% | 0% | 6.84% | 62.6% | 72.5% |  |
| AS | 3.350 | 2 | 2820 | 0.64% (0.40–1.01) | 0.60% | 0% | 9.89% | 80.3% | 88.1% |  |
| AS | 3.350 | 3 | 2821 | 0.78% (0.52–1.18) | 0.64% | 0% | 11.4% | 86.4% | 94.0% |  |
| SA | 3.500 | 1 | 1921 | 0.83% (0.51–1.35) | 0.75% | 0% | 0% | 56.0% | 91.5% |  |
| SA | 3.500 | 2 | 1977 | 0.66% (0.38–1.12) | 0.60% | 0% | 2.70% | 76.6% | 97.0% |  |
| SA | 3.500 | 3 | 1982 | 0.61% (0.35–1.06) | 0.55% | 0.40% | 3.24% | 80.6% | 98.8% |  |
| AF | 3.950 | 1 | 1978 | 0.56% (0.31–0.99) | 0.50% | 0% | 1.15% | 19.5% | 72.7% |  |
| AF | 3.950 | 2 | 1998 | 0.70% (0.42–1.17) | 0.65% | 0.20% | 2.68% | 33.0% | 90.1% |  |
| AF | 3.950 | 3 | 2000 | 0.70% (0.42–1.17) | 0.50% | 0.20% | 3.07% | 37.1% | 94.9% |  |
| OC | 7.880 | 1 | 1430 | 1.61% (1.07–2.40) | 1.10% | 0.60% | 0.38% | 9.27% | 41.9% | **10.50** |
| OC | 7.880 | 2 | 1529 | 1.11% (0.70–1.77) | 0.85% | 0.60% | 0.38% | 11.9% | 56.5% | **10.15** |
| OC | 7.880 | 3 | 1546 | 1.23% (0.79–1.91) | 0.95% | 0.80% | 0.76% | 14.6% | 58.5% | **10.35** |
| All | per continent | 1 | 13,297 | 0.74% (0.61–0.90) | 0.66% | 0.12% | 6.46% | 55.0% | 67.8% |  |
| All | per continent | 2 | 13,502 | 0.67% (0.55–0.83) | 0.59% | 0.12% | 19.5% | 71.9% | 85.7% |  |
| All | per continent | 3 | 13,528 | 0.69% (0.56–0.84) | 0.55% | 0.20% | 27.4% | 77.0% | 90.7% |  |

### V11's k on drift verdicts

V11 calibrates k at 99% per continent on single-run lag.dev tracks (lag.dev's own choice of 8 extenders, the fit measured from the visitor's city point, light-impossible tracks left out) by its upper-bound rule: the value at rank n·q + 1.645·√(n·q·(1 − q)), rounded up to its 0.05 bin's upper edge, never below the textbook 3.035. Drift verdicts use the hybrid choice of 12 and pool up to 3 runs. Does lag.dev's k hold there? "Drift needs" is the k at which the drift verdicts' region would miss the truth 1% of the time, from the default-k part's tiny-country visitors.

| Continent | Default k | Lag tracks | left out: light | lag 99% quantile | Calibrated k (upper bound) | Drift needs: 1 run | 2 runs | 3 runs |
|---|--:|--:|--:|--:|--:|--:|--:|--:|
| EU | 3.035 | 14,078 | 0 | 3.17 | 3.35 | 2.30 | 2.35 | 2.46 |
| NA | 3.250 | 9153 | 0 | 3.12 | 3.25 | 2.82 | 2.80 | 3.08 |
| AS | 3.350 | 11,598 | 0 | 4.62 | 5.10 | 2.84 | 2.86 | 3.07 |
| SA | 3.500 | 8691 | 0 | 4.34 | 4.55 | 3.21 | 3.22 | 3.36 |
| AF | 3.950 | 7385 | 0 | 6.26 | 6.65 | 3.26 | 3.61 | 4.07 |
| OC | 7.880 | 6006 | 0 | 5.48 | 6.10 | 7.53 | 5.83 | 5.58 |

Drift verdicts at V11's default k (tiny-country visitors for the region's misses; k_early is KEarly(k)):

| Continent | k | Region misses: 1 run | 2 runs | 3 runs | False rejects, tiny: 1 run | 2 runs | 3 runs | real countries: 1 run | 2 runs | 3 runs |
|---|--:|--:|--:|--:|--:|--:|--:|--:|--:|--:|
| EU | 3.03 | 0.40% | 0.47% | 0.16% | 0.39% | 0.31% | 0.079% | 0% | 0% | 0% |
| NA | 3.25 | 0.63% | 0.62% | 0.88% | 0.38% | 0.25% | 0.88% | 0% | 0% | 0.21% |
| AS | 3.35 | 0.80% | 0.62% | 0.71% | 0.71% | 0.62% | 0.71% | 0% | 0% | 0% |
| SA | 3.50 | 0.92% | 0.63% | 0.76% | 0.88% | 0.62% | 0.75% | 0% | 0% | 0.60% |
| AF | 3.95 | 0.38% | 0.88% | 1.50% | 0.38% | 0.75% | 1.38% | 0% | 0% | 0.20% |
| OC | 7.88 | 0.53% | 0.49% | 0.48% | 0.38% | 0.38% | 0.38% | 0.80% | 1.00% | 0.80% |
| All | per continent | 0.60% | 0.61% | 0.70% | 0.52% | 0.48% | 0.64% | 0.12% | 0.14% | 0.26% |

Drift verdicts at the k calibrated on lag.dev tracks (tiny-country visitors for the region's misses; k_early is KEarly(k)):

| Continent | k | Region misses: 1 run | 2 runs | 3 runs | False rejects, tiny: 1 run | 2 runs | 3 runs | real countries: 1 run | 2 runs | 3 runs |
|---|--:|--:|--:|--:|--:|--:|--:|--:|--:|--:|
| EU | 3.35 | 0.32% | 0.31% | 0.16% | 0.24% | 0.24% | 0.079% | 0% | 0% | 0% |
| NA | 3.25 | 0.63% | 0.62% | 0.88% | 0.38% | 0.25% | 0.88% | 0% | 0% | 0.21% |
| AS | 5.10 | 0.27% | 0.35% | 0.18% | 0.18% | 0.35% | 0.18% | 0% | 0% | 0% |
| SA | 4.55 | 0.26% | 0.38% | 0.25% | 0.25% | 0.25% | 0.25% | 0% | 0% | 0% |
| AF | 6.65 | 0.13% | 0.25% | 0.38% | 0.12% | 0.12% | 0.38% | 0% | 0% | 0% |
| OC | 6.10 | 1.05% | 0.49% | 0.48% | 0.75% | 0.38% | 0.38% | 1.60% | 1.00% | 0.80% |
| All | per continent | 0.40% | 0.39% | 0.35% | 0.30% | 0.27% | 0.32% | 0.23% | 0.14% | 0.14% |

### Stops on the verdict's evidence

V10 (e2e): with far sessions opening late, as in every part, the frame decides its stops on its fit over every extender with 4 round trips, as before, or over the usable extenders only, as V10 says, at 8 usable round trips (lagdrift's StopRoundTrips, also the server's usable minimum, E5) and at 12 beside it. The contract otherwise, 3 runs; seconds from the sessions opening.

| Stops decided on | Usable round trips | Run | Stop p50, s | p90, s | p99, s | clear | cap | Runs counted | False rejects: tiny | real | Region misses | Power: 300 km | 1,000 km | 3,000 km | VPN pairs | low_precision: claims | honest |
|---|--:|--:|--:|--:|--:|--:|--:|--:|--:|--:|--:|--:|--:|--:|--:|--:|--:|
| 4 | 8 | 1 | 6.50 | 8.00 | 12.00 | 9.65% | 1.06% | 99.7% | 0.46% | 0.086% | 0.53% | 5.98% | 51.7% | 63.6% | 19.6% | 11.0% | 4.82% |
| 4 | 8 | 3 | 5.25 | 7.00 | 8.25 | 15.1% | 0.25% | 99.6% | 0.59% | 0.20% | 0.66% | 27.9% | 78.5% | 87.0% | 66.8% | 3.68% | 3.26% |
| 4 | 12 | 1 | 6.50 | 8.00 | 12.00 | 9.33% | 1.10% | 99.6% | 0.46% | 0.12% | 0.51% | 5.43% | 51.1% | 61.2% | 19.3% | 11.6% | 4.79% |
| 4 | 12 | 3 | 5.75 | 7.00 | 8.75 | 14.9% | 0.36% | 99.5% | 0.55% | 0.17% | 0.63% | 27.6% | 77.0% | 87.7% | 65.4% | 3.59% | 3.24% |
| usable | 8 | 1 | 7.50 | 8.75 | 12.00 | 10.3% | 1.06% | 99.7% | 0.52% | 0.12% | 0.60% | 6.20% | 55.5% | 69.9% | 23.6% | 8.92% | 4.79% |
| usable | 8 | 3 | 5.50 | 7.75 | 9.00 | 15.5% | 0.32% | 99.6% | 0.64% | 0.26% | 0.70% | 27.5% | 78.7% | 89.9% | 67.9% | 3.33% | 3.38% |
| usable | 12 | 1 | 8.50 | 9.75 | 12.00 | 10.1% | 1.44% | 99.6% | 0.57% | 0.12% | 0.62% | 6.20% | 55.2% | 69.6% | 23.9% | 9.04% | 4.79% |
| usable | 12 | 3 | 6.50 | 8.75 | 9.75 | 15.5% | 0.30% | 99.5% | 0.64% | 0.20% | 0.70% | 28.2% | 78.2% | 90.0% | 66.8% | 3.30% | 3.35% |

### Where the region's misses come from

Tiny-country visitors, one run each, in worlds that each take away a source of error the model's variance doesn't hold, and in the model's own world without all three. Nominal misses: 5%, 1% and 0.1%.

| World | Region misses: 95% | 99% | 99.9% | False rejects, 99%: tiny | large country, 5 km in | 99.9%: tiny | k that holds 95% | 99% | 99.9% |
|---|--:|--:|--:|--:|--:|--:|--:|--:|--:|
| base: the study's world | 3.04% (2.38–3.89) | 2.05% (1.51–2.76) | 1.10% (0.73–1.66) | 1.67% | 0.43% | 1.00% | 2.04 | 4.02 | 9.41 |
| ideal pings: no queue, jitter, spike or loss | 3.04% (2.37–3.88) | 1.94% (1.42–2.65) | 1.10% (0.73–1.65) | 1.71% | 0.43% | 0.90% | 2.05 | 3.84 | 9.52 |
| no place error: every extender exactly at its place | 3.14% (2.46–4.00) | 2.04% (1.51–2.76) | 1.05% (0.69–1.59) | 1.57% | 0.43% | 0.95% | 2.02 | 3.73 | 10.76 |
| normal stretch: the route stretch normal and unfloored | 6.35% (5.37–7.51) | 3.00% (2.34–3.84) | 1.45% (1.01–2.08) | 2.29% | 0.86% | 1.24% | 2.62 | 4.26 | 10.36 |
| model's world: all three: the route stretch normal, no place error, ideal pings | 5.95% (4.99–7.07) | 2.95% (2.29–3.78) | 1.50% (1.05–2.13) | 2.43% | 1.14% | 1.38% | 2.61 | 4.08 | 10.28 |

### The verdict's starts

The same visitors, one run each, with the server's verdict from lagdrift's fit, and from the lowest χ² of that fit and the fixed points a grid of starts settles in (300, 1,000 and 3,000 km in 6 bearings around the fit and around the address's place), when lower by more than 1: what V5's starts miss when the visitor is far from every one of them.

| Verdict's starts | False rejects, 99%: tiny | real countries | 99.9%: tiny | real countries | Region misses, 99% | 99.9% | VPN pairs | 300 km | 800 km | 1,500 km | 3,000 km | 6,000 km |
|---|--:|--:|--:|--:|--:|--:|--:|--:|--:|--:|--:|--:|
| lagdrift | 1.62% | 0.48% | 0.76% | 0.24% | 1.92% | 0.81% | 26.8% | 9.92% | 41.5% | 71.9% | 71.1% | 45.0% |
| wide | 1.52% | 0.48% | 0.67% | 0.24% | 1.82% | 0.71% | 26.4% | 9.92% | 41.5% | 70.7% | 71.1% | 43.9% |

### A visitor far from the address's place (VPN)

Visitors whose address claims a country they are not in, as a VPN's virtual location does, with the extenders chosen around the address's place by E2's earlier bands, each spread by bearing (E2), or by a variant (choice.go): the mid and far bands spread together with the near band (joint spread), an extender at each VPN hub nearest the address's place (VPN hubs), or greedily by the region they are expected to give for the address's place and the VPN hubs (information) or a ring of places 800 and 2,000 km around it (information ring), or E2's near and far bands with the 4 others by the ring (ring mid band), or E2's bands with only the mid band chosen by the ring and a short mid band's slots passing to the far band (hybrid, E2 as it now stands and every other part's choice). Each on the study's dense fleet and on a sparse one of one extender in each of 35 cities. Power is the share rejected at 99%, at V11's k for the address's continent, k_early KEarly(k); the k_early by the rule is its value at the textbook 99% k.

| Fleet, choice | k_early by the rule | False rejects, 99%, tiny: 1 run | 3 runs | real countries: 1 run | 3 runs | Worst cell at 95% | Region misses, 99% | VPN pairs: 1 run | 3 runs | Median stop, s |
|---|--:|--:|--:|--:|--:|--:|--:|--:|--:|--:|
| dense: E2 | 3 | 0.60% | 0.83% | 0% | 0.24% | 2.98% | 0.76% | 15.7% | 51.8% | 7.25 |
| dense: joint spread | 3 | 0.48% | 0.48% | 0% | 0.24% | 2.62% | 0.64% | 15.7% | 44.3% | 7.00 |
| dense: VPN hubs | 3.75 | 0.36% | 1.43% | 0% | 0% | 3.69% | 0.50% | 8.93% | 28.6% | 7.50 |
| dense: information | 3 | 0.83% | 0.48% | 0% | 0% | 3.81% | 0.88% | 11.8% | 25.7% | 8.00 |
| dense: information ring | 3 | 0.60% | 0.95% | 0% | 0% | 3.10% | 0.60% | 19.3% | 52.5% | 6.50 |
| dense: ring mid band | 3 | 0.24% | 0.95% | 0% | 0% | 3.33% | 0.48% | 11.8% | 57.5% | 7.00 |
| dense: hybrid | 3 | 0.48% | 0.83% | 0% | 0.24% | 2.74% | 0.63% | 22.9% | 66.8% | 7.25 |
| sparse: E2 | none holds | 1.79% | 1.79% | 0.24% | 0.48% | 4.88% | 2.26% | 30.7% | 49.6% | 7.75 |
| sparse: joint spread | none holds | 1.43% | 1.79% | 0.72% | 0.24% | 4.64% | 1.64% | 30.4% | 47.1% | 7.50 |
| sparse: VPN hubs | none holds | 1.55% | 1.43% | 0.48% | 0.24% | 4.88% | 1.69% | 25.7% | 43.9% | 7.75 |
| sparse: information | 3.25 | 0.83% | 1.07% | 0.72% | 0.48% | 4.52% | 0.93% | 20.4% | 57.9% | 8.00 |
| sparse: information ring | 4.25 | 1.07% | 1.31% | 0.72% | 0% | 4.40% | 1.22% | 27.1% | 57.9% | 7.50 |
| sparse: ring mid band | 4.25 | 1.19% | 0.95% | 0.24% | 0.48% | 3.93% | 1.45% | 31.4% | 59.3% | 7.75 |
| sparse: hybrid | none holds | 1.79% | 1.79% | 0.24% | 0.48% | 4.88% | 2.26% | 32.5% | 50.7% | 7.75 |

The named pairs on the dense fleet:

| Visitor / address | Distance to the country, km | E2: 1 run | 3 runs | joint spread: 1 run | 3 runs | VPN hubs: 1 run | 3 runs | information: 1 run | 3 runs | information ring: 1 run | 3 runs | ring mid band: 1 run | 3 runs | hybrid: 1 run | 3 runs |
|---|--:|--:|--:|--:|--:|--:|--:|--:|--:|--:|--:|--:|--:|--:|--:|
| Madrid/DE | 1200 | 10.0% | 60.0% | 12.5% | 50.0% | 0% | 15.0% | 2.50% | 17.5% | 77.5% | 97.5% | 37.5% | 100.0% | 52.5% | 100.0% |
| Paris/NL | 276 | 2.50% | 65.0% | 2.50% | 35.0% | 12.5% | 20.0% | 10.0% | 45.0% | 5.00% | 65.0% | 2.50% | 55.0% | 2.50% | 65.0% |
| Warsaw/DE | 425 | 0% | 12.5% | 0% | 32.5% | 0% | 0% | 2.50% | 2.50% | 27.5% | 85.0% | 0% | 25.0% | 5.00% | 77.5% |
| Toronto/US | 27 | 0% | 0% | 0% | 0% | 0% | 0% | 0% | 0% | 0% | 0% | 0% | 0% | 0% | 0% |
| Mexico City/US | 735 | 0% | 30.0% | 0% | 0% | 0% | 30.0% | 0% | 0% | 0% | 0% | 0% | 30.0% | 0% | 32.5% |
| Singapore/JP | 3290 | 75.0% | 100.0% | 70.0% | 100.0% | 47.5% | 75.0% | 67.5% | 97.5% | 20.0% | 100.0% | 22.5% | 100.0% | 75.0% | 100.0% |
| Lagos/GB | 3407 | 22.5% | 95.0% | 25.0% | 92.5% | 2.50% | 60.0% | 0% | 17.5% | 5.00% | 20.0% | 20.0% | 92.5% | 25.0% | 92.5% |
| All pairs | 735 | 15.7% | 51.8% | 15.7% | 44.3% | 8.93% | 28.6% | 11.8% | 25.7% | 19.3% | 52.5% | 11.8% | 57.5% | 22.9% | 66.8% |

Real countries at each distance, on the dense fleet:

| Claim | Distance to the country, km | E2: 1 run | 3 runs | joint spread: 1 run | 3 runs | VPN hubs: 1 run | 3 runs | information: 1 run | 3 runs | information ring: 1 run | 3 runs | ring mid band: 1 run | 3 runs | hybrid: 1 run | 3 runs |
|---|--:|--:|--:|--:|--:|--:|--:|--:|--:|--:|--:|--:|--:|--:|--:|
| 300 km | 311 | 16.8% | 40.5% | 13.0% | 42.0% | 10.7% | 35.1% | 9.92% | 31.3% | 18.3% | 45.0% | 8.40% | 40.5% | 9.16% | 40.5% |
| 800 km | 810 | 35.6% | 65.4% | 37.8% | 62.8% | 39.9% | 61.7% | 27.1% | 47.3% | 51.6% | 69.7% | 38.3% | 68.6% | 41.0% | 65.4% |
| 1500 km | 1549 | 60.5% | 80.1% | 55.1% | 77.3% | 55.1% | 73.4% | 40.6% | 69.9% | 64.5% | 81.2% | 61.3% | 83.6% | 63.3% | 79.7% |
| 3000 km | 3183 | 68.6% | 89.6% | 63.6% | 86.8% | 63.6% | 83.2% | 56.8% | 85.7% | 52.1% | 85.4% | 60.0% | 90.4% | 67.9% | 88.9% |
| 6000 km | 6055 | 48.6% | 87.1% | 35.0% | 66.1% | 41.4% | 68.9% | 45.0% | 84.6% | 30.7% | 73.2% | 45.0% | 92.1% | 51.8% | 85.7% |

The named pairs on the sparse fleet:

| Visitor / address | Distance to the country, km | E2: 1 run | 3 runs | joint spread: 1 run | 3 runs | VPN hubs: 1 run | 3 runs | information: 1 run | 3 runs | information ring: 1 run | 3 runs | ring mid band: 1 run | 3 runs | hybrid: 1 run | 3 runs |
|---|--:|--:|--:|--:|--:|--:|--:|--:|--:|--:|--:|--:|--:|--:|--:|
| Madrid/DE | 1200 | 57.5% | 85.0% | 45.0% | 80.0% | 52.5% | 70.0% | 17.5% | 92.5% | 47.5% | 95.0% | 67.5% | 95.0% | 65.0% | 87.5% |
| Paris/NL | 276 | 30.0% | 45.0% | 22.5% | 40.0% | 17.5% | 42.5% | 15.0% | 32.5% | 22.5% | 37.5% | 25.0% | 42.5% | 30.0% | 45.0% |
| Warsaw/DE | 425 | 15.0% | 50.0% | 25.0% | 57.5% | 2.50% | 42.5% | 15.0% | 62.5% | 27.5% | 52.5% | 15.0% | 60.0% | 22.5% | 55.0% |
| Toronto/US | 27 | 0% | 0% | 0% | 0% | 0% | 0% | 0% | 0% | 0% | 0% | 0% | 0% | 0% | 0% |
| Mexico City/US | 735 | 12.5% | 20.0% | 2.50% | 17.5% | 12.5% | 22.5% | 2.50% | 22.5% | 0% | 20.0% | 12.5% | 20.0% | 12.5% | 20.0% |
| Singapore/JP | 3290 | 95.0% | 100.0% | 97.5% | 100.0% | 92.5% | 100.0% | 87.5% | 100.0% | 82.5% | 100.0% | 87.5% | 100.0% | 95.0% | 100.0% |
| Lagos/GB | 3407 | 5.00% | 47.5% | 20.0% | 35.0% | 2.50% | 30.0% | 5.00% | 95.0% | 10.0% | 100.0% | 12.5% | 97.5% | 2.50% | 47.5% |
| All pairs | 735 | 30.7% | 49.6% | 30.4% | 47.1% | 25.7% | 43.9% | 20.4% | 57.9% | 27.1% | 57.9% | 31.4% | 59.3% | 32.5% | 50.7% |

Real countries at each distance, on the sparse fleet:

| Claim | Distance to the country, km | E2: 1 run | 3 runs | joint spread: 1 run | 3 runs | VPN hubs: 1 run | 3 runs | information: 1 run | 3 runs | information ring: 1 run | 3 runs | ring mid band: 1 run | 3 runs | hybrid: 1 run | 3 runs |
|---|--:|--:|--:|--:|--:|--:|--:|--:|--:|--:|--:|--:|--:|--:|--:|
| 300 km | 311 | 8.40% | 10.7% | 8.40% | 13.0% | 6.11% | 13.0% | 6.87% | 8.40% | 8.40% | 12.2% | 7.63% | 10.7% | 7.63% | 10.7% |
| 800 km | 810 | 35.1% | 41.0% | 35.1% | 42.6% | 31.9% | 39.4% | 25.0% | 45.2% | 34.0% | 45.2% | 35.6% | 44.7% | 36.2% | 41.0% |
| 1500 km | 1549 | 55.5% | 60.9% | 54.7% | 63.7% | 49.6% | 62.9% | 43.8% | 69.1% | 53.1% | 68.4% | 53.5% | 67.6% | 55.5% | 60.9% |
| 3000 km | 3183 | 71.4% | 83.2% | 67.5% | 81.8% | 69.3% | 78.9% | 59.6% | 91.8% | 68.6% | 91.4% | 68.6% | 90.0% | 71.4% | 83.6% |
| 6000 km | 6055 | 58.9% | 84.3% | 53.6% | 81.8% | 58.6% | 79.3% | 55.4% | 99.6% | 57.1% | 99.6% | 62.1% | 95.7% | 59.6% | 83.9% |

### The interrupted stop

The review's rule (V10): an interrupted run counts only if converged or clear held, by the server's check, when the silence began. Against an honest client and one that goes silent at its first look whose region touches the claimed country at 99% once the run would count (6 extenders with 8 round trips), or when clear would stop it outside (review F-02). One run, 99%.

| Rule | Client | Claims agree: 300 km | 1,000 km | 3,000 km | Claims rejected: 300 km | 1,000 km | 3,000 km | False rejects (tiny) | Runs that don't count | interrupted |
|---|--:|--:|--:|--:|--:|--:|--:|--:|--:|--:|
| counted | honest (V10) | 88.1% | 42.8% | 10.3% | 5.69% | 54.6% | 69.6% | 0.43% | 0.27% | 0.37% |
| counted | silent at a favourable look | 80.0% | 41.7% | 9.88% | 2.28% | 40.0% | 48.8% | 0.21% | 0.27% | 98.5% |
| review's rule | honest (V10) | 88.1% | 42.8% | 10.3% | 5.53% | 54.5% | 69.6% | 0.43% | 0.37% | 0.37% |
| review's rule | silent at a favourable look | 0.16% | 0.31% | 0.15% | 1.79% | 38.7% | 47.3% | 0.14% | 84.8% | 98.5% |

### κ

After 1 run at 99%, at V11's k for the address's continent, k_early KEarly(k).

|  | False rejects: tiny country | large country, 5 km in | Region misses the truth (tiny) | Power: 300 km | 1,000 km | 3,000 km | Median stop, s | p90 stop, s | At the cap |
|---|--:|--:|--:|--:|--:|--:|--:|--:|--:|
| κ 0.15 in the world, 0.15 in the model, one run | 0.67% | 0.43% | 0.78% | 19.2% | 80.9% | 86.4% | 6.75 | 8.50 | 0.71% |
| κ 0.15 in the world, 0.25 in the model, one run | 0.38% | 0.14% | 0.50% | 5.06% | 57.6% | 72.3% | 6.50 | 8.25 | 0.98% |
| κ 0.25 in the world, 0.15 in the model, one run | 1.52% | 0.14% | 1.99% | 14.8% | 69.8% | 80.7% | 7.50 | 8.75 | 0.71% |
| κ 0.25 in the world, 0.25 in the model, one run | 0.76% | 0% | 0.91% | 4.40% | 55.0% | 70.3% | 7.50 | 8.50 | 0.79% |
| κ 0.25 in the world, 0.35 in the model, one run | 0.29% | 0% | 0.31% | 1.63% | 39.2% | 56.1% | 7.00 | 8.50 | 1.15% |
| κ 0.35 in the world, 0.25 in the model, one run | 0.19% | 0.14% | 0.51% | 6.04% | 52.6% | 64.3% | 7.50 | 9.25 | 1.17% |
| κ 0.35 in the world, 0.35 in the model, one run | 0.095% | 0% | 0.21% | 1.79% | 38.3% | 53.2% | 7.50 | 8.75 | 0.93% |

### V10's constants

One constant changed at a time from V10; round trips also set the server's usable extender (E5). After 1 run at 99%, at V11's k for the address's continent, k_early KEarly(k).

|  | False rejects: tiny country | large country, 5 km in | Region misses the truth (tiny) | Power: 300 km | 1,000 km | 3,000 km | Median stop, s | p90 stop, s | At the cap |
|---|--:|--:|--:|--:|--:|--:|--:|--:|--:|
| k_early 4 | 0.57% | 0.19% | 0.72% | 5.98% | 57.9% | – | 7.50 | 8.75 | 0.88% |
| round trips 12 | 0.51% | 0.29% | 0.66% | 5.76% | 57.3% | – | 8.50 | 9.75 | 1.39% |
| round trips 16 | 0.51% | 0.19% | 0.66% | 5.76% | 57.4% | – | 9.50 | 10.50 | 2.50% |
| window 1000 ms | 0.46% | 0.29% | 0.55% | 5.76% | 55.1% | – | 5.25 | 7.00 | 0.56% |
| window 3000 ms | 0.51% | 0.19% | 0.66% | 6.09% | 58.1% | – | 8.75 | 10.75 | 4.17% |
| area ratio 1.05 | 0.57% | 0.19% | 0.72% | 5.87% | 58.0% | – | 7.75 | 9.50 | 2.98% |
| area ratio 1.2 | 0.51% | 0.19% | 0.60% | 5.65% | 57.8% | – | 7.25 | 8.25 | 0.60% |
| extenders 6 | 0.57% | 0.19% | 0.78% | 6.09% | 57.3% | – | 7.50 | 8.50 | 0.88% |
| extenders 10 | 0.51% | 0.19% | 0.66% | 5.98% | 57.4% | – | 7.50 | 8.75 | 0.94% |

### Other worlds

After 1 run at 99%, at V11's k for the address's continent, k_early KEarly(k).

|  | False rejects: tiny country | large country, 5 km in | Region misses the truth (tiny) | Power: 300 km | 1,000 km | 3,000 km | Median stop, s | p90 stop, s | At the cap |
|---|--:|--:|--:|--:|--:|--:|--:|--:|--:|
| main | 0.52% | 0.43% | 0.60% | 5.98% | 55.4% | 70.4% | 7.50 | 8.75 | 1.05% |
| misplaced | 0.97% | 0.29% | 1.27% | 6.20% | 56.9% | – | 7.50 | 8.75 | 0.92% |
| normal stretch | 1.03% | 0.19% | 1.57% | 7.39% | 54.3% | – | 7.50 | 8.75 | 1.20% |

### Oceania

Oceania's visitors, one run each, at the textbook k, k_early 4, where V11 now judges Oceania at 7.88: in worlds that each take one source of error away, and under models that each try one remedy. The region's misses are the tiny country's; a false reject needs the whole region to leave the country (V6).

| Oceania | Region misses the truth, 99% (tiny) | False rejects, 99%: tiny | large country, 5 km in | real countries | real countries, 99.9% | Power, 99%: 300 km | 1,000 km | σ_major, km | b held at 0 |
|---|--:|--:|--:|--:|--:|--:|--:|--:|--:|
| base: the study's world | 3.10% (2.12–4.50) | 2.00% | 2.00% | 0.75% | 0.75% | 2.29% | 62.0% | 557 | 20.9% |
| normal stretch: the route stretch normal and unfloored: no long tail of detours | 3.76% (2.66–5.29) | 2.42% | 1.33% | 0.75% | 0.75% | 2.29% | 65.0% | 581 | 20.2% |
| no long-path error: no route error on paths of 2,500 km or more | 2.88% (1.97–4.19) | 1.67% | 1.00% | 0.50% | 0.25% | 4.00% | 68.0% | 471 | 6.42% |
| no short-path error: no route error on paths under 2,500 km | 4.02% (2.89–5.57) | 2.58% | 2.00% | 1.00% | 1.00% | 2.29% | 59.0% | 503 | 20.2% |
| no place error: every extender exactly at its place | 3.58% (2.52–5.06) | 2.33% | 2.33% | 0.75% | 0.75% | 2.29% | 66.0% | 560 | 21.1% |
| ideal pings: no queue, jitter, spike or loss | 3.19% (2.20–4.60) | 2.00% | 2.00% | 0.75% | 0.75% | 1.71% | 65.0% | 554 | 22.4% |
| denser fleet: 9 more extenders: Adelaide, Canberra, Hobart, Darwin, Wellington, Christchurch, Port Moresby, Suva, Nouméa | 0.35% (0.14–0.90) | 0.25% | 0% | 0% | 0% | 20.6% | 96.0% | 275 | 5.50% |
| model κ 0.30: the model's κ 0.30 for Oceania (a κ calibrated per continent, V4) | 1.53% (0.88–2.66) | 0.92% | 2.00% | 0.75% | 0.75% | 0.57% | 46.0% | 606 | 20.6% |
| model κ 0.35: the model's κ 0.35 for Oceania | 1.38% (0.75–2.52) | 0.83% | 2.00% | 0.75% | 0.75% | 0.57% | 43.0% | 656 | 20.7% |
| model κ 0.45: the model's κ 0.45 for Oceania | 1.48% (0.78–2.78) | 0.67% | 1.67% | 0.75% | 0.75% | 0% | 29.0% | 757 | 21.0% |
| s_ext by distance: each extender's place error at least 5% of its distance from the address's place, within lagdrift's [10, 100] km hold | 3.11% (2.13–4.52) | 2.00% | 2.00% | 0.75% | 0.75% | 1.71% | 62.0% | 561 | 20.8% |

The base world per city:

| City | Tiny: visitors | region misses, 99% | σ_major, km | Real country: false rejects, 99% | 99.9% | distance to the border, km |
|---|--:|--:|--:|--:|--:|--:|
| Sydney | 150 | 0% | 279 | 0% | 0% | 9 |
| Melbourne | 150 | 0% | 604 | 0% | 0% | 10 |
| Brisbane | 150 | 0% | 479 | 0% | 0% | 13 |
| Perth | 150 | 0% | 842 | 0% | 0% | 10 |
| Adelaide | 150 | 2.13% | 474 | 0% | 0% | 10 |
| Auckland | 150 | 16.7% | 1216 | 0% | 0% | 2 |
| Wellington | 150 | 13.0% | 1268 | 0% | 0% | 2 |
| Christchurch | 150 | 30.0% | 1198 | 6.00% | 6.00% | 8 |

### What a fit costs

By who fits and what. In the study: every fit's wall-clock duration, shown as the upper limit of its doubling bucket from 0.25 ms; the study's workers share the machine with each other and anything else, so these count waits for a core. One at a time: a sample of the same fits run again after the study on one thread, each measured by the CPU time it took, which is what a quiet machine would see. V10's budget: a warm refit at p95 ≤ 1 ms and a full fit at p95 ≤ 10 ms, in Go.

| Fit | In the study: fits | mean, ms | p50 under, ms | p95 under, ms | One at a time: fits | p50, ms | p90, ms | p95, ms | p99, ms | most, ms |
|---|--:|--:|--:|--:|--:|--:|--:|--:|--:|--:|
| the frame's full fit, one run | 1,901,952 | 0.91 | 0.5 | 4 | 1491 | 0.40 | 0.97 | 1.21 | 1.86 | 3.72 |
| the frame's full fit, pooled runs | 2,729,885 | 2.56 | 2 | 16 | 1466 | 1.05 | 2.33 | 3.00 | 5.44 | 9.08 |
| the server's fit, one run | 404,429 | 0.94 | 0.5 | 4 | 1471 | 0.42 | 0.87 | 1.08 | 1.67 | 2.75 |
| the server's fit, pooled runs | 453,334 | 2.91 | 2 | 16 | 1456 | 1.16 | 2.40 | 3.00 | 4.59 | 14.61 |
| the frame's warm refit, one run | 4,311,965 | 0.09 | 0.25 | 0.25 | 1493 | 0.03 | 0.10 | 0.14 | 0.27 | 0.82 |
| the frame's warm refit, pooled runs | 5,840,097 | 0.27 | 0.25 | 1 | 1500 | 0.09 | 0.28 | 0.39 | 0.66 | 1.48 |

### Checks against lagdrift

The study's stops and verdicts against lagdrift.EvaluateStop and lagdrift.Decide, every seventh visitor. And the fit's own (V5's acceptance): the checked visitors' verdict fits refitted warm from themselves, which should move under 1 km in at least 99.9% of fits, and, for every 21st visitor's single-run verdicts, a grid of starts (the address's place, the three fastest extenders, 6 bearings at 300, 1,000 and 3,000 km from the fit), each refitted until it settles, which should find no fixed point with a χ² lower by more than 1, the warm rule's tolerance.

| Part | EvaluateStop checks | disagree | Decide checks | disagree | Warm refits | moved ≥ 1 km | farthest, km | Grids | lower χ² | lower by > 1 | largest drop |
|---|--:|--:|--:|--:|--:|--:|--:|--:|--:|--:|--:|
| main | 0 | 0 | 11,125 | 0 | 11,125 | 0% | 0.1 | 1119 | 23 | 8 | 62.44 |
| staggered sessions | 0 | 0 | 8369 | 0 | 8369 | 0% | 0.1 | 917 | 19 | 8 | 19.50 |
| no early stop | 0 | 0 | 959 | 0 | 959 | 0% | 0.1 | 321 | 13 | 5 | 1.87 |
| cadence: refits | 0 | 0 | 2579 | 0 | 2579 | 0% | 0.1 | 259 | 11 | 0 | 0.80 |
| cadence: full fits | 29,555 | 0 | 2576 | 0 | 2576 | 0% | 0.1 | 259 | 11 | 0 | 0.80 |
| κ 0.15/0.15 | 0 | 0 | 528 | 0 | 528 | 0% | 0.1 | 175 | 1 | 0 | 0.91 |
| κ 0.15/0.25 | 0 | 0 | 515 | 0 | 515 | 0% | 0.1 | 167 | 2 | 1 | 1.60 |
| κ 0.25/0.15 | 0 | 0 | 532 | 0 | 532 | 0% | 0.1 | 176 | 6 | 1 | 4.75 |
| κ 0.25/0.25 | 0 | 0 | 512 | 0 | 512 | 0% | 0.1 | 168 | 5 | 3 | 3.31 |
| κ 0.25/0.35 | 0 | 0 | 494 | 0 | 494 | 0% | 0.1 | 159 | 7 | 1 | 1.68 |
| κ 0.35/0.25 | 0 | 0 | 499 | 0 | 499 | 0% | 0.1 | 167 | 3 | 1 | 1.09 |
| κ 0.35/0.35 | 0 | 0 | 488 | 0 | 488 | 0% | 0.1 | 160 | 4 | 2 | 1.41 |
| stop rule | 0 | 0 | 3552 | 0 | 3552 | 0% | 0.1 | 1187 | 22 | 18 | 16.99 |
| misplaced | 0 | 0 | 657 | 0 | 657 | 0% | 0.1 | 219 | 4 | 3 | 2.77 |
| normal stretch | 0 | 0 | 658 | 0 | 658 | 0% | 0.1 | 220 | 5 | 2 | 2.28 |
| V11: lag.dev tracks | 0 | 0 | 8168 | 0 | 8168 | 0% | 0.1 | 2714 | 121 | 24 | 2.82 |
| V11 drift: default k | 0 | 0 | 4347 | 0 | 4347 | 0% | 0.1 | 484 | 11 | 2 | 2.94 |
| V11 drift: lag k | 0 | 0 | 4349 | 0 | 4349 | 0% | 0.1 | 484 | 11 | 2 | 2.94 |
| V11 table | 0 | 0 | 9818 | 0 | 9818 | 0% | 0.1 | 1068 | 41 | 19 | 6.05 |
| late sessions: stops on 4 | 0 | 0 | 10,229 | 0 | 10,229 | 0% | 0.1 | 1113 | 28 | 11 | 22.22 |
| late sessions: stops on usable | 0 | 0 | 10,334 | 0 | 10,334 | 0% | 0.1 | 1128 | 26 | 9 | 2.98 |
| calibration: base | 0 | 0 | 387 | 0 | 387 | 0% | 0.1 | 129 | 6 | 0 | 0.80 |
| calibration: ideal pings | 0 | 0 | 386 | 0 | 386 | 0% | 0.1 | 128 | 5 | 0 | 0.58 |
| calibration: no place error | 0 | 0 | 387 | 0 | 387 | 0% | 0.1 | 129 | 5 | 0 | 0.50 |
| calibration: normal stretch | 0 | 0 | 379 | 0 | 379 | 0% | 0.1 | 126 | 3 | 0 | 0.86 |
| calibration: model's world | 0 | 0 | 380 | 0 | 380 | 0% | 0.1 | 126 | 4 | 1 | 1.52 |
| starts: lagdrift | 0 | 0 | 416 | 0 | 416 | 0% | 0.1 | 137 | 2 | 1 | 8.12 |
| starts: wide | 0 | 0 | 414 | 0 | 414 | 0% | 0.1 | 137 | 1 | 0 | 0.23 |
| VPN dense: E2 | 0 | 0 | 1953 | 0 | 1953 | 0% | 0.1 | 164 | 1 | 1 | 3.61 |
| VPN dense: joint spread | 0 | 0 | 1812 | 0 | 1812 | 0% | 0.1 | 156 | 4 | 2 | 3.05 |
| VPN dense: VPN hubs | 0 | 0 | 1749 | 0 | 1749 | 0% | 0.1 | 166 | 4 | 2 | 16.99 |
| VPN dense: information | 0 | 0 | 1765 | 0 | 1765 | 0% | 0.1 | 152 | 9 | 4 | 5.11 |
| VPN dense: information ring | 0 | 0 | 1894 | 0 | 1894 | 0% | 0.1 | 176 | 13 | 4 | 17.60 |
| VPN dense: ring mid band | 0 | 0 | 1932 | 0 | 1932 | 0% | 0.1 | 156 | 6 | 3 | 3.59 |
| VPN dense: hybrid | 0 | 0 | 1984 | 0 | 1984 | 0% | 0.1 | 164 | 2 | 0 | 0.84 |
| VPN sparse: E2 | 0 | 0 | 1842 | 0 | 1842 | 0% | 0.1 | 172 | 1 | 0 | 0.13 |
| VPN sparse: joint spread | 0 | 0 | 1821 | 0 | 1821 | 0% | 0.1 | 168 | 1 | 0 | 0.58 |
| VPN sparse: VPN hubs | 0 | 0 | 1822 | 0 | 1822 | 0% | 0.1 | 175 | 5 | 2 | 2.83 |
| VPN sparse: information | 0 | 0 | 1894 | 0 | 1894 | 0% | 0.1 | 162 | 7 | 4 | 2.53 |
| VPN sparse: information ring | 0 | 0 | 1956 | 0 | 1956 | 0% | 0.1 | 183 | 6 | 3 | 4.99 |
| VPN sparse: ring mid band | 0 | 0 | 1973 | 0 | 1973 | 0% | 0.1 | 181 | 3 | 1 | 1.18 |
| VPN sparse: hybrid | 0 | 0 | 1839 | 0 | 1839 | 0% | 0.1 | 173 | 1 | 0 | 0.13 |
| interrupted: counted | 0 | 0 | 951 | 0 | 951 | 0% | 0.1 | 316 | 17 | 6 | 18.11 |
| interrupted: review's rule | 0 | 0 | 564 | 0 | 564 | 0% | 0.1 | 190 | 10 | 4 | 18.11 |
| Oceania: base | 0 | 0 | 224 | 0 | 224 | 0% | 0.1 | 78 | 2 | 0 | 0.71 |
| Oceania: normal stretch | 0 | 0 | 219 | 0 | 219 | 0% | 0.1 | 76 | 1 | 0 | 0.90 |
| Oceania: no long-path error | 0 | 0 | 241 | 0 | 241 | 0% | 0.1 | 80 | 1 | 0 | 0.01 |
| Oceania: no short-path error | 0 | 0 | 223 | 0 | 223 | 0% | 0.1 | 76 | 2 | 1 | 3.42 |
| Oceania: no place error | 0 | 0 | 222 | 0 | 222 | 0% | 0.1 | 77 | 1 | 1 | 2.14 |
| Oceania: ideal pings | 0 | 0 | 226 | 0 | 226 | 0% | 0.1 | 78 | 2 | 0 | 0.67 |
| Oceania: denser fleet | 0 | 0 | 300 | 0 | 300 | 0% | 0.1 | 99 | 5 | 2 | 2.73 |
| Oceania: model κ 0.30 | 0 | 0 | 204 | 0 | 204 | 0% | 0.1 | 72 | 2 | 0 | 0.49 |
| Oceania: model κ 0.35 | 0 | 0 | 187 | 0 | 187 | 0% | 0.1 | 63 | 3 | 1 | 1.39 |
| Oceania: model κ 0.45 | 0 | 0 | 159 | 0 | 159 | 0% | 0.1 | 50 | 3 | 0 | 0.85 |
| Oceania: s_ext by distance | 0 | 0 | 224 | 0 | 224 | 0% | 0.1 | 78 | 2 | 0 | 0.68 |

- main: visitor 693 (Europe, Lille) 0@30|8: χ² 7.10 at 50.272,3.688, 62 km from the truth; a start converges to χ² 4.15 at 54.199,9.025, 568 km away and 561 km from the truth
- main: visitor 1512 (North America, Buffalo) 0@21|8: χ² 4.98 at 42.620,-78.875, 37 km from the truth; a start converges to χ² 3.60 at 46.018,-78.214, 381 km away and 345 km from the truth
- main: visitor 7392 (North America, San Diego) 0@48|8: χ² 5.93 at 32.204,-117.372, 70 km from the truth; a start converges to χ² 1.79 at 27.224,-131.163, 1441 km away and 1477 km from the truth
- staggered sessions: visitor 1512 (North America, Buffalo) 0@17|8: χ² 5.42 at 42.629,-78.859, 36 km from the truth; a start converges to χ² 4.03 at 45.999,-78.181, 379 km away and 343 km from the truth
- staggered sessions: visitor 10605 (South America, Córdoba) 0@48|8: χ² 14.20 at -33.920,-62.439, 321 km from the truth; a start converges to χ² 10.82 at -43.078,-61.248, 1023 km away and 1319 km from the truth
- staggered sessions: visitor 14364 (Oceania, Adelaide) 0@20|8: χ² 8.32 at -35.998,140.500, 202 km from the truth; a start converges to χ² 6.89 at -46.220,141.659, 1141 km away and 1268 km from the truth
- no early stop: visitor 315 (North America, Tijuana) 0@37|8: χ² 8.67 at 34.016,-117.722, 193 km from the truth; a start converges to χ² 6.80 at 32.616,-121.297, 367 km away and 405 km from the truth
- no early stop: visitor 315 (North America, Tijuana) 0@48|8: χ² 8.66 at 34.015,-117.719, 193 km from the truth; a start converges to χ² 6.80 at 32.609,-121.294, 367 km away and 405 km from the truth
- no early stop: visitor 1533 (North America, Austin) 0@21|8: χ² 6.24 at 29.942,-97.636, 50 km from the truth; a start converges to χ² 4.69 at 31.023,-91.777, 574 km away and 574 km from the truth
- κ 0.15/0.25: visitor 1197 (North America, San Diego) 0@23|8: χ² 3.21 at 33.197,-117.135, 64 km from the truth; a start converges to χ² 1.61 at 30.347,-123.868, 711 km away and 681 km from the truth
- κ 0.25/0.15: visitor 2142 (Asia, Surabaya) 0@39|8: χ² 22.57 at -6.450,112.160, 112 km from the truth; a start converges to χ² 17.82 at -10.715,101.335, 1281 km away and 1299 km from the truth
- κ 0.25/0.25: visitor 1197 (North America, San Diego) 0@33|8: χ² 9.25 at 33.079,-117.551, 60 km from the truth; a start converges to χ² 7.42 at 32.428,-122.148, 436 km away and 465 km from the truth
- κ 0.25/0.25: visitor 1386 (Asia, Bangalore) 0@39|8: χ² 5.71 at 14.388,79.141, 237 km from the truth; a start converges to χ² 3.69 at 8.921,78.282, 615 km away and 443 km from the truth
- κ 0.25/0.25: visitor 1575 (Africa, Kampala) 0@32|8: χ² 9.65 at 1.607,33.495, 175 km from the truth; a start converges to χ² 6.34 at -1.789,43.644, 1190 km away and 1260 km from the truth
- κ 0.25/0.35: visitor 1575 (Africa, Kampala) 0@32|8: χ² 4.94 at 1.600,33.450, 171 km from the truth; a start converges to χ² 3.26 at -1.624,43.573, 1181 km away and 1249 km from the truth
- κ 0.35/0.25: visitor 2961 (Africa, Durban) 0@32|8: χ² 3.00 at -23.635,24.517, 952 km from the truth; a start converges to χ² 1.91 at -30.648,36.476, 1416 km away and 533 km from the truth
- κ 0.35/0.35: visitor 693 (South America, Porto Alegre) 0@31|8: χ² 5.10 at -27.554,-56.271, 563 km from the truth; a start converges to χ² 3.68 at -39.915,-43.828, 1789 km away and 1291 km from the truth
- κ 0.35/0.35: visitor 3423 (Eastern Europe, Middle East, Kyiv) 0@20|8: χ² 5.53 at 43.581,28.688, 772 km from the truth; a start converges to χ² 4.44 at 41.451,21.413, 641 km away and 1214 km from the truth
- stop rule: visitor 420 (North America, Mexico City) 0@22|8: χ² 5.57 at 20.040,-98.573, 98 km from the truth; a start converges to χ² 4.17 at 15.848,-100.282, 500 km away and 404 km from the truth
- stop rule: visitor 420 (North America, Mexico City) 0@26|12: χ² 5.56 at 20.040,-98.588, 97 km from the truth; a start converges to χ² 4.17 at 15.893,-100.296, 495 km away and 400 km from the truth
- stop rule: visitor 420 (North America, Mexico City) 0@30|16: χ² 5.55 at 20.044,-98.585, 98 km from the truth; a start converges to χ² 4.16 at 15.881,-100.313, 498 km away and 402 km from the truth
- misplaced: visitor 420 (North America, Mexico City) 0@22|8: χ² 5.57 at 20.040,-98.573, 98 km from the truth; a start converges to χ² 4.17 at 15.848,-100.282, 500 km away and 404 km from the truth
- misplaced: visitor 1155 (South America, Córdoba) 0@34|8: χ² 7.50 at -30.826,-61.504, 259 km from the truth; a start converges to χ² 4.73 at -49.831,-56.587, 2153 km away and 2145 km from the truth
- misplaced: visitor 2415 (South America, Córdoba) 0@30|8: χ² 3.62 at -30.617,-64.975, 119 km from the truth; a start converges to χ² 2.27 at -48.348,-67.027, 1979 km away and 1897 km from the truth
- normal stretch: visitor 1155 (South America, Córdoba) 0@34|8: χ² 6.09 at -30.986,-62.188, 191 km from the truth; a start converges to χ² 3.81 at -50.231,-58.688, 2160 km away and 2143 km from the truth
- normal stretch: visitor 2814 (Europe, Birmingham) 0@27|8: χ² 8.14 at 48.091,4.260, 645 km from the truth; a start converges to χ² 5.94 at 51.991,-1.259, 586 km away and 59 km from the truth
- V11: lag.dev tracks: visitor 630 (Europe, Basel) 0@48|8: χ² 7.96 at 47.961,6.916, 63 km from the truth; a start converges to χ² 6.46 at 46.158,-5.140, 934 km away and 978 km from the truth
- V11: lag.dev tracks: visitor 1533 (Europe, Basel) 0@48|8: χ² 6.50 at 47.556,7.829, 11 km from the truth; a start converges to χ² 5.21 at 45.906,19.746, 926 km away and 935 km from the truth
- V11: lag.dev tracks: visitor 2478 (Europe, Zurich) 0@48|8: χ² 4.47 at 47.101,8.205, 29 km from the truth; a start converges to χ² 1.85 at 57.460,11.790, 1177 km away and 1149 km from the truth
- V11 drift: default k: visitor 693 (Europe, Lille) 0@30|8: χ² 7.10 at 50.272,3.688, 62 km from the truth; a start converges to χ² 4.15 at 54.199,9.025, 568 km away and 561 km from the truth
- V11 drift: default k: visitor 1512 (North America, Buffalo) 0@21|8: χ² 4.98 at 42.620,-78.875, 37 km from the truth; a start converges to χ² 3.60 at 46.018,-78.214, 381 km away and 345 km from the truth
- V11 drift: lag k: visitor 693 (Europe, Lille) 0@30|8: χ² 7.10 at 50.272,3.688, 62 km from the truth; a start converges to χ² 4.15 at 54.199,9.025, 568 km away and 561 km from the truth
- V11 drift: lag k: visitor 1512 (North America, Buffalo) 0@21|8: χ² 4.98 at 42.620,-78.875, 37 km from the truth; a start converges to χ² 3.60 at 46.018,-78.214, 381 km away and 345 km from the truth
- V11 table: visitor 693 (Europe, Lille) 0@30|8: χ² 7.10 at 50.272,3.688, 62 km from the truth; a start converges to χ² 4.15 at 54.199,9.025, 568 km away and 561 km from the truth
- V11 table: visitor 1701 (Europe, Brussels) 0@44|8: χ² 5.06 at 50.697,4.936, 34 km from the truth; a start converges to χ² 2.75 at 52.414,-4.697, 692 km away and 658 km from the truth
- V11 table: visitor 3528 (North America, Montreal) 0@22|8: χ² 3.40 at 44.972,-74.039, 78 km from the truth; a start converges to χ² 2.32 at 49.614,-72.025, 538 km away and 466 km from the truth
- late sessions: stops on 4: visitor 693 (Europe, Lille) 0@23|8: χ² 7.28 at 50.253,3.595, 60 km from the truth; a start converges to χ² 4.22 at 54.273,9.185, 587 km away and 575 km from the truth
- late sessions: stops on 4: visitor 693 (Europe, Lille) 0@25|12: χ² 6.30 at 50.264,3.710, 64 km from the truth; a start converges to χ² 4.18 at 53.412,8.264, 469 km away and 467 km from the truth
- late sessions: stops on 4: visitor 1512 (North America, Buffalo) 0@17|8: χ² 4.86 at 42.619,-79.001, 38 km from the truth; a start converges to χ² 3.58 at 46.048,-78.373, 385 km away and 347 km from the truth
- late sessions: stops on usable: visitor 693 (Europe, Lille) 0@30|8: χ² 7.10 at 50.272,3.688, 62 km from the truth; a start converges to χ² 4.15 at 54.199,9.025, 568 km away and 561 km from the truth
- late sessions: stops on usable: visitor 693 (Europe, Lille) 0@33|12: χ² 7.07 at 50.252,3.718, 65 km from the truth; a start converges to χ² 4.09 at 54.233,9.115, 575 km away and 568 km from the truth
- late sessions: stops on usable: visitor 1512 (North America, Buffalo) 0@21|8: χ² 4.98 at 42.620,-78.875, 37 km from the truth; a start converges to χ² 3.60 at 46.018,-78.214, 381 km away and 345 km from the truth
- calibration: model's world: visitor 399 (North America, Boston) 0@27|8: χ² 8.96 at 42.720,-71.728, 69 km from the truth; a start converges to χ² 7.44 at 42.108,-66.902, 402 km away and 341 km from the truth
- starts: lagdrift: visitor 2541 (Eastern Europe, Middle East, Moscow) 0@24|8: χ² 14.68 at 60.300,20.410, 1118 km from the truth; a start converges to χ² 6.56 at 57.129,41.657, 1270 km away and 294 km from the truth
- VPN dense: E2: visitor 0 (Europe, Paris) 0@48|8: χ² 5.09 at 51.807,2.900, 329 km from the truth; a start converges to χ² 1.48 at 49.930,-5.702, 638 km away and 608 km from the truth
- VPN dense: joint spread: visitor 0 (Europe, Paris) 0@48|8: χ² 4.56 at 52.037,2.516, 353 km from the truth; a start converges to χ² 1.51 at 49.239,-5.638, 653 km away and 597 km from the truth
- VPN dense: joint spread: visitor 3129 (Europe, Paris) 0@48|8: χ² 5.91 at 50.548,8.770, 496 km from the truth; a start converges to χ² 4.19 at 47.716,1.501, 615 km away and 146 km from the truth
- VPN dense: VPN hubs: visitor 2961 (Africa, Abuja) 0@30|8: χ² 5.70 at 9.080,7.909, 61 km from the truth; a start converges to χ² 3.46 at -0.296,-6.381, 1895 km away and 1851 km from the truth
- VPN dense: VPN hubs: visitor 3339 (Africa, Lagos) 0@33|8: χ² 27.55 at 40.825,31.545, 4723 km from the truth; a start converges to χ² 10.56 at 7.144,12.744, 4183 km away and 1039 km from the truth
- VPN dense: information: visitor 378 (Eastern Europe, Middle East, Sofia) 0@31|8: χ² 7.59 at 46.323,15.795, 710 km from the truth; a start converges to χ² 2.48 at 42.146,21.713, 661 km away and 144 km from the truth
- VPN dense: information: visitor 777 (Africa, Kinshasa) 0@32|8: χ² 3.47 at 1.914,14.655, 714 km from the truth; a start converges to χ² 2.17 at -12.633,7.343, 1808 km away and 1248 km from the truth
- VPN dense: information: visitor 1197 (North America, Phoenix) 0@32|8: χ² 7.19 at 36.925,-110.001, 434 km from the truth; a start converges to χ² 3.99 at 16.714,-110.619, 2248 km away and 1857 km from the truth
- VPN dense: information ring: visitor 315 (Europe, Porto) 0@13|8: χ² 9.75 at 41.350,-8.561, 21 km from the truth; a start converges to χ² 6.63 at 35.017,-6.228, 733 km away and 720 km from the truth
- VPN dense: information ring: visitor 651 (Eastern Europe, Middle East, Moscow) 0@13|8: χ² 8.01 at 58.933,19.806, 1119 km from the truth; a start converges to χ² 4.42 at 61.924,49.222, 1633 km away and 945 km from the truth
- VPN dense: information ring: visitor 966 (Asia, Bangkok) 0@32|8: χ² 28.28 at 6.419,131.209, 3446 km from the truth; a start converges to χ² 10.68 at 0.651,85.099, 5155 km away and 2245 km from the truth
- VPN dense: ring mid band: visitor 651 (Eastern Europe, Middle East, Moscow) 0@13|8: χ² 8.01 at 58.933,19.806, 1119 km from the truth; a start converges to χ² 4.42 at 61.924,49.222, 1633 km away and 945 km from the truth
- VPN dense: ring mid band: visitor 924 (Eastern Europe, Middle East, Ankara) 0@22|8: χ² 17.53 at 34.671,23.950, 989 km from the truth; a start converges to χ² 15.93 at 40.035,29.010, 745 km away and 337 km from the truth
- VPN dense: ring mid band: visitor 1428 (Europe, Basel) 0@20|8: χ² 5.04 at 47.468,7.478, 16 km from the truth; a start converges to χ² 3.85 at 46.443,11.032, 293 km away and 302 km from the truth
- VPN sparse: VPN hubs: visitor 672 (Eastern Europe, Middle East, Riyadh) 0@18|8: χ² 1.81 at 26.503,44.393, 317 km from the truth; a start converges to χ² 0.75 at -3.699,38.196, 3424 km away and 3284 km from the truth
- VPN sparse: VPN hubs: visitor 903 (North America, Calgary) 0@33|8: χ² 11.52 at 44.634,-109.796, 770 km from the truth; a start converges to χ² 8.69 at 18.586,-107.182, 2907 km away and 3651 km from the truth
- VPN sparse: information: visitor 672 (Eastern Europe, Middle East, Riyadh) 0@22|8: χ² 3.16 at 27.908,45.094, 401 km from the truth; a start converges to χ² 0.64 at -4.026,37.957, 3632 km away and 3326 km from the truth
- VPN sparse: information: visitor 1197 (North America, Phoenix) 0@32|8: χ² 5.54 at 34.971,-106.345, 547 km from the truth; a start converges to χ² 3.58 at 11.449,-104.515, 2622 km away and 2553 km from the truth
- VPN sparse: information: visitor 1617 (North America, San Francisco) 0@31|8: χ² 4.38 at 38.086,-120.446, 189 km from the truth; a start converges to χ² 2.53 at 29.344,-131.470, 1407 km away and 1244 km from the truth
- VPN sparse: information ring: visitor 1134 (Europe, Birmingham) 0@23|8: χ² 5.19 at 53.179,-2.257, 77 km from the truth; a start converges to χ² 3.18 at 58.873,24.032, 1738 km away and 1756 km from the truth
- VPN sparse: information ring: visitor 1134 (Europe, Birmingham) 0@22|8: χ² 8.54 at 49.296,-3.154, 370 km from the truth; a start converges to χ² 3.55 at 55.058,27.346, 2156 km away and 1925 km from the truth
- VPN sparse: information ring: visitor 1134 (Europe, Birmingham) 0@24|8: χ² 4.75 at 53.053,-2.636, 78 km from the truth; a start converges to χ² 3.25 at 59.100,24.072, 1774 km away and 1764 km from the truth
- VPN sparse: ring mid band: visitor 1134 (Europe, Birmingham) 0@22|8: χ² 5.30 at 50.060,-4.515, 330 km from the truth; a start converges to χ² 4.12 at 46.438,11.938, 1280 km away and 1202 km from the truth
- interrupted: counted: visitor 315 (North America, Tijuana) 0@37|8: χ² 8.67 at 34.016,-117.722, 193 km from the truth; a start converges to χ² 6.80 at 32.616,-121.297, 367 km away and 405 km from the truth
- interrupted: counted: visitor 1533 (North America, Austin) 0@21|8: χ² 6.24 at 29.942,-97.636, 50 km from the truth; a start converges to χ² 4.69 at 31.023,-91.777, 574 km away and 574 km from the truth
- interrupted: counted: visitor 1806 (South America, Santiago) 0@17|8: χ² 1.76 at -32.121,-67.804, 296 km from the truth; a start converges to χ² 0.52 at -55.782,-90.235, 3155 km away and 2908 km from the truth
- interrupted: review's rule: visitor 315 (North America, Tijuana) 0@37|8: χ² 8.67 at 34.016,-117.722, 193 km from the truth; a start converges to χ² 6.80 at 32.616,-121.297, 367 km away and 405 km from the truth
- interrupted: review's rule: visitor 1533 (North America, Austin) 0@21|8: χ² 6.24 at 29.942,-97.636, 50 km from the truth; a start converges to χ² 4.69 at 31.023,-91.777, 574 km away and 574 km from the truth
- interrupted: review's rule: visitor 3528 (Europe, Basel) 0@22|8: χ² 21.14 at 53.194,30.184, 1719 km from the truth; a start converges to χ² 3.02 at 62.537,-22.650, 3189 km away and 2503 km from the truth
- Oceania: no short-path error: visitor 1260 (Oceania, Adelaide) 0@48|8: χ² 13.72 at -36.066,141.971, 342 km from the truth; a start converges to χ² 10.30 at -46.154,144.373, 1140 km away and 1342 km from the truth
- Oceania: no place error: visitor 924 (Oceania, Adelaide) 0@33|8: χ² 10.92 at -35.436,144.052, 495 km from the truth; a start converges to χ² 8.78 at -44.492,147.858, 1058 km away and 1322 km from the truth
- Oceania: denser fleet: visitor 231 (Oceania, Christchurch) 0@26|8: χ² 7.47 at -41.881,172.489, 181 km from the truth; a start converges to χ² 4.73 at -47.151,-179.832, 844 km away and 715 km from the truth
- Oceania: denser fleet: visitor 1302 (Oceania, Wellington) 0@23|8: χ² 6.60 at -40.942,173.626, 100 km from the truth; a start converges to χ² 5.59 at -42.079,178.320, 411 km away and 312 km from the truth
- Oceania: model κ 0.35: visitor 924 (Oceania, Adelaide) 0@33|8: χ² 6.31 at -35.687,144.461, 535 km from the truth; a start converges to χ² 4.91 at -43.897,147.963, 960 km away and 1277 km from the truth


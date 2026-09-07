# Burn rate design

Date: 2026-09-07
Status: approved for planning

## Problem

The guard rail and the TUI both read a level. A level says how much of a window
is spent. It does not say how fast the window is draining.

That gap costs in three places.

1. The guard sizes its next check from the level alone. A quadratic ramp on the
   level cannot see a burst coming. Work at 40 percent that climbs 30 points in
   ten minutes gets the same slow check as work idling at 40 percent.
2. The now tab shows a gauge and a reset time. It cannot answer "when is this
   spent".
3. The history tab graphs the level over time. The reader must infer the slope
   by eye.

## What burn rate means here

Burn rate is percent of one rate limit window consumed per hour. A rate of
100%/h empties that window in one hour.

The `calls` table records what clusage's own probe calls cost. It has no
visibility into what the user's sessions spend. So burn rate comes from the
utilization series in the `readings` table, never from the token series. A
tokens-per-hour figure is not available from the data clusage holds.

## 1. The estimator

Add `rate.go` with one entry point.

```go
// burnRate returns percent of the window consumed per hour, and ok=false when
// the history cannot support an estimate.
func burnRate(readings []Reading, name string, now time.Time) (float64, bool)
```

`burnRate` replays a time-decayed exponentially weighted moving average over
the series that `utilSeries` already returns. Every reading is already stored,
so the function needs no state and no schema change.

For each consecutive pair of readings it computes the instantaneous rate.

```
inst  = (u2 - u1) / (t2 - t1)      percent per hour
alpha = 1 - exp(-dt / tau)
ewma  = ewma + alpha * (inst - ewma)
```

The decay uses elapsed time, not sample count. A 30 second hook probe and a 15
minute cron fetch then carry the weight their spacing deserves. Irregular
spacing is the one real weakness of a moving average over this data, and time
decay removes it.

`now` handles a stale history. After the last pair, decay the value once more
over the gap from the newest reading to `now`, with the same alpha against a
target of zero. A rate measured three hours ago must not report as current. A
newest reading older than four times `tau` returns `ok=false`.

`tau` comes from the window length in the window name. Use `tau = length / 8`.
That gives about 37 minutes for `5h` and about 21 hours for `7d`. A name that
carries no parseable length, such as `overage`, falls back to the `5h` value.

Keep `tau` as a single constant. Do not add a config key yet.

## 2. Reset segmentation

A rate limit window resets. Utilization then falls from 88 percent to near
zero. A naive delta reads that fall as a large negative rate.

Detect the end of a segment from two signals, in this order.

1. The `Reset` header changed between two readings. This is the honest signal
   that a new window began.
2. Utilization fell. Use this only when a reading carries no reset header.

Either signal ends the segment and restarts the average. `burnRate` returns
`ok=false` until two readings sit inside the new segment. The minutes after a
reset therefore report no rate, rather than a wrong one.

## 3. The guard rail

### The new column

`clusage usage` gains one column per window row. It sits between the status and
the reset text.

```
5h       33% used   allowed        14.2%/h   resets Wed 19:30 (in 4h36m)
```

This position is safe for the existing `win()` awk in the hook. That awk reads
`$1`, `$2` and `$4`, then searches for the literal string `resets`. A field
added after `$4` moves nothing it depends on. A test against both table forms
confirmed byte-identical parse output.

### How the hook reads it

The hook finds the rate by scanning fields for one that ends in `%/h`. It never
reads the rate by field position. An older clusage emits no such field, which
reads as an unknown rate. The hook then behaves exactly as v0.8.0 does.

### The gate

```
project(pct, rate, cut) = (cut - pct) / rate * 3600 / 4
interval = min(ramp(five, seven),
               project(five, rate5, SOFT),
               project(seven, rate7, HARD))
```

`pct` and `cut` are whole percents. `rate` is percent per hour. The division
gives hours, and the factor of 3600 gives seconds. Divide by four so four
checks land before the threshold, not one. Clamp the result to the existing
`INTERVAL_MIN` and `INTERVAL` bounds.

Drop any term whose rate is unknown or not positive. The ramp is then the
floor, so v0.8.0 behavior is the fallback on every degraded path.

### The stamp

The stamp grows to five fields.

```
<unix time> <5h percent> <7d percent> <5h rate> <7d rate>
```

Missing fields read as unknown, so a v0.8.0 stamp still works.

### Messages

The pause and deny text states a level and no trend. Add the rate and the
projection, so a denied agent learns whether the wait is short.

```
clusage guard rail: the 5h limit is at 91% and rising at 14.2%/h, so it
reaches 100% in about 38m.
```

Omit the clause when the rate is unknown.

The message projects to 100 percent, while the gate projects to the threshold.
That is deliberate. A message only appears once a threshold already tripped, so
the reader needs the distance to overage, not the distance to a cut behind
them.

## 4. The now tab

Add one dim row per window under its gauge.

```
burn 14.2%/h   full in 47m
```

The projection targets 100 percent, not the guard thresholds. A usage viewer
answers "when is this spent". The guard keeps its own cuts in its own config,
and the TUI must not duplicate them.

When `burnRate` returns `ok=false` the row reads `burn -`. Clamp a negative
rate to zero for display. A rate of zero reads `burn 0%/h` with no projection,
because nothing is draining.

## 5. The history tab

Add a second area chart under the utilization chart. Give it its own `%/h`
autoscale. A rate has no natural ceiling, so the fixed 0 to 100 percent pinning
used for utilization does not apply.

Height is the real cost. Today `chartH = height - 5 - len(wins)`, clamped to 3
and 16. The rate chart needs 3 rows plus 1 axis row, so it costs 4 rows at
minimum. Apply this rule against the row budget the tab already computes.

1. If the budget is 7 rows or more, split it. Give utilization the larger
   half, and give the rate chart at least 4 rows.
2. If the budget is 4 to 6 rows, replace the rate chart with one labelled
   sparkline row.
3. If the budget is under 4 rows, drop the rate row.

A short terminal then degrades, instead of pushing the help footer off screen.

`colorCols` bands by utilization thresholds. Those thresholds mean nothing for
a rate. Give the rate chart its own bands, keyed on projected time to
exhaustion rather than on the rate value.

## 6. Files

| File | Change |
|---|---|
| `rate.go` | new. `burnRate` and the segmentation |
| `rate_test.go` | new |
| `api.go` | the `%/h` column in `report` |
| `views.go` | the now row, the rate chart, the height rule |
| `chart.go` | rate colour bands |
| `hooks/clusage-guard.sh` | scan for the rate, `project`, wider stamp |
| `hooks/clusage-guard.test.sh` | projection and stamp cases |
| `README.md` | the new column, the projection, the bounds |

## 7. Tests

Name these cases.

- A flat series gives a rate near zero.
- A steady climb recovers its true slope.
- A reset gives `ok=false`, then recovers once two readings follow it.
- An irregular gap weighs by elapsed time, not by sample count.
- A single reading gives `ok=false`.
- An empty history gives `ok=false`.
- A window name with no parseable length still returns a rate.
- The guard takes the tighter of the ramp and the projection.
- A table with no `%/h` field reproduces v0.8.0 intervals exactly.
- A v0.8.0 stamp still parses.

## 8. Out of scope

- No `burn_tau_minutes` config key. One constant until the default proves
  wrong.
- No burn rate on the tokens tab. That table counts clusage's own probe cost,
  which is not what burn rate means here.
- No stored rates. Replay is cheap at these row counts.

## Decisions taken

| Question | Choice |
|---|---|
| Estimator | Time-decayed exponentially weighted moving average |
| Guard use | Take the tighter of the ramp and the projection |
| History view | Second area chart below the utilization chart |
| Now view | One row per window, rate plus projection to 100 percent |

# Chart Panning Investigation Note

**Status:** not started. This is a note to investigate later, not a plan to execute.

**Goal:** Let the arrow keys pan the charted time window left and right, so the
user can look at a stretch of history that the current span does not cover.

## Why this looks cheap

The history and token charts now render through `ntcharts`
(`github.com/NimbleMarkets/ntcharts v0.5.1`). That library already ships the
mechanism:

- `linechart.Model` holds a `UpdateHandler`, and the default is
  `XYAxesUpdateHandler(1, 1)`.
- `timeserieslinechart.Model.Update(msg)` forwards a Bubble Tea message to that
  handler, which moves the view range.
- `SetViewTimeRange(min, max)` sets the visible window inside the wider data
  range set by `SetTimeRange`.
- `WithZoneManager` adds mouse support through `bubblezone`, which is already an
  indirect dependency.

So panning needs a viewport offset in the model, not a new renderer.

## What to work out first

1. `timeChart` in `linechart.go` builds a fresh chart on every render and keeps
   no state. Panning needs the offset to live in the TUI model instead, and
   `timeChart` needs to take it.
2. The arrow keys currently do nothing on the History tab. Check them against
   the `tab` and `s` bindings in `tui.go`, and against the help text.
3. Decide what panning does to the summary line under the frames. The line
   reports `min`, `max` and `now` over the whole span. Panning the view should
   probably not change those numbers.
4. Decide whether the three charts on the History tab pan together. They share
   one time axis, so they should.
5. `readingsSince` only loads the current span from SQLite. Panning past the
   left edge of that span needs a wider query, or a reload on pan.
6. Check what panning means once the newest reading is off the right edge. The
   view should snap back to now on the next fetch, or say that it is not live.

## Success criteria

- Left and right arrows move the window, and the x-axis labels follow.
- A pan never invents data. A stretch with no reading still draws as a gap.
- `go test ./...` passes, and the frame widths still line up.

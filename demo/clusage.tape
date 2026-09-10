# The README demo. Record it with:
#
#   go build -ldflags "-X main.version=$(git describe --tags)" -o clusage .
#   vhs demo/clusage.tape
#
# It runs ./clusage, so the recording shows this working copy and not whatever
# is installed. See demo/README.md.
#
# The demo never presses r. That key calls the API, which spends real usage,
# and the stored reading already fills every tab.

Output "demo/clusage.gif"

Set Shell "bash"
Set FontSize 14
Set Width 1000
Set Height 560
Set Padding 20
# A terminal UI holds still between key presses, so a low framerate costs
# nothing visible and keeps the GIF small enough for the README.
Set Framerate 12
Set TypingSpeed 60ms

Hide
Type "clear" Enter
Show

Type "./clusage" Sleep 700ms Enter
Sleep 3s

# Now: step through the limit windows. The third press wraps back to 5h, and
# the History tab reads the same selection, so it opens on 5h.
Tab Sleep 1500ms
Tab Sleep 1500ms
Tab Sleep 1500ms

# History: the 5h window over three spans, then the 7d window. It stops on the
# 30d span, because the 7d window holds too few readings to fill a 6h graph.
Type "2" Sleep 2500ms
Type "s" Sleep 2s
Type "s" Sleep 2500ms
Tab Sleep 2500ms

# Tokens: what the probe calls have cost.
Type "3" Sleep 3s

# Config: the effective settings and the guard rail thresholds.
Type "4" Sleep 4s

# The full key list.
Type "?" Sleep 2500ms

Type "q" Sleep 1s

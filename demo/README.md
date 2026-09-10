# The README demo

[VHS](https://github.com/charmbracelet/vhs) runs `./clusage` against the stored
readings and records it. Re-record from the repository root:

```sh
go build -ldflags "-X main.version=$(git describe --tags)" -o clusage .
vhs demo/clusage.tape
```

Three things to know:

- VHS 0.12.0 records the frames, prints `Creating demo/clusage.gif...`, exits 0
  and writes no file. Use 0.11.0: `go install github.com/charmbracelet/vhs@v0.11.0`.
- The tape never presses `r`, because that key calls the API.
- The Config tab names paths. Check the frame before you commit a new GIF:
  `ffmpeg -ss 22 -i demo/clusage.gif -frames:v 1 /tmp/config-tab.png`.

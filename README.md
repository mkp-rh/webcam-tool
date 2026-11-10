# Build / run

## Prereqs

- `v4l2-ctl` (usually from `v4l-utils`)
- Build deps for Fyne (example for Fedora):

```
sudo dnf install -y gcc pkgconf-pkg-config mesa-libGL-devel libX11-devel libXrandr-devel libXcursor-devel libXi-devel libXxf86vm-devel
```

## Run

```
go run ./cmd/webcamctl
```

## Config

- Saved to `~/.config/webcam.conf`
- Remembers last selected camera and per-camera control values

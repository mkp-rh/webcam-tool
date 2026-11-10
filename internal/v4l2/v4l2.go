package v4l2

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

type CameraOption struct {
	Label  string
	Device string
}

type MenuItem struct {
	Index int64
	Label string
}

type Control struct {
	Name     string
	Type     string // bool, int, int64, menu, ...
	Min      int64
	Max      int64
	Step     int64
	Default  int64
	Value    int64
	ReadOnly bool
	Inactive bool
	Menu     []MenuItem // for menu type
}

var (
	reCtrlLine = regexp.MustCompile(`^([a-zA-Z0-9_]+)\s+0x[0-9a-fA-F]+\s+\(([^)]+)\)\s*:\s*(.*)$`)
	reKV       = regexp.MustCompile(`\b([a-zA-Z_]+)=([^\s]+)`)
	reMenuLine = regexp.MustCompile(`^\s*([0-9]+)\s*:\s*(.*)$`)
)

func ListCameras(ctx context.Context) ([]CameraOption, error) {
	out, err := run(ctx, "v4l2-ctl", "--list-devices")
	if err == nil && strings.TrimSpace(out) != "" {
		return parseListDevices(out), nil
	}
	// fallback: scan /dev/v4l/by-id and /dev/video*
	return scanDevicesFallback()
}

func ListControls(ctx context.Context, device string) ([]Control, error) {
	out, err := run(ctx, "v4l2-ctl", "-d", device, "--list-ctrls-menus")
	if err != nil {
		return nil, err
	}
	return parseControls(out)
}

func ApplyControls(ctx context.Context, device string, ctrls []Control, values map[string]int64) error {
	boolPhase := make([]string, 0, len(ctrls))
	autoMenuPhase := make([]string, 0, len(ctrls))
	restPhase := make([]string, 0, len(ctrls))

	for _, c := range ctrls {
		v, ok := values[c.Name]
		if !ok {
			continue
		}
		if c.ReadOnly || c.Inactive {
			continue
		}

		switch {
		case c.Type == "bool":
			boolPhase = append(boolPhase, fmt.Sprintf("%s=%d", c.Name, v))
		case c.Type == "menu" && IsAutoLike(c.Name):
			autoMenuPhase = append(autoMenuPhase, fmt.Sprintf("%s=%d", c.Name, v))
		default:
			restPhase = append(restPhase, fmt.Sprintf("%s=%d", c.Name, v))
		}
	}

	// stable ordering helps debugging/repro
	sort.Strings(boolPhase)
	sort.Strings(autoMenuPhase)
	sort.Strings(restPhase)

	// Requirement:
	// - bool controls must be set in a separate v4l2-ctl process
	// - bools must be set before any absolute values
	if len(boolPhase) > 0 {
		c1, cancel := context.WithTimeout(ctx, 2*time.Second)
		_, err := run(c1, "v4l2-ctl", "-d", device, "-c", strings.Join(boolPhase, ","))
		cancel()
		if err != nil {
			return err
		}
	}
	// Many devices expose auto settings as menus (e.g. exposure_auto).
	if len(autoMenuPhase) > 0 {
		c2, cancel := context.WithTimeout(ctx, 2*time.Second)
		_, err := run(c2, "v4l2-ctl", "-d", device, "-c", strings.Join(autoMenuPhase, ","))
		cancel()
		if err != nil {
			return err
		}
	}
	if len(restPhase) > 0 {
		c2, cancel := context.WithTimeout(ctx, 2*time.Second)
		_, err := run(c2, "v4l2-ctl", "-d", device, "-c", strings.Join(restPhase, ","))
		cancel()
		if err != nil {
			return err
		}
	}
	return nil
}

func EqualInt64Map(a, b map[string]int64) bool {
	if len(a) != len(b) {
		return false
	}
	for k, av := range a {
		if bv, ok := b[k]; !ok || bv != av {
			return false
		}
	}
	return true
}

func parseListDevices(out string) []CameraOption {
	var res []CameraOption
	sc := bufio.NewScanner(strings.NewReader(out))

	var curName string
	for sc.Scan() {
		line := sc.Text()
		if strings.TrimSpace(line) == "" {
			continue
		}
		if strings.HasSuffix(strings.TrimSpace(line), ":") && !strings.HasPrefix(line, "\t") && !strings.HasPrefix(line, " ") {
			curName = strings.TrimSuffix(strings.TrimSpace(line), ":")
			continue
		}
		if strings.HasPrefix(line, "\t") || strings.HasPrefix(line, " ") {
			node := strings.TrimSpace(line)
			if !strings.HasPrefix(node, "/dev/video") {
				continue
			}
			dev := preferStablePath(node)
			label := curName
			if label == "" {
				label = "Camera"
			}
			res = append(res, CameraOption{
				Label:  fmt.Sprintf("%s (%s)", label, dev),
				Device: dev,
			})
		}
	}
	return res
}

func preferStablePath(devNode string) string {
	byIDDir := "/dev/v4l/by-id"
	entries, err := os.ReadDir(byIDDir)
	if err != nil {
		return devNode
	}
	want, err := filepath.EvalSymlinks(devNode)
	if err != nil {
		want = devNode
	}
	for _, e := range entries {
		p := filepath.Join(byIDDir, e.Name())
		t, err := filepath.EvalSymlinks(p)
		if err != nil {
			continue
		}
		if t == want {
			return p
		}
	}
	return devNode
}

func scanDevicesFallback() ([]CameraOption, error) {
	var res []CameraOption

	// Prefer by-id (stable)
	if entries, err := os.ReadDir("/dev/v4l/by-id"); err == nil {
		for _, e := range entries {
			p := filepath.Join("/dev/v4l/by-id", e.Name())
			res = append(res, CameraOption{
				Label:  e.Name(),
				Device: p,
			})
		}
	}

	// Fallback to /dev/video*
	nodes, _ := filepath.Glob("/dev/video*")
	for _, n := range nodes {
		res = append(res, CameraOption{
			Label:  filepath.Base(n),
			Device: n,
		})
	}

	// Dedup by Device
	seen := map[string]bool{}
	out := make([]CameraOption, 0, len(res))
	for _, c := range res {
		if c.Device == "" || seen[c.Device] {
			continue
		}
		seen[c.Device] = true
		out = append(out, c)
	}
	if len(out) == 0 {
		return nil, errors.New("no video devices found (tried v4l2-ctl and /dev scanning)")
	}
	return out, nil
}

func parseControls(out string) ([]Control, error) {
	var ctrls []Control

	sc := bufio.NewScanner(strings.NewReader(out))
	var lastMenu *Control

	for sc.Scan() {
		line := sc.Text()
		if strings.TrimSpace(line) == "" {
			continue
		}

		// menu entries (indented and previous control was menu)
		if lastMenu != nil && (strings.HasPrefix(line, "\t") || strings.HasPrefix(line, " ")) {
			m := reMenuLine.FindStringSubmatch(strings.TrimSpace(line))
			if m != nil {
				var idx int64
				_, _ = fmt.Sscanf(m[1], "%d", &idx)
				lastMenu.Menu = append(lastMenu.Menu, MenuItem{Index: idx, Label: strings.TrimSpace(m[2])})
				continue
			}
		}

		m := reCtrlLine.FindStringSubmatch(strings.TrimSpace(line))
		if m == nil {
			lastMenu = nil
			continue
		}

		name := m[1]
		typ := strings.ToLower(strings.TrimSpace(m[2]))
		attrs := m[3]

		c := Control{
			Name: name,
			Type: typ,
		}

		kv := map[string]string{}
		for _, kvs := range reKV.FindAllStringSubmatch(attrs, -1) {
			kv[kvs[1]] = kvs[2]
		}

		c.Min = parseI64(kv["min"])
		c.Max = parseI64(kv["max"])
		c.Step = parseI64(kv["step"])
		c.Default = parseI64(kv["default"])
		c.Value = parseI64(kv["value"])

		if c.Type == "bool" {
			c.Min, c.Max, c.Step = 0, 1, 1
		}
		if c.Step == 0 {
			c.Step = 1
		}

		// flags
		if f, ok := kv["flags"]; ok {
			flags := strings.Split(f, ",")
			for _, fl := range flags {
				fl = strings.TrimSpace(strings.ToLower(fl))
				switch fl {
				case "inactive":
					c.Inactive = true
				case "read-only", "readonly":
					c.ReadOnly = true
				}
			}
		}

		ctrls = append(ctrls, c)
		if c.Type == "menu" {
			lastMenu = &ctrls[len(ctrls)-1]
		} else {
			lastMenu = nil
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return ctrls, nil
}

func IsAutoLike(name string) bool {
	n := strings.ToLower(name)
	return strings.Contains(n, "auto") || strings.Contains(n, "automatic")
}

func parseI64(s string) int64 {
	if s == "" {
		return 0
	}
	var v int64
	_, _ = fmt.Sscanf(s, "%d", &v)
	return v
}

func run(ctx context.Context, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	out := strings.TrimSpace(stdout.String())
	if err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		} else {
			msg = err.Error() + ": " + msg
		}
		return out, errors.New(msg)
	}
	return out, nil
}


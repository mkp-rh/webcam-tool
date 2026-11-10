package main

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/app"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/dialog"
	"fyne.io/fyne/v2/widget"

	"webcamctl/internal/config"
	"webcamctl/internal/v4l2"
)

const (
	applyDebounce   = 150 * time.Millisecond
	keepAlivePeriod = 4 * time.Second
)

type desiredState struct {
	mu     sync.RWMutex
	values map[string]int64
}

func newDesiredState() *desiredState {
	return &desiredState{values: map[string]int64{}}
}

func (d *desiredState) set(name string, v int64) {
	d.mu.Lock()
	d.values[name] = v
	d.mu.Unlock()
}

func (d *desiredState) get(name string, fallback int64) int64 {
	d.mu.RLock()
	v, ok := d.values[name]
	d.mu.RUnlock()
	if !ok {
		return fallback
	}
	return v
}

func (d *desiredState) snapshot() map[string]int64 {
	d.mu.RLock()
	defer d.mu.RUnlock()
	cp := make(map[string]int64, len(d.values))
	for k, v := range d.values {
		cp[k] = v
	}
	return cp
}

func main() {
	a := app.NewWithID("webcamctl")
	w := a.NewWindow("Webcam Controls (v4l2-ctl)")
	w.Resize(fyne.NewSize(980, 720))

	cfg, _ := config.Load()

	status := widget.NewLabel("")
	setStatus := func(s string) {
		status.SetText(s)
	}

	cameraSelect := widget.NewSelect([]string{}, nil)
	refreshBtn := widget.NewButton("Refresh cameras", nil)
	reloadCtrlsBtn := widget.NewButton("Reload controls", nil)
	applyNowBtn := widget.NewButton("Re-apply now", nil)

	topLeft := widget.NewLabel("Camera")
	topRight := container.NewHBox(refreshBtn, reloadCtrlsBtn, applyNowBtn)
	// Put the select in the center of a Border layout so it can grow wider.
	// This makes the drop-down popup wider too, while keeping the same MinSize.
	top := container.NewBorder(nil, nil, topLeft, topRight, cameraSelect)

	controlsHolder := container.NewMax(widget.NewLabel("Loading..."))
	w.SetContent(container.NewBorder(top, status, nil, nil, controlsHolder))

	var (
		curCancel      context.CancelFunc
		changeCh       = make(chan struct{}, 1)
		applyNowCh     = make(chan struct{}, 1)
		reloadCtrlsCh  = make(chan struct{}, 1)
		labelToDevice  = map[string]string{}
		deviceToLabel  = map[string]string{}
		curWorkerGuard sync.Mutex
	)

	signal := func(ch chan struct{}) {
		select {
		case ch <- struct{}{}:
		default:
		}
	}

	startWorker := func(device string, controls []v4l2.Control, desired *desiredState) {
		curWorkerGuard.Lock()
		defer curWorkerGuard.Unlock()

		if curCancel != nil {
			curCancel()
			curCancel = nil
		}

		ctx, cancel := context.WithCancel(context.Background())
		curCancel = cancel

		// Drain any stale signals from a previous worker.
	Drain:
		for {
			select {
			case <-applyNowCh:
			case <-reloadCtrlsCh:
			case <-changeCh:
			default:
				break Drain
			}
		}

		go func() {
			defer func() {
				// best-effort: avoid stale status on exit
			}()

			// Debounced apply on changes; periodic keepalive regardless.
			var debounceT *time.Timer
			var debounceC <-chan time.Time
			resetDebounce := func() {
				if debounceT == nil {
					debounceT = time.NewTimer(applyDebounce)
					debounceC = debounceT.C
					return
				}
				if !debounceT.Stop() {
					select {
					case <-debounceT.C:
					default:
					}
				}
				debounceT.Reset(applyDebounce)
				debounceC = debounceT.C
			}

			keepT := time.NewTicker(keepAlivePeriod)
			defer keepT.Stop()

			lastApplied := map[string]int64{}

			apply := func(force bool) {
				snap := desired.snapshot()
				if !force && v4l2.EqualInt64Map(snap, lastApplied) {
					return
				}

				autoChanged := false
				for k, v := range snap {
					if !v4l2.IsAutoLike(k) {
						continue
					}
					if pv, ok := lastApplied[k]; !ok || pv != v {
						autoChanged = true
						break
					}
				}

				// apply (bools + "auto" menus first) then the rest
				if err := v4l2.ApplyControls(ctx, device, controls, snap); err != nil {
					setStatus("Apply failed: " + err.Error())
					return
				}

				lastApplied = snap

				cfg.SetLastCamera(device)
				cfg.SetCameraControls(device, snap)
				if err := cfg.Save(); err != nil {
					setStatus("Saved, but config write failed: " + err.Error())
					return
				}
				setStatus("Applied & saved " + time.Now().Format("15:04:05"))

				if autoChanged {
					signal(reloadCtrlsCh)
				}
			}

			for {
				select {
				case <-ctx.Done():
					return
				case <-changeCh:
					resetDebounce()
				case <-debounceC:
					debounceC = nil
					apply(false)
				case <-applyNowCh:
					apply(true)
				case <-keepT.C:
					apply(true)
				case <-reloadCtrlsCh:
					// re-list controls; update UI ranges/active flags.
					ctx2, cancel := context.WithTimeout(ctx, 3*time.Second)
					newCtrls, err := v4l2.ListControls(ctx2, device)
					cancel()
					if err != nil {
						setStatus("Reload controls failed: " + err.Error())
						continue
					}
					// Ensure any newly-appearing controls get an initial value.
					snap := desired.snapshot()
					for _, c := range newCtrls {
						if _, ok := snap[c.Name]; !ok {
							desired.set(c.Name, c.Value)
						}
					}
					controls = newCtrls
					controlsHolder.Objects = []fyne.CanvasObject{buildControlsUI(newCtrls, desired, signal, changeCh)}
					controlsHolder.Refresh()
					setStatus("Controls reloaded")
				}
			}
		}()
	}

	loadCameras := func() {
		setStatus("Listing cameras via v4l2-ctl...")
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()

			cams, err := v4l2.ListCameras(ctx)
			if err != nil {
				setStatus("Camera listing failed: " + err.Error())
				dialog.ShowError(err, w)
				return
			}

			sort.Slice(cams, func(i, j int) bool { return cams[i].Label < cams[j].Label })

			lbls := make([]string, 0, len(cams))
			l2d := map[string]string{}
			d2l := map[string]string{}
			for _, c := range cams {
				lbls = append(lbls, c.Label)
				l2d[c.Label] = c.Device
				d2l[c.Device] = c.Label
			}

			labelToDevice = l2d
			deviceToLabel = d2l
			cameraSelect.Options = lbls
			cameraSelect.Refresh()
			setStatus(fmt.Sprintf("Found %d camera device(s)", len(lbls)))

			// select last-used if available
			want := cfg.LastCamera()
			if want != "" {
				if label, ok := deviceToLabel[want]; ok {
					cameraSelect.SetSelected(label)
					return
				}
			}
			if len(lbls) > 0 {
				cameraSelect.SetSelected(lbls[0])
			}
		}()
	}

	loadControlsForDevice := func(device string) {
		controlsHolder.Objects = []fyne.CanvasObject{widget.NewLabel("Loading controls...")}
		controlsHolder.Refresh()
		setStatus("Listing controls via v4l2-ctl...")

		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()

			ctrls, err := v4l2.ListControls(ctx, device)
			if err != nil {
				setStatus("Control listing failed: " + err.Error())
				dialog.ShowError(err, w)
				return
			}

			desired := newDesiredState()
			saved := cfg.CameraControls(device)
			for _, c := range ctrls {
				if v, ok := saved[c.Name]; ok {
					desired.set(c.Name, v)
				} else {
					desired.set(c.Name, c.Value)
				}
			}

			controlsHolder.Objects = []fyne.CanvasObject{buildControlsUI(ctrls, desired, signal, changeCh)}
			controlsHolder.Refresh()
			setStatus(fmt.Sprintf("Loaded %d controls for %s", len(ctrls), device))
			startWorker(device, ctrls, desired)
			signal(applyNowCh) // apply saved config immediately
		}()
	}

	cameraSelect.OnChanged = func(label string) {
		device, ok := labelToDevice[label]
		if !ok || device == "" {
			return
		}
		loadControlsForDevice(device)
	}

	refreshBtn.OnTapped = func() { loadCameras() }
	reloadCtrlsBtn.OnTapped = func() { signal(reloadCtrlsCh) }
	applyNowBtn.OnTapped = func() { signal(applyNowCh) }

	loadCameras()
	w.ShowAndRun()
}

func buildControlsUI(ctrls []v4l2.Control, desired *desiredState, signal func(chan struct{}), changeCh chan struct{}) fyne.CanvasObject {
	grouped := map[string][]v4l2.Control{}
	groupOrder := []string{}
	seen := map[string]bool{}
	for _, c := range ctrls {
		g := groupForControl(c.Name)
		if !seen[g] {
			seen[g] = true
			groupOrder = append(groupOrder, g)
		}
		grouped[g] = append(grouped[g], c)
	}
	sort.Strings(groupOrder)

	root := container.NewVBox()

	for _, g := range groupOrder {
		items := grouped[g]
		sort.Slice(items, func(i, j int) bool { return items[i].Name < items[j].Name })

		box := container.NewVBox()
		for _, c := range items {
			row := buildControlRow(c, desired, func(name string, v int64) {
				desired.set(name, v)
				signal(changeCh)
			})
			box.Add(row)
		}

		section := widget.NewCard(g, "", box)
		root.Add(section)
	}

	return container.NewVScroll(root)
}

func buildControlRow(c v4l2.Control, desired *desiredState, onChange func(name string, v int64)) fyne.CanvasObject {
	title := widget.NewLabel(c.Name)
	title.Alignment = fyne.TextAlignLeading
	title.Wrapping = fyne.TextWrapOff
	title.Truncation = fyne.TextTruncateEllipsis

	// Force a visible "name" column so Border layout can't collapse labels.
	const nameColWidth float32 = 320
	h := title.MinSize().Height
	if h < 24 {
		h = 24
	}
	titleCell := container.NewGridWrap(fyne.NewSize(nameColWidth, h), title)

	disabled := c.ReadOnly || c.Inactive

	switch c.Type {
	case "bool":
		ck := widget.NewCheck("", nil)
		init := desired.get(c.Name, c.Value) != 0
		ck.SetChecked(init)
		ck.OnChanged = func(b bool) {
			if b {
				onChange(c.Name, 1)
			} else {
				onChange(c.Name, 0)
			}
		}
		ck.Disable()
		if !disabled {
			ck.Enable()
		}
		return container.NewBorder(nil, nil, titleCell, nil, ck)

	case "menu":
		opts := make([]string, 0, len(c.Menu))
		for _, m := range c.Menu {
			opts = append(opts, fmt.Sprintf("%d: %s", m.Index, m.Label))
		}
		sel := widget.NewSelect(opts, nil)
		init := desired.get(c.Name, c.Value)
		cur := fmt.Sprintf("%d:", init)
		for _, o := range opts {
			if strings.HasPrefix(o, cur) {
				sel.SetSelected(o)
				break
			}
		}
		sel.OnChanged = func(s string) {
			i := strings.IndexByte(s, ':')
			if i <= 0 {
				return
			}
			var idx int
			_, err := fmt.Sscanf(s[:i], "%d", &idx)
			if err != nil {
				return
			}
			onChange(c.Name, int64(idx))
		}
		if disabled {
			sel.Disable()
		}
		return container.NewBorder(nil, nil, titleCell, nil, sel)

	case "button":
		btn := widget.NewButton("Run", func() {
			onChange(c.Name, 1)
		})
		if disabled {
			btn.Disable()
		}
		return container.NewBorder(nil, nil, titleCell, nil, btn)

	default: // int / int64 / others presented as slider if possible
		min := float64(c.Min)
		max := float64(c.Max)
		step := float64(c.Step)
		if step <= 0 {
			step = 1
		}

		init := desired.get(c.Name, c.Value)
		valLabel := widget.NewLabel(fmt.Sprintf("%d", init))
		s := widget.NewSlider(min, max)
		s.Step = step
		s.Value = float64(init)
		s.OnChanged = func(f float64) {
			// snap to step
			stepI := c.Step
			if stepI <= 0 {
				stepI = 1
			}
			vSteps := int64(math.Round((f - float64(c.Min)) / float64(stepI)))
			v := c.Min + vSteps*stepI
			if v < c.Min {
				v = c.Min
			}
			if v > c.Max {
				v = c.Max
			}
			valLabel.SetText(fmt.Sprintf("%d", v))
			onChange(c.Name, v)
		}
		if disabled {
			s.Disable()
		}

		return container.NewBorder(nil, nil, titleCell, valLabel, s)
	}
}

func groupForControl(name string) string {
	n := strings.ToLower(name)
	switch {
	case strings.Contains(n, "exposure"):
		return "Exposure"
	case strings.Contains(n, "white_balance") || strings.Contains(n, "temperature"):
		return "White balance"
	case strings.Contains(n, "focus"):
		return "Focus"
	case strings.Contains(n, "brightness") || strings.Contains(n, "contrast") || strings.Contains(n, "saturation") ||
		strings.Contains(n, "gamma") || strings.Contains(n, "sharpness") || strings.Contains(n, "gain") ||
		strings.Contains(n, "hue") || strings.Contains(n, "color"):
		return "Color"
	case strings.Contains(n, "pan") || strings.Contains(n, "tilt") || strings.Contains(n, "zoom"):
		return "Pan/Tilt/Zoom"
	case strings.Contains(n, "power_line_frequency") || strings.Contains(n, "backlight") || strings.Contains(n, "compensation"):
		return "Lighting"
	default:
		return "Other"
	}
}


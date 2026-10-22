package main

import (
	"image"
	"image/color"
	"strings"

	"gioui.org/io/key"
	"gioui.org/layout"
	"gioui.org/op"
	"gioui.org/op/clip"
	"gioui.org/op/paint"
	"gioui.org/widget"
	"gioui.org/widget/material"
)

// gsettings.go: the Settings overlay (gear button / Ctrl+,). Three sections
// (Drives, Content search, Search) edit a staged copy of the service
// settings plus local UI preferences. Apply writes only changed fields.

const (
	settingsTabDrives = iota
	settingsTabContent
	settingsTabSearch
)

var settingsTabLabels = [3]string{"Drives", "Content search", "Search"}

// localPrefs are the search preferences stored in native.json.
type localPrefs struct {
	ContentsMode bool
	PreviewOpen  bool
	View         string
	SortColumn   string
	SortAsc      bool
}

func prefsFromSettings(s Settings) localPrefs {
	return localPrefs{
		ContentsMode: s.ContentsMode,
		PreviewOpen:  s.PreviewOpen,
		View:         s.View,
		SortColumn:   s.SortColumn,
		SortAsc:      s.SortAsc,
	}
}

// settingsBlock records one laid-out block. Tests use it to prove that
// interactive blocks do not overlap.
type settingsBlock struct {
	Kind string
	Rect image.Rectangle
}

type settingsPanelState struct {
	model  *settingsModel
	prefs  localPrefs
	tab    int
	drive  string
	load   bool
	saving bool
	err    string
	notice string

	budgetCustom bool
	fieldErr     map[string]string

	blocks []settingsBlock
	cursor int
	scroll widget.List

	closeBtn  widget.Clickable
	cancelBtn widget.Clickable
	applyBtn  widget.Clickable
	undoBtn   widget.Clickable
	tabBtns   [3]widget.Clickable

	driveBools  []widget.Bool
	installBool widget.Bool
	// installService stages "install the seekfs Windows service" for Apply.
	installService bool
	// installing marks an Apply that included the service install.
	installing   bool
	driveChips   []widget.Clickable
	overrideBool widget.Bool
	enabledBool  widget.Bool
	modeBtns     [3]widget.Clickable
	addRootBtn   widget.Clickable
	addExclBtn   widget.Clickable
	rootDel      []widget.Clickable
	exclDel      []widget.Clickable
	gitBool      widget.Bool
	sysBool      widget.Bool
	noLimitCk    widget.Clickable
	customCk     widget.Clickable
	budgetEd     widget.Editor
	extsEd       widget.Editor
	contentsBool widget.Bool
	previewBool  widget.Bool
	viewBtns     [3]widget.Clickable
	sortBtns     [5]widget.Clickable
	sortAscBool  widget.Bool
}

// settingsPendingResult carries a settings-get/set outcome from the
// background client goroutine to the UI goroutine.
type settingsPendingResult struct {
	set  bool
	resp serviceResponse
	err  error
}

func newSettingsPanelState(s Settings) *settingsPanelState {
	p := &settingsPanelState{
		prefs:    prefsFromSettings(s),
		fieldErr: map[string]string{},
	}
	p.scroll.Axis = layout.Vertical
	p.budgetEd.SingleLine = true
	p.extsEd.SingleLine = true
	return p
}

// openSettings shows the panel and loads a fresh snapshot.
func (u *UI) openSettings() {
	if u.settingsOpen {
		return
	}
	u.settingsOpen = true
	if u.gsettings == nil {
		u.gsettings = newSettingsPanelState(u.settings)
	}
	p := u.gsettings
	p.load = p.model == nil
	p.err = ""
	p.fieldErr = map[string]string{}
	p.prefs = prefsFromSettings(u.settings)
	if p.model != nil {
		p.syncEditors()
	}
	go u.fetchSettings()
}

// openSettingsTab opens Settings on a specific section (for example Drives
// from the first-run panel).
func (u *UI) openSettingsTab(tab int) {
	if tab < 0 || tab > settingsTabSearch {
		tab = settingsTabDrives
	}
	if u.settingsOpen {
		if u.gsettings != nil {
			u.gsettings.tab = tab
		}
		return
	}
	u.openSettings()
	if u.gsettings != nil {
		u.gsettings.tab = tab
	}
}

// closeSettings hides the panel and discards staged edits.
func (u *UI) closeSettings() {
	u.settingsOpen = false
	if u.gsettings != nil {
		u.gsettings.fieldErr = map[string]string{}
		u.gsettings.err = ""
		u.gsettings.notice = ""
	}
}

func (u *UI) fetchSettings() {
	uiautoMark("settings-fetch-start")
	resp, err := u.client.SettingsGet()
	u.mu.Lock()
	u.settingsPending = settingsPendingResult{set: true, resp: resp, err: err}
	u.mu.Unlock()
	uiautoMark("settings-fetch-finish")
	u.invalidate()
}

// consumeSettingsPending applies a background result on the UI goroutine.
func (u *UI) consumeSettingsPending() {
	u.mu.Lock()
	pend := u.settingsPending
	u.settingsPending = settingsPendingResult{}
	u.mu.Unlock()
	if !pend.set || u.gsettings == nil {
		return
	}
	p := u.gsettings
	wasSaving := p.saving
	installing := p.installing
	p.load = false
	p.saving = false
	p.installing = false
	if wasSaving {
		uiautoMark("settings-apply-finish")
	}
	if pend.err != nil {
		p.err = pend.err.Error()
		return
	}
	if pend.resp.Settings != nil {
		p.model = newSettingsModel(*pend.resp.Settings)
		if p.drive != "" && p.model.drive(p.drive) == nil {
			p.drive = ""
		}
		p.budgetCustom = p.model.contentValues(p.drive).BudgetBytes > 0
		p.installService = p.model.offerServiceInstallByDefault()
		p.syncEditors()
	}
	if !pend.resp.OK {
		p.err = pend.resp.Message
		if f := fieldKeyForMessage(pend.resp.Message); f != "" {
			p.fieldErr[f] = pend.resp.Message
		}
		return
	}
	p.err = ""
	if wasSaving {
		p.notice = indexingNotice(pend.resp.Settings)
		if installing && p.model != nil && p.model.serviceInstalled() {
			p.notice = "Installed the seekfs service. " + p.notice
		}
		if strings.HasPrefix(p.notice, "Settings saved. Indexing") {
			u.mu.Lock()
			u.flashMsg = p.notice
			u.mu.Unlock()
		}
	} else {
		p.notice = ""
	}
}

func indexingNotice(s *serviceSettings) string {
	if s == nil {
		return "Settings saved"
	}
	var indexing []string
	for _, v := range s.Volumes {
		if v.State == "indexing" {
			indexing = append(indexing, v.Volume)
		}
	}
	if len(indexing) > 0 {
		return "Settings saved. Indexing " + strings.Join(indexing, ", ") + " in the background."
	}
	return "Settings saved"
}

func fieldKeyForMessage(msg string) string {
	m := strings.ToLower(msg)
	switch {
	case strings.Contains(m, "budget"):
		return "budget"
	case strings.Contains(m, "ext"):
		return "exts"
	case strings.Contains(m, "root"):
		return "roots"
	case strings.Contains(m, "exclude"):
		return "exclude"
	case strings.Contains(m, "mode"):
		return "mode"
	case strings.Contains(m, "enabled"):
		return "enabled"
	case strings.Contains(m, "volume"), strings.Contains(m, "indexed"):
		return "drives"
	default:
		return "general"
	}
}

func (p *settingsPanelState) syncEditors() {
	if p.model == nil {
		return
	}
	v := p.model.contentValues(p.drive)
	p.budgetEd.SetText(formatBudgetGB(v.BudgetBytes))
	p.extsEd.SetText(formatExtList(v.Exts))
	p.budgetCustom = v.BudgetBytes > 0
}

// applyLocalPrefs writes the Search section into native.json.
func (u *UI) applyLocalPrefs(p localPrefs) {
	u.mu.Lock()
	u.settings.ContentsMode = p.ContentsMode
	u.settings.PreviewOpen = p.PreviewOpen
	u.settings.View = p.View
	u.settings.SortColumn = p.SortColumn
	u.settings.SortAsc = p.SortAsc
	u.contents = p.ContentsMode
	u.view = viewModeFromString(p.View)
	u.sortKey = sortKeyFromString(p.SortColumn)
	u.sortAsc = p.SortAsc
	u.sortLocked()
	u.mu.Unlock()
	u.saveSoon()
}

// applySettings validates the staged editors and sends the patch.
func (u *UI) applySettings() {
	p := u.gsettings
	if p == nil || p.model == nil || p.saving {
		return
	}
	p.fieldErr = map[string]string{}
	p.err = ""
	budget := int64(0)
	if p.budgetCustom {
		v, err := parseBudgetGB(p.budgetEd.Text())
		if err != nil {
			p.fieldErr["budget"] = err.Error()
			return
		}
		budget = v
	}
	exts, err := parseExtList(p.extsEd.Text())
	if err != nil {
		p.fieldErr["exts"] = err.Error()
		return
	}
	current := p.model.contentValues(p.drive)
	if budget != current.BudgetBytes {
		p.model.setBudget(p.drive, budget)
	}
	if !equalStrings(exts, current.Exts) {
		p.model.setExts(p.drive, exts)
	}
	patch := p.model.patch()
	u.applyLocalPrefs(p.prefs)
	install := p.installService && !p.model.serviceInstalled()
	if patch == nil && !install {
		p.notice = "No changes to apply"
		return
	}
	p.saving = true
	p.installing = install
	p.notice = ""
	uiautoMark("settings-apply-start")
	canWrite := p.model.canWrite
	go u.applySettingsAsync(patch, canWrite, install)
}

func (u *UI) applySettingsAsync(patch *serviceSettings, canWrite, installService bool) {
	if installService {
		// Installing the service needs the elevated helper; it applies the
		// staged changes after the service is up.
		if err := runElevatedSettings(patch, u.client.PipeName, true); err != nil {
			u.postSettings(serviceResponse{}, err)
			return
		}
		refreshed, gerr := u.client.SettingsGet()
		u.postSettings(refreshed, gerr)
		return
	}
	resp, err := u.client.SettingsSet(*patch)
	if err == nil && resp.OK {
		u.postSettings(resp, nil)
		return
	}
	if err == nil && elevationDecision(canWrite, &resp) {
		if eerr := runElevatedSettings(patch, u.client.PipeName, false); eerr != nil {
			u.postSettings(serviceResponse{}, eerr)
			return
		}
		refreshed, gerr := u.client.SettingsGet()
		u.postSettings(refreshed, gerr)
		return
	}
	if err == nil {
		err = errorFromMessage(resp.Message)
	}
	u.postSettings(serviceResponse{}, err)
}

func (u *UI) postSettings(resp serviceResponse, err error) {
	u.mu.Lock()
	u.settingsPending = settingsPendingResult{set: true, resp: resp, err: err}
	u.mu.Unlock()
	u.invalidate()
}

func errorFromMessage(msg string) error {
	if strings.TrimSpace(msg) == "" {
		return errSettingsFailed
	}
	return &settingsError{msg: msg}
}

type settingsError struct{ msg string }

func (e *settingsError) Error() string { return e.msg }

var errSettingsFailed = &settingsError{msg: "settings-set failed"}

// ---- overlay layout ----

func (u *UI) layoutSettingsOverlay(gtx layout.Context) layout.Dimensions {
	size := gtx.Constraints.Max
	defer clip.Rect{Max: size}.Push(gtx.Ops).Pop()
	paint.ColorOp{Color: color.NRGBA{A: 0x66}}.Add(gtx.Ops)
	paint.PaintOp{}.Add(gtx.Ops)
	return layout.Stack{Alignment: layout.Center}.Layout(gtx,
		layout.Expanded(func(gtx layout.Context) layout.Dimensions {
			return layout.Dimensions{Size: gtx.Constraints.Max}
		}),
		layout.Stacked(func(gtx layout.Context) layout.Dimensions {
			return u.layoutSettingsCard(gtx)
		}),
	)
}

func (u *UI) layoutSettingsCard(gtx layout.Context) layout.Dimensions {
	p := u.gsettings
	if p == nil {
		return layout.Dimensions{}
	}
	maxW := gtx.Dp(Dp(820))
	maxH := gtx.Dp(Dp(660))
	size := image.Pt(maxW, maxH)
	if gtx.Constraints.Max.X < size.X {
		size.X = gtx.Constraints.Max.X
	}
	if gtx.Constraints.Max.Y < size.Y {
		size.Y = gtx.Constraints.Max.Y
	}
	if size.X < 320 {
		size.X = 320
	}
	if size.Y < 260 {
		size.Y = 260
	}
	gtx.Constraints.Min = size
	gtx.Constraints.Max = size
	s := u.scheme

	if u.handleSettingsKeys(gtx) {
		return layout.Dimensions{Size: size}
	}
	if !u.settingsOpen {
		return layout.Dimensions{Size: size}
	}

	mac := op.Record(gtx.Ops)
	dims := layout.Flex{Axis: layout.Vertical}.Layout(gtx,
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return u.layoutSettingsHeader(gtx, p)
		}),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return u.layoutSettingsTabs(gtx, p)
		}),
		layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
			return layout.Inset{Left: Dp(20), Right: Dp(20), Top: Dp(4)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				return u.layoutSettingsBody(gtx, p)
			})
		}),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return u.layoutSettingsFooter(gtx, p)
		}),
	)
	call := mac.Stop()
	gtx.Constraints.Min = size
	gtx.Constraints.Max = size
	roundedFill(gtx, s.Panel, size, Dp(12))
	call.Add(gtx.Ops)
	border(gtx, size, s.Border, Dp(12), Dp(1))
	return dims
}

// settingsHeaderSubtitle reports the truthful state line under the Settings
// title: only an in-flight request or an actually indexing volume qualifies.
func settingsHeaderSubtitle(p *settingsPanelState) string {
	if p.saving {
		return "Applying..."
	}
	if p.load {
		return "Loading..."
	}
	if p.model != nil {
		for _, v := range p.model.draft.Volumes {
			if v.State == "indexing" {
				return "Indexing..."
			}
		}
	}
	return ""
}

func (u *UI) layoutSettingsHeader(gtx layout.Context, p *settingsPanelState) layout.Dimensions {
	s := u.scheme
	if p.closeBtn.Clicked(gtx) {
		u.closeSettings()
	}
	return layout.Inset{Left: Dp(20), Right: Dp(12), Top: Dp(12), Bottom: Dp(8)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
		return layout.Flex{Axis: layout.Horizontal, Alignment: layout.Middle}.Layout(gtx,
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				return ficon(u.th, Sp(20), iconSettings, s.Accent).Layout(gtx)
			}),
			layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
				return layout.Inset{Left: Dp(10)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
					return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
						layout.Rigid(func(gtx layout.Context) layout.Dimensions {
							return label(u.th, Sp(18), "Settings", s.Text).Layout(gtx)
						}),
						layout.Rigid(func(gtx layout.Context) layout.Dimensions {
							subtitle := settingsHeaderSubtitle(p)
							if subtitle == "" {
								return layout.Dimensions{}
							}
							return label(u.th, Sp(12), subtitle, s.Muted).Layout(gtx)
						}),
					)
				})
			}),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				return fluentBtn(gtx, u.th, s, &p.closeBtn, iconClear, true, false, Dp(32))
			}),
		)
	})
}

func (u *UI) layoutSettingsTabs(gtx layout.Context, p *settingsPanelState) layout.Dimensions {
	s := u.scheme
	for i := range p.tabBtns {
		if p.tabBtns[i].Clicked(gtx) {
			p.tab = i
		}
	}
	dims := layout.Inset{Left: Dp(20), Right: Dp(20), Bottom: Dp(8)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
		return layout.Flex{Axis: layout.Horizontal, Alignment: layout.Middle}.Layout(gtx,
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				return chip(gtx, u.th, s, &p.tabBtns[0], settingsTabLabels[0], p.tab == 0)
			}),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				return layout.Inset{Left: Dp(8)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
					return chip(gtx, u.th, s, &p.tabBtns[1], settingsTabLabels[1], p.tab == 1)
				})
			}),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				return layout.Inset{Left: Dp(8)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
					return chip(gtx, u.th, s, &p.tabBtns[2], settingsTabLabels[2], p.tab == 2)
				})
			}),
		)
	})
	return layout.Stack{}.Layout(gtx,
		layout.Stacked(func(gtx layout.Context) layout.Dimensions { return dims }),
		layout.Stacked(func(gtx layout.Context) layout.Dimensions {
			return layout.S.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				return hairline(gtx, s.Border)
			})
		}),
	)
}

func (u *UI) layoutSettingsBody(gtx layout.Context, p *settingsPanelState) layout.Dimensions {
	if p.load && p.model == nil {
		return layout.Center.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
			return label(u.th, Sp(14), "Loading settings...", u.scheme.Muted).Layout(gtx)
		})
	}
	if p.model == nil {
		msg := firstNonEmpty(p.err, "Settings are unavailable")
		return layout.Center.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
			return label(u.th, Sp(14), msg, u.scheme.Error).Layout(gtx)
		})
	}
	p.blocks = p.blocks[:0]
	p.cursor = 0
	return material.List(u.th, &p.scroll).Layout(gtx, 1, func(gtx layout.Context, _ int) layout.Dimensions {
		var d layout.Dimensions
		switch p.tab {
		case settingsTabDrives:
			d = u.layoutSettingsDrives(gtx, p)
		case settingsTabContent:
			d = u.layoutSettingsContent(gtx, p)
		default:
			d = u.layoutSettingsSearch(gtx, p)
		}
		return layout.Dimensions{Size: image.Pt(gtx.Constraints.Max.X, d.Size.Y)}
	})
}

// block lays out one block at the current vertical cursor and records its
// rectangle for the geometry tests.
func (p *settingsPanelState) block(gtx layout.Context, kind string, fn layout.Widget) layout.Dimensions {
	width := gtx.Constraints.Max.X
	gtx.Constraints.Min = image.Point{}
	mac := op.Record(gtx.Ops)
	d := fn(gtx)
	call := mac.Stop()
	off := op.Offset(image.Pt(0, p.cursor)).Push(gtx.Ops)
	call.Add(gtx.Ops)
	off.Pop()
	h := d.Size.Y
	p.blocks = append(p.blocks, settingsBlock{Kind: kind, Rect: image.Rect(0, p.cursor, width, p.cursor+h)})
	p.cursor += h + gtx.Dp(Dp(8))
	return layout.Dimensions{Size: image.Pt(width, h)}
}

func (u *UI) fieldError(gtx layout.Context, p *settingsPanelState, key string) layout.Dimensions {
	msg := p.fieldErr[key]
	if msg == "" {
		return layout.Dimensions{}
	}
	return layout.Inset{Top: Dp(3)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
		return label(u.th, Sp(11), msg, u.scheme.Error).Layout(gtx)
	})
}

func (u *UI) settingsHeading(gtx layout.Context, title, subtitle string) layout.Dimensions {
	s := u.scheme
	return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return label(u.th, Sp(15), title, s.Text).Layout(gtx)
		}),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			if subtitle == "" {
				return layout.Dimensions{}
			}
			return layout.Inset{Top: Dp(2)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				return label(u.th, Sp(12), subtitle, s.Muted).Layout(gtx)
			})
		}),
	)
}

// ---- Drives ----

func (u *UI) layoutSettingsDrives(gtx layout.Context, p *settingsPanelState) layout.Dimensions {
	s := u.scheme
	vols := p.model.draft.Volumes
	for len(p.driveBools) < len(vols) {
		p.driveBools = append(p.driveBools, widget.Bool{})
	}
	p.block(gtx, "drives-heading", func(gtx layout.Context) layout.Dimensions {
		return u.settingsHeading(gtx, "Drives",
			"Choose the volumes the service indexes. New drives start indexing after Apply.")
	})
	p.block(gtx, "drives-service", func(gtx layout.Context) layout.Dimensions {
		return u.layoutServiceInstall(gtx, p)
	})
	if p.fieldErr["drives"] != "" {
		p.block(gtx, "drives-error", func(gtx layout.Context) layout.Dimensions {
			return u.fieldError(gtx, p, "drives")
		})
	}
	if len(vols) == 0 {
		p.block(gtx, "drives-empty", func(gtx layout.Context) layout.Dimensions {
			return label(u.th, Sp(13), "The service reported no local volumes.", s.Muted).Layout(gtx)
		})
		return layout.Dimensions{Size: image.Pt(gtx.Constraints.Max.X, p.cursor)}
	}
	for i := range vols {
		i := i
		v := vols[i]
		clk := &p.driveBools[i]
		clk.Value = v.Indexed
		if clk.Update(gtx) {
			p.model.setDriveIndexed(v.Volume, clk.Value)
		}
		kind := "drive:" + v.Volume
		p.block(gtx, kind, func(gtx layout.Context) layout.Dimensions {
			return u.layoutDriveRow(gtx, p, v, clk)
		})
	}
	return layout.Dimensions{Size: image.Pt(gtx.Constraints.Max.X, p.cursor)}
}

// layoutServiceInstall shows whether the installed Windows service runs, or
// offers to install it. Without it the app's own service stops with the app
// session and cannot keep drives current after a restart.
func (u *UI) layoutServiceInstall(gtx layout.Context, p *settingsPanelState) layout.Dimensions {
	s := u.scheme
	if p.model.serviceInstalled() {
		return label(u.th, Sp(12), "seekfs runs as a Windows service: it starts with Windows and keeps indexed drives up to date.", s.Muted).Layout(gtx)
	}
	p.installBool.Value = p.installService
	if p.installBool.Update(gtx) {
		p.installService = p.installBool.Value
	}
	return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return material.CheckBox(u.th, &p.installBool, "Install the seekfs service (recommended)").Layout(gtx)
		}),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return layout.Inset{Left: Dp(32)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				l := label(u.th, Sp(11), "Starts with Windows and keeps indexed drives up to date. Apply asks for administrator approval once.", s.Muted)
				l.MaxLines = 2
				return l.Layout(gtx)
			})
		}),
	)
}

func (u *UI) layoutDriveRow(gtx layout.Context, p *settingsPanelState, v volumeDraft, clk *widget.Bool) layout.Dimensions {
	s := u.scheme
	state := v.State
	if state == "" {
		if v.Indexed {
			state = "ready"
		} else {
			state = "not_indexed"
		}
	}
	stateColor := s.Muted
	switch state {
	case "ready":
		stateColor = s.OK
	case "indexing":
		stateColor = s.Warning
	case "error":
		stateColor = s.Error
	}
	title := v.Volume
	if v.Label != "" {
		title += "  " + v.Label
	}
	if v.FS != "" {
		title += "  -  " + v.FS
	}
	return layout.Inset{Top: Dp(6), Bottom: Dp(6), Left: Dp(4), Right: Dp(4)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
		return layout.Flex{Axis: layout.Horizontal, Alignment: layout.Middle}.Layout(gtx,
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				return material.CheckBox(u.th, clk, "Index this drive").Layout(gtx)
			}),
			layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
				return layout.Inset{Left: Dp(8), Right: Dp(12)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
					return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
						layout.Rigid(func(gtx layout.Context) layout.Dimensions {
							return label(u.th, Sp(13), title, s.Text).Layout(gtx)
						}),
						layout.Rigid(func(gtx layout.Context) layout.Dimensions {
							return layout.Inset{Top: Dp(4)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
								return u.usageBar(gtx, v.TotalBytes, v.FreeBytes)
							})
						}),
						layout.Rigid(func(gtx layout.Context) layout.Dimensions {
							free := ""
							if v.TotalBytes > 0 {
								free = formatBytesCompact(v.FreeBytes) + " free of " + formatBytesCompact(v.TotalBytes)
							}
							return layout.Inset{Top: Dp(2)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
								return label(u.th, Sp(11), free, s.Muted).Layout(gtx)
							})
						}),
						layout.Rigid(func(gtx layout.Context) layout.Dimensions {
							// The service's build failure reason; without it
							// the row only says "Error".
							if state != "error" || v.Error == "" {
								return layout.Dimensions{}
							}
							return layout.Inset{Top: Dp(2)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
								l := label(u.th, Sp(11), v.Error, s.Error)
								l.MaxLines = 3
								return l.Layout(gtx)
							})
						}),
					)
				})
			}),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
					layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						return label(u.th, Sp(12), stateLabel(state), stateColor).Layout(gtx)
					}),
					layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						return label(u.th, Sp(11), formatCount(int(v.Entries))+" entries", s.Muted).Layout(gtx)
					}),
				)
			}),
		)
	})
}

func stateLabel(state string) string {
	switch state {
	case "ready":
		return "Indexed"
	case "indexing":
		return "Indexing..."
	case "error":
		return "Error"
	case "not_indexed":
		return "Not indexed"
	default:
		return state
	}
}

func (u *UI) usageBar(gtx layout.Context, total, free int64) layout.Dimensions {
	s := u.scheme
	w := gtx.Constraints.Max.X
	h := gtx.Dp(Dp(6))
	if w <= 0 {
		w = gtx.Dp(Dp(120))
	}
	gtx.Constraints.Min = image.Pt(w, h)
	gtx.Constraints.Max = image.Pt(w, h)
	roundedFill(gtx, s.Border, image.Pt(w, h), Dp(3))
	if total > 0 {
		used := total - free
		if used < 0 {
			used = 0
		}
		if used > total {
			used = total
		}
		fw := int(float64(w) * float64(used) / float64(total))
		if fw > 0 {
			defer clip.RRect{Rect: image.Rect(0, 0, fw, h), SE: gtx.Dp(Dp(3)), SW: gtx.Dp(Dp(3)), NW: gtx.Dp(Dp(3)), NE: gtx.Dp(Dp(3))}.Push(gtx.Ops).Pop()
			paint.ColorOp{Color: s.Accent}.Add(gtx.Ops)
			paint.PaintOp{}.Add(gtx.Ops)
		}
	}
	return layout.Dimensions{Size: image.Pt(w, h)}
}

// ---- Content search ----

func (u *UI) layoutSettingsContent(gtx layout.Context, p *settingsPanelState) layout.Dimensions {
	s := u.scheme
	p.block(gtx, "content-heading", func(gtx layout.Context) layout.Dimensions {
		return u.settingsHeading(gtx, "Content search",
			"Index file contents across Git worktrees and chosen folders.")
	})
	p.block(gtx, "content-drive-selector", func(gtx layout.Context) layout.Dimensions {
		return u.layoutContentDriveSelector(gtx, p)
	})
	if p.drive != "" {
		p.block(gtx, "content-override", func(gtx layout.Context) layout.Dimensions {
			return u.layoutOverrideToggle(gtx, p)
		})
		if !p.model.hasOverride(p.drive) {
			p.block(gtx, "content-inherit", func(gtx layout.Context) layout.Dimensions {
				return label(u.th, Sp(12), "This drive uses the All drives settings.", s.Muted).Layout(gtx)
			})
			return layout.Dimensions{Size: image.Pt(gtx.Constraints.Max.X, p.cursor)}
		}
	} else {
		p.block(gtx, "content-enabled", func(gtx layout.Context) layout.Dimensions {
			p.enabledBool.Value = p.model.draft.Content.Enabled
			if p.enabledBool.Update(gtx) {
				p.model.setContentEnabled(p.enabledBool.Value)
			}
			return material.CheckBox(u.th, &p.enabledBool, "Enable content search").Layout(gtx)
		})
	}

	v := p.model.contentValues(p.drive)
	if p.fieldErr["enabled"] != "" {
		p.block(gtx, "content-enabled-error", func(gtx layout.Context) layout.Dimensions {
			return u.fieldError(gtx, p, "enabled")
		})
	}
	p.block(gtx, "content-mode", func(gtx layout.Context) layout.Dimensions {
		return u.layoutContentMode(gtx, p, v.Mode)
	})
	p.block(gtx, "content-roots", func(gtx layout.Context) layout.Dimensions {
		return u.layoutPathList(gtx, p, "Folders", v.Roots, true)
	})
	p.block(gtx, "content-exclude", func(gtx layout.Context) layout.Dimensions {
		return u.layoutPathList(gtx, p, "Exclusions", v.Exclude, false)
	})
	p.block(gtx, "content-toggles", func(gtx layout.Context) layout.Dimensions {
		p.gitBool.Value = v.Git
		p.sysBool.Value = v.SystemExcludes
		if p.gitBool.Update(gtx) {
			p.model.setGit(p.drive, p.gitBool.Value)
		}
		if p.sysBool.Update(gtx) {
			p.model.setSystemExcludes(p.drive, p.sysBool.Value)
		}
		return layout.Flex{Axis: layout.Horizontal}.Layout(gtx,
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				return material.CheckBox(u.th, &p.gitBool, "Follow .gitignore").Layout(gtx)
			}),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				return layout.Inset{Left: Dp(20)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
					return material.CheckBox(u.th, &p.sysBool, "Skip system folders").Layout(gtx)
				})
			}),
		)
	})
	p.block(gtx, "content-budget", func(gtx layout.Context) layout.Dimensions {
		return u.layoutBudget(gtx, p)
	})
	p.block(gtx, "content-exts", func(gtx layout.Context) layout.Dimensions {
		return u.layoutExts(gtx, p)
	})
	return layout.Dimensions{Size: image.Pt(gtx.Constraints.Max.X, p.cursor)}
}

func (u *UI) layoutContentDriveSelector(gtx layout.Context, p *settingsPanelState) layout.Dimensions {
	s := u.scheme
	vols := p.model.draft.Volumes
	need := len(vols) + 1
	for len(p.driveChips) < need {
		p.driveChips = append(p.driveChips, widget.Clickable{})
	}
	if p.driveChips[0].Clicked(gtx) {
		p.drive = ""
		p.syncEditors()
	}
	for i := range vols {
		i := i
		if p.driveChips[i+1].Clicked(gtx) {
			p.drive = vols[i].Volume
			p.syncEditors()
		}
	}
	return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return label(u.th, Sp(12), "Settings apply to", s.Muted).Layout(gtx)
		}),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return layout.Inset{Top: Dp(6)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				children := make([]layout.FlexChild, 0, len(vols)+1)
				children = append(children, layout.Rigid(func(gtx layout.Context) layout.Dimensions {
					return chip(gtx, u.th, s, &p.driveChips[0], "All drives", p.drive == "")
				}))
				for i := range vols {
					i := i
					children = append(children, layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						return layout.Inset{Left: Dp(8)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
							return chip(gtx, u.th, s, &p.driveChips[i+1], vols[i].Volume, p.drive == vols[i].Volume)
						})
					}))
				}
				return layout.Flex{Axis: layout.Horizontal, Alignment: layout.Middle}.Layout(gtx, children...)
			})
		}),
	)
}

func (u *UI) layoutOverrideToggle(gtx layout.Context, p *settingsPanelState) layout.Dimensions {
	s := u.scheme
	p.overrideBool.Value = p.model.hasOverride(p.drive)
	if p.overrideBool.Update(gtx) {
		if p.overrideBool.Value {
			p.model.ensureOverride(p.drive)
		} else {
			p.model.removeOverride(p.drive)
		}
		p.syncEditors()
	}
	return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return material.CheckBox(u.th, &p.overrideBool,
				"Use separate content settings for "+p.drive).Layout(gtx)
		}),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return layout.Inset{Top: Dp(2)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				if p.model.hasOverride(p.drive) {
					return label(u.th, Sp(11), "Editing the per-drive override.", s.Muted).Layout(gtx)
				}
				return label(u.th, Sp(11), "Currently inheriting All drives.", s.Muted).Layout(gtx)
			})
		}),
	)
}

func (u *UI) layoutContentMode(gtx layout.Context, p *settingsPanelState, mode string) layout.Dimensions {
	s := u.scheme
	modes := [3]string{contentModeAuto, contentModeExplicit, contentModeOff}
	labels := [3]string{"Automatic", "Only chosen folders", "Off"}
	for i := range p.modeBtns {
		if p.modeBtns[i].Clicked(gtx) {
			p.model.setContentMode(p.drive, modes[i])
		}
	}
	return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return label(u.th, Sp(12), "Mode", s.Muted).Layout(gtx)
		}),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return layout.Inset{Top: Dp(6)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				return layout.Flex{Axis: layout.Horizontal, Alignment: layout.Middle}.Layout(gtx,
					layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						return chip(gtx, u.th, s, &p.modeBtns[0], labels[0], mode == modes[0])
					}),
					layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						return layout.Inset{Left: Dp(8)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
							return chip(gtx, u.th, s, &p.modeBtns[1], labels[1], mode == modes[1])
						})
					}),
					layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						return layout.Inset{Left: Dp(8)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
							return chip(gtx, u.th, s, &p.modeBtns[2], labels[2], mode == modes[2])
						})
					}),
				)
			})
		}),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			hint := "Git worktrees plus the chosen folders."
			if mode == contentModeExplicit {
				hint = "Only the folders listed below."
			} else if mode == contentModeOff {
				hint = "Content is not indexed for this scope."
			}
			return layout.Inset{Top: Dp(4)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				return label(u.th, Sp(11), hint, s.Muted).Layout(gtx)
			})
		}),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return u.fieldError(gtx, p, "mode")
		}),
	)
}

func (u *UI) layoutPathList(gtx layout.Context, p *settingsPanelState, title string, paths []string, roots bool) layout.Dimensions {
	s := u.scheme
	del := &p.rootDel
	addBtn := &p.addRootBtn
	if !roots {
		del = &p.exclDel
		addBtn = &p.addExclBtn
	}
	for len(*del) < len(paths) {
		*del = append(*del, widget.Clickable{})
	}
	remove := -1
	for i := range paths {
		i := i
		if (*del)[i].Clicked(gtx) {
			remove = i
		}
	}
	if remove >= 0 {
		if roots {
			p.model.removeRoot(p.drive, remove)
		} else {
			p.model.removeExclude(p.drive, remove)
		}
	}
	if addBtn.Clicked(gtx) {
		if path, err := u.shell.PickFolder(title); err == nil && path != "" {
			if roots {
				p.model.addRoot(p.drive, path)
			} else {
				p.model.addExclude(p.drive, path)
			}
		}
	}
	children := []layout.FlexChild{
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return layout.Flex{Axis: layout.Horizontal, Alignment: layout.Middle}.Layout(gtx,
				layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
					return label(u.th, Sp(12), title, s.Muted).Layout(gtx)
				}),
				layout.Rigid(func(gtx layout.Context) layout.Dimensions {
					return chip(gtx, u.th, s, addBtn, "Add folder...", false)
				}),
			)
		}),
	}
	for i := range paths {
		i := i
		children = append(children, layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return layout.Inset{Top: Dp(4)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				return layout.Flex{Axis: layout.Horizontal, Alignment: layout.Middle}.Layout(gtx,
					layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
						return label(u.th, Sp(12), paths[i], s.Text).Layout(gtx)
					}),
					layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						return fluentBtn(gtx, u.th, s, &(*del)[i], iconClear, true, false, Dp(24))
					}),
				)
			})
		}))
	}
	field := "roots"
	if !roots {
		field = "exclude"
	}
	if p.fieldErr[field] != "" {
		children = append(children, layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return u.fieldError(gtx, p, field)
		}))
	}
	return layout.Flex{Axis: layout.Vertical}.Layout(gtx, children...)
}

func (u *UI) layoutBudget(gtx layout.Context, p *settingsPanelState) layout.Dimensions {
	s := u.scheme
	if p.noLimitCk.Clicked(gtx) {
		p.budgetCustom = false
		p.model.setBudget(p.drive, 0)
		p.budgetEd.SetText("")
	}
	if p.customCk.Clicked(gtx) {
		p.budgetCustom = true
		gtx.Execute(key.FocusCmd{Tag: &p.budgetEd})
	}
	ed := material.Editor(u.th, &p.budgetEd, "GB")
	ed.Color = s.Text
	ed.HintColor = s.Muted
	ed.SelectionColor = s.Selected
	ed.TextSize = Sp(13)
	return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return label(u.th, Sp(12), "Size limit", s.Muted).Layout(gtx)
		}),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return layout.Inset{Top: Dp(6)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				return layout.Flex{Axis: layout.Horizontal, Alignment: layout.Middle}.Layout(gtx,
					layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						return chip(gtx, u.th, s, &p.noLimitCk, "No limit", !p.budgetCustom)
					}),
					layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						return layout.Inset{Left: Dp(8)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
							return chip(gtx, u.th, s, &p.customCk, "Custom", p.budgetCustom)
						})
					}),
					layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						if !p.budgetCustom {
							return layout.Dimensions{}
						}
						gtx.Constraints.Min.X = gtx.Dp(Dp(90))
						gtx.Constraints.Max.X = gtx.Dp(Dp(90))
						return layout.Inset{Left: Dp(8)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
							return u.roundedField(gtx, func(gtx layout.Context) layout.Dimensions {
								return layout.Inset{Left: Dp(8), Right: Dp(8), Top: Dp(5), Bottom: Dp(5)}.Layout(gtx, ed.Layout)
							}, gtx.Focused(&p.budgetEd))
						})
					}),
				)
			})
		}),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			if msg := p.fieldErr["budget"]; msg != "" {
				return layout.Inset{Top: Dp(3)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
					return label(u.th, Sp(11), msg, s.Error).Layout(gtx)
				})
			}
			return layout.Dimensions{}
		}),
	)
}

func (u *UI) layoutExts(gtx layout.Context, p *settingsPanelState) layout.Dimensions {
	s := u.scheme
	ed := material.Editor(u.th, &p.extsEd, "go, md, txt")
	ed.Color = s.Text
	ed.HintColor = s.Muted
	ed.SelectionColor = s.Selected
	ed.TextSize = Sp(13)
	gtx.Constraints.Min.X = 0
	return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return label(u.th, Sp(12), "Extensions (empty detects text automatically)", s.Muted).Layout(gtx)
		}),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return layout.Inset{Top: Dp(6)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				return u.roundedField(gtx, func(gtx layout.Context) layout.Dimensions {
					return layout.Inset{Left: Dp(10), Right: Dp(10), Top: Dp(7), Bottom: Dp(7)}.Layout(gtx, ed.Layout)
				}, gtx.Focused(&p.extsEd))
			})
		}),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			if msg := p.fieldErr["exts"]; msg != "" {
				return layout.Inset{Top: Dp(3)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
					return label(u.th, Sp(11), msg, s.Error).Layout(gtx)
				})
			}
			return layout.Dimensions{}
		}),
	)
}

// ---- Search ----

func (u *UI) layoutSettingsSearch(gtx layout.Context, p *settingsPanelState) layout.Dimensions {
	s := u.scheme
	views := []string{ViewDetails.String(), ViewIcons.String(), ViewGrouped.String()}
	sorts := []SortKey{SortName, SortFolder, SortSize, SortModified, SortType}
	p.block(gtx, "search-heading", func(gtx layout.Context) layout.Dimensions {
		return u.settingsHeading(gtx, "Search",
			"These preferences are stored in this app's native.json.")
	})
	p.block(gtx, "search-toggles", func(gtx layout.Context) layout.Dimensions {
		p.contentsBool.Value = p.prefs.ContentsMode
		p.previewBool.Value = p.prefs.PreviewOpen
		if p.contentsBool.Update(gtx) {
			p.prefs.ContentsMode = p.contentsBool.Value
		}
		if p.previewBool.Update(gtx) {
			p.prefs.PreviewOpen = p.previewBool.Value
		}
		return layout.Flex{Axis: layout.Horizontal}.Layout(gtx,
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				return material.CheckBox(u.th, &p.contentsBool, "Default to Contents mode").Layout(gtx)
			}),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				return layout.Inset{Left: Dp(20)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
					return material.CheckBox(u.th, &p.previewBool, "Show preview pane").Layout(gtx)
				})
			}),
		)
	})
	p.block(gtx, "search-view", func(gtx layout.Context) layout.Dimensions {
		labels := []string{"Details", "Icons", "Grouped"}
		for i := range p.viewBtns {
			if p.viewBtns[i].Clicked(gtx) {
				p.prefs.View = views[i]
			}
		}
		return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				return label(u.th, Sp(12), "View mode", s.Muted).Layout(gtx)
			}),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				return layout.Inset{Top: Dp(6)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
					return layout.Flex{Axis: layout.Horizontal}.Layout(gtx,
						layout.Rigid(func(gtx layout.Context) layout.Dimensions {
							return chip(gtx, u.th, s, &p.viewBtns[0], labels[0], p.prefs.View == views[0])
						}),
						layout.Rigid(func(gtx layout.Context) layout.Dimensions {
							return layout.Inset{Left: Dp(8)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
								return chip(gtx, u.th, s, &p.viewBtns[1], labels[1], p.prefs.View == views[1])
							})
						}),
						layout.Rigid(func(gtx layout.Context) layout.Dimensions {
							return layout.Inset{Left: Dp(8)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
								return chip(gtx, u.th, s, &p.viewBtns[2], labels[2], p.prefs.View == views[2])
							})
						}),
					)
				})
			}),
		)
	})
	p.block(gtx, "search-sort", func(gtx layout.Context) layout.Dimensions {
		for i := range p.sortBtns {
			if p.sortBtns[i].Clicked(gtx) {
				p.prefs.SortColumn = sorts[i].String()
			}
		}
		p.sortAscBool.Value = p.prefs.SortAsc
		if p.sortAscBool.Update(gtx) {
			p.prefs.SortAsc = p.sortAscBool.Value
		}
		return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				return label(u.th, Sp(12), "Default sort", s.Muted).Layout(gtx)
			}),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				return layout.Inset{Top: Dp(6)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
					return layout.Flex{Axis: layout.Horizontal, Alignment: layout.Middle}.Layout(gtx,
						layout.Rigid(func(gtx layout.Context) layout.Dimensions {
							return chip(gtx, u.th, s, &p.sortBtns[0], "Name", p.prefs.SortColumn == sorts[0].String())
						}),
						layout.Rigid(func(gtx layout.Context) layout.Dimensions {
							return layout.Inset{Left: Dp(8)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
								return chip(gtx, u.th, s, &p.sortBtns[1], "Folder", p.prefs.SortColumn == sorts[1].String())
							})
						}),
						layout.Rigid(func(gtx layout.Context) layout.Dimensions {
							return layout.Inset{Left: Dp(8)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
								return chip(gtx, u.th, s, &p.sortBtns[2], "Size", p.prefs.SortColumn == sorts[2].String())
							})
						}),
						layout.Rigid(func(gtx layout.Context) layout.Dimensions {
							return layout.Inset{Left: Dp(8)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
								return chip(gtx, u.th, s, &p.sortBtns[3], "Modified", p.prefs.SortColumn == sorts[3].String())
							})
						}),
						layout.Rigid(func(gtx layout.Context) layout.Dimensions {
							return layout.Inset{Left: Dp(8)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
								return chip(gtx, u.th, s, &p.sortBtns[4], "Type", p.prefs.SortColumn == sorts[4].String())
							})
						}),
						layout.Rigid(func(gtx layout.Context) layout.Dimensions {
							return layout.Inset{Left: Dp(16)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
								return material.CheckBox(u.th, &p.sortAscBool, "Ascending").Layout(gtx)
							})
						}),
					)
				})
			}),
		)
	})
	return layout.Dimensions{Size: image.Pt(gtx.Constraints.Max.X, p.cursor)}
}

// ---- footer ----

func (u *UI) layoutSettingsFooter(gtx layout.Context, p *settingsPanelState) layout.Dimensions {
	s := u.scheme
	if p.applyBtn.Clicked(gtx) {
		u.applySettings()
	}
	if p.cancelBtn.Clicked(gtx) {
		u.closeSettings()
	}
	if p.undoBtn.Clicked(gtx) {
		if p.model != nil && p.model.undo() {
			p.syncEditors()
			p.notice = ""
		}
	}
	notice := p.notice
	if p.err != "" {
		notice = p.err
	}
	if p.saving {
		notice = "Applying settings..."
	}
	noticeColor := s.Muted
	if p.err != "" {
		noticeColor = s.Error
	} else if p.notice != "" {
		noticeColor = s.OK
	}
	return layout.Stack{}.Layout(gtx,
		layout.Stacked(func(gtx layout.Context) layout.Dimensions {
			return layout.Inset{Left: Dp(20), Right: Dp(20), Top: Dp(10), Bottom: Dp(14)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				return layout.Flex{Axis: layout.Horizontal, Alignment: layout.Middle}.Layout(gtx,
					layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
						return label(u.th, Sp(12), notice, noticeColor).Layout(gtx)
					}),
					layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						return chip(gtx, u.th, s, &p.undoBtn, "Undo", false)
					}),
					layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						return layout.Inset{Left: Dp(8)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
							return chip(gtx, u.th, s, &p.cancelBtn, "Cancel", false)
						})
					}),
					layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						return layout.Inset{Left: Dp(8)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
							return chip(gtx, u.th, s, &p.applyBtn, "Apply", true)
						})
					}),
				)
			})
		}),
		layout.Stacked(func(gtx layout.Context) layout.Dimensions {
			return layout.N.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				return hairline(gtx, s.Border)
			})
		}),
	)
}

// handleSettingsKeys closes on Esc and applies on Enter.
func (u *UI) handleSettingsKeys(gtx layout.Context) bool {
	applied := false
	for {
		ev, ok := gtx.Event(key.Filter{Name: key.NameEscape})
		if !ok {
			break
		}
		if e, ok := ev.(key.Event); ok && e.State == key.Press {
			u.closeSettings()
			return true
		}
	}
	for {
		ev, ok := gtx.Event(key.Filter{Name: key.NameReturn})
		if !ok {
			break
		}
		if e, ok := ev.(key.Event); ok && e.State == key.Press {
			u.applySettings()
			applied = true
		}
	}
	return applied
}

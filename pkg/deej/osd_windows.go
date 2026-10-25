package deej

import (
	"fmt"
	"math"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"unsafe"

	"github.com/lxn/win"
	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

// layout, in 96-dpi pixels
const (
	osdCardWidth   = 300
	osdCardHeight  = 64
	osdRadius      = 8
	osdShadow      = 16 // room around the card for the drop shadow
	osdScreenInset = 24 // distance between the card and the work area edge
	osdPad         = 16
	osdIconSize    = 28
	osdPctWidth    = 44
	osdBarY        = 44
	osdBarHeight   = 4
	osdThumbRadius = 7
	osdThumbDot    = 4

	osdTimeoutMs = 1500
	osdFadeMs    = 15
	osdFadeStep  = 40 // alpha per fade tick

	osdShowMsg = win.WM_APP + 1
	osdHideID  = 1
	osdFadeID  = 2
)

var (
	user32                  = syscall.NewLazyDLL("user32.dll")
	procUpdateLayeredWindow = user32.NewProc("UpdateLayeredWindow")

	osdOnce   sync.Once
	osdHwnd   win.HWND
	osdAlpha  int32 // current fade level, only touched on the window thread
	osdFadeIn bool

	osdLock     sync.Mutex
	osdLabel    string
	osdValue    float32
	osdPosition string
	osdMic      bool
	osdExe      string

	osdLogos = map[string]win.HICON{} // "path|size" -> icon, only touched on the window thread
)

type osdTheme struct {
	bg, border, text, track, thumb, accent rgb
}

type rgb struct{ r, g, b float64 }

func (c rgb) colorref() win.COLORREF { return win.RGB(byte(c.r), byte(c.g), byte(c.b)) }

func (c rgb) lerp(o rgb, t float64) rgb {
	return rgb{c.r + (o.r-c.r)*t, c.g + (o.g-c.g)*t, c.b + (o.b-c.b)*t}
}

// showVolumeOSD shows a small always-on-top, non-activating popup with the given label and volume
func showVolumeOSD(label string, value float32, position string, mic bool, sessions []Session) {
	exe := osdSessionsExe(sessions)

	osdOnce.Do(func() {
		ready := make(chan struct{})
		go osdLoop(ready)
		<-ready
	})

	osdLock.Lock()
	osdLabel, osdValue, osdPosition, osdMic, osdExe = label, value, position, mic, exe
	osdLock.Unlock()

	if osdHwnd != 0 {
		win.PostMessage(osdHwnd, osdShowMsg, 0, 0)
	}
}

// osdLoop owns the popup window; win32 windows must be driven from the thread that created them
func osdLoop(ready chan struct{}) {
	runtime.LockOSThread()

	className, _ := syscall.UTF16PtrFromString("deejVolumeOSD")
	hInstance := win.GetModuleHandle(nil)

	wc := win.WNDCLASSEX{
		LpfnWndProc:   syscall.NewCallback(osdWndProc),
		HInstance:     hInstance,
		LpszClassName: className,
	}
	wc.CbSize = uint32(unsafe.Sizeof(wc))
	win.RegisterClassEx(&wc)

	osdHwnd = win.CreateWindowEx(
		win.WS_EX_TOPMOST|win.WS_EX_TOOLWINDOW|win.WS_EX_NOACTIVATE|win.WS_EX_LAYERED|win.WS_EX_TRANSPARENT,
		className, nil, win.WS_POPUP, 0, 0, 0, 0, 0, 0, hInstance, nil)

	close(ready)

	var msg win.MSG
	for win.GetMessage(&msg, 0, 0, 0) > 0 {
		win.TranslateMessage(&msg)
		win.DispatchMessage(&msg)
	}
}

func osdWndProc(hwnd win.HWND, msg uint32, wParam, lParam uintptr) uintptr {
	switch msg {
	case osdShowMsg:
		osdRender(hwnd)
		win.SetWindowPos(hwnd, win.HWND_TOPMOST, 0, 0, 0, 0,
			win.SWP_NOMOVE|win.SWP_NOSIZE|win.SWP_NOACTIVATE|win.SWP_SHOWWINDOW)
		osdFadeIn = true
		win.SetTimer(hwnd, osdFadeID, osdFadeMs, 0)
		win.SetTimer(hwnd, osdHideID, osdTimeoutMs, 0) // restarts the countdown
		return 0

	case win.WM_TIMER:
		switch wParam {
		case osdHideID:
			win.KillTimer(hwnd, osdHideID)
			osdFadeIn = false
			win.SetTimer(hwnd, osdFadeID, osdFadeMs, 0)

		case osdFadeID:
			if osdFadeIn {
				osdAlpha = min(osdAlpha+osdFadeStep, 255)
			} else {
				osdAlpha = max(osdAlpha-osdFadeStep, 0)
			}
			osdSetAlpha(hwnd)

			if osdAlpha == 0 {
				win.ShowWindow(hwnd, win.SW_HIDE)
			}
			if osdAlpha == 0 || osdAlpha == 255 {
				win.KillTimer(hwnd, osdFadeID)
			}
		}
		return 0
	}

	return win.DefWindowProc(hwnd, msg, wParam, lParam)
}

func osdBlend() win.BLENDFUNCTION {
	return win.BLENDFUNCTION{SourceConstantAlpha: byte(osdAlpha), AlphaFormat: win.AC_SRC_ALPHA}
}

// osdSetAlpha changes the overall opacity without touching the window contents
func osdSetAlpha(hwnd win.HWND) {
	blend := osdBlend()
	procUpdateLayeredWindow.Call(uintptr(hwnd), 0, 0, 0, 0, 0, 0, uintptr(unsafe.Pointer(&blend)), 2 /* ULW_ALPHA */)
}

// osdRender draws the popup into a 32-bit bitmap with per-pixel alpha (rounded corners, shadow)
// and hands it to the layered window
func osdRender(hwnd win.HWND) {
	osdLock.Lock()
	label, value, position, mic, exe := osdLabel, float64(osdValue), osdPosition, osdMic, osdExe
	osdLock.Unlock()

	theme := osdCurrentTheme()

	// everything below is in physical pixels
	scale := float64(win.GetDpiForWindow(hwnd)) / 96
	px := func(v float64) int32 { return int32(math.Round(v * scale)) }

	cardW, cardH, shadow := px(osdCardWidth), px(osdCardHeight), px(osdShadow)
	w, h := cardW+2*shadow, cardH+2*shadow

	// bitmap setup
	screenDC := win.GetDC(0)
	defer win.ReleaseDC(0, screenDC)
	memDC := win.CreateCompatibleDC(screenDC)
	defer win.DeleteDC(memDC)

	bmi := win.BITMAPINFOHEADER{BiWidth: w, BiHeight: -h, BiPlanes: 1, BiBitCount: 32, BiCompression: win.BI_RGB}
	bmi.BiSize = uint32(unsafe.Sizeof(bmi))
	var bits unsafe.Pointer
	bitmap := win.CreateDIBSection(memDC, &bmi, win.DIB_RGB_COLORS, &bits, 0, 0)
	if bitmap == 0 {
		return
	}
	defer win.DeleteObject(win.HGDIOBJ(bitmap))
	win.SelectObject(memDC, win.HGDIOBJ(bitmap))

	pixels := unsafe.Slice((*uint32)(bits), int(w*h))
	get := func(i int) rgb {
		p := pixels[i]
		return rgb{float64(p >> 16 & 0xff), float64(p >> 8 & 0xff), float64(p & 0xff)}
	}
	set := func(i int, c rgb) {
		pixels[i] = uint32(c.r+0.5)<<16 | uint32(c.g+0.5)<<8 | uint32(c.b+0.5)
	}

	// card background + hairline border
	card := sdfRoundRect(float64(shadow), float64(shadow), float64(cardW), float64(cardH), float64(osdRadius)*scale)
	for y := int32(0); y < h; y++ {
		for x := int32(0); x < w; x++ {
			d := card(float64(x)+0.5, float64(y)+0.5)
			set(int(y*w+x), theme.border.lerp(theme.bg, coverage(d+scale)))
		}
	}

	// anti-aliased shapes painted straight into the bitmap
	paint := func(sdf func(x, y float64) float64, color rgb, x0, y0, x1, y1 float64) {
		for y := int32(max(y0, 0)); y < int32(math.Min(y1+1, float64(h))); y++ {
			for x := int32(max(x0, 0)); x < int32(math.Min(x1+1, float64(w))); x++ {
				if cov := coverage(sdf(float64(x)+0.5, float64(y)+0.5)); cov > 0 {
					i := int(y*w + x)
					set(i, get(i).lerp(color, cov))
				}
			}
		}
	}

	cardX, cardY := float64(shadow), float64(shadow)
	barX0 := cardX + float64(px(osdPad+osdIconSize+12+osdThumbRadius))
	barX1 := cardX + float64(cardW-px(osdPad+osdPctWidth+8+osdThumbRadius))
	barY := cardY + float64(osdBarY)*scale
	barH := float64(osdBarHeight) * scale
	thumbX := barX0 + (barX1-barX0)*value
	thumbR, dotR := float64(osdThumbRadius)*scale, float64(osdThumbDot)*scale

	paint(sdfRoundRect(barX0-barH/2, barY-barH/2, barX1-barX0+barH, barH, barH/2), theme.track, barX0-barH, barY-barH, barX1+barH, barY+barH)
	paint(sdfRoundRect(barX0-barH/2, barY-barH/2, thumbX-barX0+barH, barH, barH/2), theme.accent, barX0-barH, barY-barH, thumbX+barH, barY+barH)
	paint(sdfCircle(thumbX, barY, thumbR+scale), theme.border, thumbX-thumbR-2*scale, barY-thumbR-2*scale, thumbX+thumbR+2*scale, barY+thumbR+2*scale)
	paint(sdfCircle(thumbX, barY, thumbR), theme.thumb, thumbX-thumbR-scale, barY-thumbR-scale, thumbX+thumbR+scale, barY+thumbR+scale)
	paint(sdfCircle(thumbX, barY, dotR), theme.accent, thumbX-dotR-scale, barY-dotR-scale, thumbX+dotR+scale, barY+dotR+scale)

	// text, drawn by GDI onto the (still opaque) bitmap
	win.SetBkMode(memDC, win.TRANSPARENT)
	win.SetTextColor(memDC, theme.text.colorref())

	text := func(s string, face string, size float64, weight int32, flags uint32, r win.RECT) {
		lf := win.LOGFONT{LfHeight: -px(size), LfWeight: weight, LfQuality: win.ANTIALIASED_QUALITY} // ClearType fringes show on layered windows
		copy(lf.LfFaceName[:], syscall.StringToUTF16(face))
		font := win.CreateFontIndirect(&lf)
		old := win.SelectObject(memDC, win.HGDIOBJ(font))
		str, _ := syscall.UTF16PtrFromString(s)
		win.DrawTextEx(memDC, str, -1, &r, flags|win.DT_SINGLELINE|win.DT_NOPREFIX, nil)
		win.SelectObject(memDC, old)
		win.DeleteObject(win.HGDIOBJ(font))
	}

	left, top := shadow, shadow
	iconSize := px(osdIconSize)
	if logo := osdLogo(exe, iconSize); logo != 0 {
		win.DrawIconEx(memDC, left+px(osdPad), top+(cardH-iconSize)/2, logo, iconSize, iconSize, 0, 0, win.DI_NORMAL)
	} else {
		iconRect := win.RECT{Left: left + px(osdPad), Top: top, Right: left + px(osdPad) + iconSize, Bottom: top + cardH}
		text(osdIcon(value, mic), "Segoe MDL2 Assets", 20, win.FW_NORMAL, win.DT_CENTER|win.DT_VCENTER, iconRect)
	}

	labelRect := win.RECT{Left: int32(barX0 - thumbR), Top: top + px(10), Right: int32(barX1 + thumbR), Bottom: top + px(32)}
	text(label, "Segoe UI", 14, win.FW_SEMIBOLD, win.DT_LEFT|win.DT_VCENTER|win.DT_END_ELLIPSIS, labelRect)

	pctRect := win.RECT{Left: left + cardW - px(osdPad+osdPctWidth), Top: top, Right: left + cardW - px(osdPad), Bottom: top + cardH}
	text(fmt.Sprintf("%d", int(value*100+0.5)), "Segoe UI", 20, win.FW_SEMIBOLD, win.DT_RIGHT|win.DT_VCENTER, pctRect)

	// alpha pass: card coverage over a soft drop shadow, premultiplied as UpdateLayeredWindow wants
	shadowCard := sdfRoundRect(float64(shadow), float64(shadow)+3*scale, float64(cardW), float64(cardH), float64(osdRadius)*scale)
	blur := float64(osdShadow) * scale
	for y := int32(0); y < h; y++ {
		for x := int32(0); x < w; x++ {
			fx, fy := float64(x)+0.5, float64(y)+0.5
			cov := coverage(card(fx, fy))
			fall := math.Max(0, 1-math.Max(0, shadowCard(fx, fy))/blur)
			alpha := cov + 0.3*fall*fall*(1-cov)

			i := int(y*w + x)
			c := get(i)
			pixels[i] = uint32(alpha*255+0.5)<<24 | uint32(c.r*cov+0.5)<<16 | uint32(c.g*cov+0.5)<<8 | uint32(c.b*cov+0.5)
		}
	}

	pos := osdWindowPos(position, w, h, shadow, px(osdScreenInset))
	size := win.SIZE{CX: w, CY: h}
	src := win.POINT{}
	blend := osdBlend()
	procUpdateLayeredWindow.Call(uintptr(hwnd), uintptr(screenDC), uintptr(unsafe.Pointer(&pos)), uintptr(unsafe.Pointer(&size)),
		uintptr(memDC), uintptr(unsafe.Pointer(&src)), 0, uintptr(unsafe.Pointer(&blend)), 2 /* ULW_ALPHA */)
}

// osdWindowPos places the window inside the primary monitor's work area (i.e. not over the taskbar)
func osdWindowPos(position string, w, h, shadow, inset int32) win.POINT {
	mi := win.MONITORINFO{}
	mi.CbSize = uint32(unsafe.Sizeof(mi))
	win.GetMonitorInfo(win.MonitorFromWindow(0, win.MONITOR_DEFAULTTOPRIMARY), &mi)
	wa := mi.RcWork

	pick := func(side string, lo, hi, size int32) int32 {
		switch side {
		case "top", "left":
			return lo + inset - shadow
		case "bottom", "right":
			return hi - inset - size + shadow
		}
		return lo + (hi-lo-size)/2
	}

	vertical, horizontal := position, "center"
	for i := range position {
		if position[i] == '-' {
			vertical, horizontal = position[:i], position[i+1:]
		}
	}

	return win.POINT{X: pick(horizontal, wa.Left, wa.Right, w), Y: pick(vertical, wa.Top, wa.Bottom, h)}
}

// osdSessionsExe returns the executable path behind the sessions, if they all belong to the same app
func osdSessionsExe(sessions []Session) string {
	exe := ""
	for _, s := range sessions {
		ws, ok := s.(*wcaSession)
		if !ok || ws.pid == 0 {
			return "" // master, mic, devices, system sounds: no app logo
		}

		if exe != "" && !strings.EqualFold(ws.processName, filepath.Base(exe)) {
			return "" // several different apps, a single logo would be misleading
		}

		if exe == "" {
			exe = osdProcessExe(ws.pid)
		}
	}

	return exe
}

func osdProcessExe(pid uint32) string {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
	if err != nil {
		return ""
	}
	defer windows.CloseHandle(h)

	buf := make([]uint16, windows.MAX_PATH)
	size := uint32(len(buf))
	if windows.QueryFullProcessImageName(h, 0, &buf[0], &size) != nil {
		return ""
	}

	return windows.UTF16ToString(buf[:size])
}

// osdLogo extracts (and caches) an app's icon at the requested pixel size
func osdLogo(exe string, size int32) win.HICON {
	if exe == "" {
		return 0
	}

	key := fmt.Sprintf("%s|%d", exe, size)
	if icon, ok := osdLogos[key]; ok {
		return icon
	}

	var icon win.HICON
	path, _ := syscall.UTF16PtrFromString(exe)
	if win.FAILED(win.SHDefExtractIcon(path, 0, 0, &icon, nil, uint32(size))) {
		icon = 0
	}
	osdLogos[key] = icon // ponytail: never freed, bounded by the number of distinct apps seen

	return icon
}

// osdIcon picks a Segoe MDL2 Assets glyph matching the volume level
func osdIcon(value float64, mic bool) string {
	switch {
	case mic:
		return ""
	case value < 0.005:
		return ""
	case value < 0.33:
		return ""
	case value < 0.66:
		return ""
	}
	return ""
}

// osdCurrentTheme follows the Windows light/dark setting and accent color, like the native flyouts do
func osdCurrentTheme() osdTheme {
	light := false
	if k, err := registry.OpenKey(registry.CURRENT_USER, `Software\Microsoft\Windows\CurrentVersion\Themes\Personalize`, registry.QUERY_VALUE); err == nil {
		v, _, err := k.GetIntegerValue("SystemUsesLightTheme")
		light = err == nil && v == 1
		k.Close()
	}

	t := osdTheme{
		bg: rgb{44, 44, 44}, border: rgb{64, 64, 64}, text: rgb{255, 255, 255},
		track: rgb{110, 110, 110}, thumb: rgb{69, 69, 69}, accent: rgb{96, 205, 255},
	}
	paletteIdx := 1 // lighter accent shade reads better on dark
	if light {
		t = osdTheme{
			bg: rgb{249, 249, 249}, border: rgb{222, 222, 222}, text: rgb{27, 27, 27},
			track: rgb{180, 180, 180}, thumb: rgb{255, 255, 255}, accent: rgb{0, 95, 184},
		}
		paletteIdx = 4
	}

	// AccentPalette holds 8 RGBA shades of the accent color, lightest first
	if k, err := registry.OpenKey(registry.CURRENT_USER, `Software\Microsoft\Windows\CurrentVersion\Explorer\Accent`, registry.QUERY_VALUE); err == nil {
		if p, _, err := k.GetBinaryValue("AccentPalette"); err == nil && len(p) >= 32 {
			i := paletteIdx * 4
			t.accent = rgb{float64(p[i]), float64(p[i+1]), float64(p[i+2])}
		}
		k.Close()
	}

	return t
}

// coverage turns a signed distance (negative = inside) into an anti-aliased pixel coverage
func coverage(d float64) float64 {
	return math.Max(0, math.Min(1, 0.5-d))
}

func sdfRoundRect(x, y, w, h, r float64) func(px, py float64) float64 {
	cx, cy := x+w/2, y+h/2
	hx, hy := w/2-r, h/2-r
	return func(px, py float64) float64 {
		qx, qy := math.Abs(px-cx)-hx, math.Abs(py-cy)-hy
		return math.Hypot(math.Max(qx, 0), math.Max(qy, 0)) + math.Min(math.Max(qx, qy), 0) - r
	}
}

func sdfCircle(cx, cy, r float64) func(px, py float64) float64 {
	return func(px, py float64) float64 {
		return math.Hypot(px-cx, py-cy) - r
	}
}

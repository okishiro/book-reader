package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"html"
	"image"
	"image/color"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/app"
	"fyne.io/fyne/v2/canvas"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/dialog"
	"fyne.io/fyne/v2/storage"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"
	"github.com/taylorskalyo/goreader/epub"
)

type SwipeableCanvas struct {
	widget.BaseWidget
	content            fyne.CanvasObject
	onSwipeL, onSwipeR func()
	dragTotal          float32
}

func NewSwipeableCanvas(c fyne.CanvasObject, l, r func()) *SwipeableCanvas {
	s := &SwipeableCanvas{content: c, onSwipeL: l, onSwipeR: r}
	s.ExtendBaseWidget(s)
	return s
}
func (s *SwipeableCanvas) CreateRenderer() fyne.WidgetRenderer { return widget.NewSimpleRenderer(s.content) }
func (s *SwipeableCanvas) Dragged(e *fyne.DragEvent)           { s.dragTotal += e.Dragged.DX }
func (s *SwipeableCanvas) DragEnd() {
	if s.dragTotal < -30.0 && s.onSwipeL != nil {
		s.onSwipeL()
	} else if s.dragTotal > 30.0 && s.onSwipeR != nil {
		s.onSwipeR()
	}
	s.dragTotal = 0
}
func (s *SwipeableCanvas) Tapped(e *fyne.PointEvent) {
	w := s.Size().Width
	if e.Position.X < w*0.35 && s.onSwipeR != nil {
		s.onSwipeR()
	} else if e.Position.X > w*0.65 && s.onSwipeL != nil {
		s.onSwipeL()
	}
}

type fluidMobileLayout struct{}
func (f fluidMobileLayout) MinSize(_ []fyne.CanvasObject) fyne.Size { return fyne.NewSize(280, 400) }
func (f fluidMobileLayout) Layout(objs []fyne.CanvasObject, size fyne.Size) {
	tw, th := size.Width*0.92, size.Height*0.90
	for _, o := range objs {
		o.Move(fyne.NewPos((size.Width-tw)/2, (size.Height-th)/2))
		o.Resize(fyne.NewSize(tw, th))
	}
}

type zoneLayout struct{ idx int }
func (z zoneLayout) MinSize(objs []fyne.CanvasObject) fyne.Size {
	if len(objs) == 0 { return fyne.NewSize(280, 140) }
	return objs[0].MinSize()
}
func (z zoneLayout) Layout(objs []fyne.CanvasObject, size fyne.Size) {
	bias := []float32{0.0, 0.25, 0.50, 0.75, 1.00}[z.idx]
	for _, o := range objs {
		h := o.MinSize().Height
		if h <= 0 { h = 140 }
		ay := size.Height - h
		if ay < 0 { ay = 0 }
		o.Move(fyne.NewPos(0, ay*bias))
		o.Resize(fyne.NewSize(size.Width, h))
	}
}

type ReaderTheme struct {
	fyne.Theme
	fontSize      float32
	fontStyle     fyne.TextStyle
	fontName      string
	isConfiguring bool
}

func (t *ReaderTheme) Size(n fyne.ThemeSizeName) float32 {
	if n == theme.SizeNameText && !t.isConfiguring { return t.fontSize }
	return t.Theme.Size(n)
}
func (t *ReaderTheme) Font(s fyne.TextStyle) fyne.Resource {
	if !t.isConfiguring {
		if t.fontName == "Serif" { s.Symbol = true } else if t.fontName == "Monospace" { s.Monospace = true }
	}
	return t.Theme.Font(s)
}
func (t *ReaderTheme) Color(n fyne.ThemeColorName, v fyne.ThemeVariant) color.Color {
	cMap := map[fyne.ThemeColorName]color.Color{
		theme.ColorNameBackground:     color.Black,
		theme.ColorNameForeground:     color.White,
		theme.ColorNameButton:         color.RGBA{25, 25, 25, 255},
		theme.ColorNameDisabledButton: color.RGBA{12, 12, 12, 255},
		theme.ColorNameSeparator:      color.RGBA{45, 45, 45, 255},
	}
	if c, ok := cMap[n]; ok { return c }
	return t.Theme.Color(n, theme.VariantDark)
}

type BookProgress struct{ Chapter, Page int }
type AppStateData struct{ BookTracking map[string]BookProgress `json:"book_tracking"` }
type PageContent struct {
	IsImage bool
	Text    string
	ImgData image.Image
}

type ReaderApp struct {
	rc                                 *epub.ReadCloser
	currentBook, epubsDir, dbPath      string
	spinePaths, available              []string
	currentChap, currentPage, wpp      int
	pages                              []PageContent
	myApp                              fyne.App
	window                             fyne.Window
	rt                                 *ReaderTheme
	chapCache                          map[int][]PageContent
	trackingState                      AppStateData
	textLabel                          *widget.Label
	imageCanvas                        *canvas.Image
	contentBox, zoneAdjust             *fyne.Container
	inReaderView, isBold, isJustified  bool
	currentFace                        string
	selectedZone                       int
}

func main() {
	a := app.New()
	w := a.NewWindow("Reader")
	rt := &ReaderTheme{Theme: theme.DarkTheme(), fontSize: 17, fontName: "Sans-Serif"}
	a.Settings().SetTheme(rt)
	dir := filepath.Join(a.Storage().RootURI().Path(), "library")
	_ = os.MkdirAll(dir, 0755)
	r := &ReaderApp{myApp: a, window: w, rt: rt, epubsDir: dir, dbPath: filepath.Join(dir, "state.json"),
		chapCache: make(map[int][]PageContent), wpp: 55, currentFace: "Sans-Serif", trackingState: AppStateData{make(map[string]BookProgress)}}
	
	if f, err := os.Open(r.dbPath); err == nil {
		_ = json.NewDecoder(f).Decode(&r.trackingState)
		f.Close()
	}
	w.Canvas().SetOnTypedKey(func(k *fyne.KeyEvent) {
		if (k.Name == fyne.KeyEscape || k.Name == "Back") && r.inReaderView { r.goBack() }
	})
	w.SetContent(r.buildLibraryScreen())
	w.Resize(fyne.NewSize(390, 680))
	w.CenterOnScreen()
	w.ShowAndRun()
	if r.rc != nil { r.rc.Close() }
}

func (r *ReaderApp) goBack() { r.save(); r.inReaderView = false; r.window.SetContent(r.buildLibraryScreen()) }
func (r *ReaderApp) save() {
	if r.currentBook == "" { return }
	r.trackingState.BookTracking[r.currentBook] = BookProgress{r.currentChap, r.currentPage}
	if f, err := os.Create(r.dbPath); err == nil {
		e := json.NewEncoder(f); e.SetIndent("", "  "); _ = e.Encode(r.trackingState); f.Close()
	}
}

func (r *ReaderApp) buildLibraryScreen() fyne.CanvasObject {
	r.inReaderView = false
	files, _ := os.ReadDir(r.epubsDir)
	r.available = nil
	for _, f := range files {
		if !f.IsDir() && strings.HasSuffix(strings.ToLower(f.Name()), ".epub") { r.available = append(r.available, f.Name()) }
	}
	t := canvas.NewText("MY LIBRARY", color.White); t.TextSize = 18; t.TextStyle.Bold = true; t.Alignment = fyne.TextAlignCenter
	btn := widget.NewButton("+ Import ePub", func() {
		fd := dialog.NewFileOpen(func(uc fyne.URIReadCloser, err error) {
			if err != nil || uc == nil { return }
			defer uc.Close()
			n := uc.URI().Name()
			if !strings.HasSuffix(strings.ToLower(n), ".epub") { n += ".epub" }
			if dst, err := os.Create(filepath.Join(r.epubsDir, n)); err == nil {
				_, _ = io.Copy(dst, uc); dst.Close(); r.window.SetContent(r.buildLibraryScreen())
			}
		}, r.window)
		fd.SetFilter(storage.NewExtensionFileFilter([]string{".epub", ".EPUB"})); fd.Show()
	})
	btn.Importance = widget.HighImportance

	list := widget.NewList(func() int { return len(r.available) },
		func() fyne.CanvasObject { l := widget.NewLabel(""); l.Truncation = fyne.TextTruncateEllipsis; return l },
		func(id widget.ListItemID, o fyne.CanvasObject) {
			n := r.available[id]
			if strings.HasSuffix(strings.ToLower(n), ".epub") { n = n[:len(n)-5] }
			o.(*widget.Label).SetText(n)
		})
	list.OnSelected = func(id widget.ListItemID) { list.UnselectAll(); r.openBook(r.available[id]) }
	
	var body fyne.CanvasObject = list
	if len(r.available) == 0 {
		m := widget.NewLabel("No local books imported yet.\nUse the Import button above."); m.Alignment, m.Wrapping, m.TextStyle.Italic = fyne.TextAlignCenter, fyne.TextWrapWord, true
		body = container.NewCenter(m)
	}
	return container.NewMax(canvas.NewRectangle(color.Black), container.NewBorder(container.NewVBox(container.NewCenter(t), widget.NewSeparator(), container.NewPadded(btn), widget.NewSeparator()), nil, nil, nil, container.NewPadded(body)))
}

func (r *ReaderApp) buildReaderScreen() fyne.CanvasObject {
	r.inReaderView = true
	r.textLabel = widget.NewLabel(""); r.textLabel.Wrapping, r.textLabel.Alignment, r.textLabel.TextStyle = fyne.TextWrapWord, fyne.TextAlignLeading, r.rt.fontStyle
	r.imageCanvas = canvas.NewImageFromImage(nil); r.imageCanvas.FillMode = canvas.ImageFillContain
	r.contentBox = container.NewMax(r.textLabel)
	r.zoneAdjust = container.New(zoneLayout{r.selectedZone}, r.contentBox)
	
	bBtn := widget.NewButtonWithIcon("", theme.NavigateBackIcon(), func() { r.goBack() }); bBtn.Importance = widget.LowImportance
	oBtn := widget.NewButtonWithIcon("", theme.SettingsIcon(), func() { r.showOptions() })
	
	return container.NewMax(canvas.NewRectangle(color.Black), container.NewBorder(container.NewVBox(container.NewPadded(container.NewBorder(nil, nil, bBtn, oBtn, container.NewCenter(canvas.NewText(" ", color.Transparent)))), widget.NewSeparator()), nil, nil, nil, NewSwipeableCanvas(container.NewMax(canvas.NewRectangle(color.Black), container.New(fluidMobileLayout{}, r.zoneAdjust)), func() { r.turnPage(1) }, func() { r.turnPage(-1) })))
}

func (r *ReaderApp) showOptions() {
	r.rt.isConfiguring = true; r.myApp.Settings().SetTheme(r.rt)
	ch := func(d float32) func() { return func() { if r.rt.fontSize+d >= 11 && r.rt.fontSize+d <= 30 { r.rt.fontSize += d; r.upTypo() } } }
	
	bChk := widget.NewCheck("Bold Text", func(c bool) { r.isBold = c; r.upTypo() }); bChk.SetChecked(r.isBold)
	jChk := widget.NewCheck("Justified Text", func(c bool) {
		r.isJustified = c
		if c { r.textLabel.Alignment = fyne.TextAlignLeading } else { r.textLabel.Alignment = fyne.TextAlignCenter }
		r.render()
	}); jChk.SetChecked(r.isJustified)
	
	zSel := widget.NewSelect([]string{"Top", "Upper-Mid", "Center", "Lower-Mid", "Bottom"}, func(s string) {
		r.selectedZone = map[string]int{"Top": 0, "Upper-Mid": 1, "Center": 2, "Lower-Mid": 3, "Bottom": 4}[s]
		r.zoneAdjust.Layout = zoneLayout{r.selectedZone}; r.zoneAdjust.Refresh()
	}); zSel.SetSelected([]string{"Top", "Upper-Mid", "Center", "Lower-Mid", "Bottom"}[r.selectedZone])
	
	fSel := widget.NewSelect([]string{"Sans-Serif", "Serif", "Monospace"}, func(s string) { r.currentFace, r.rt.fontName = s, s; r.upTypo() }); fSel.SetSelected(r.currentFace)
	wEnt := widget.NewEntry(); wEnt.SetText(strconv.Itoa(r.wpp))
	
	gCur, gTot := 1, 0
	for i := 0; i < len(r.spinePaths); i++ {
		sz := len(r.getChap(i))
		if i < r.currentChap { gCur += sz }
		gTot += sz
	}
	if gTot == 0 { gTot = 1 }
	gCur += r.currentPage
	
	pEnt := widget.NewEntry(); pEnt.SetText(strconv.Itoa(gCur))
	d := dialog.NewCustom("Text Options", "Save & Apply", widget.NewForm(widget.NewFormItem("Font Face", fSel), widget.NewFormItem("Text Scaling", container.NewGridWithColumns(2, widget.NewButton("A-", ch(-1)), widget.NewButton("A+", ch(1)))), widget.NewFormItem("Format Style", container.NewVBox(bChk, jChk)), widget.NewFormItem("Shift Text Location", zSel), widget.NewFormItem("Words Per Page", wEnt), widget.NewFormItem("Go to Page #", container.NewBorder(nil, nil, nil, widget.NewLabel(fmt.Sprintf("/ %d", gTot)), pEnt))), r.window)
	d.SetOnClosed(func() {
		if v, err := strconv.Atoi(strings.TrimSpace(wEnt.Text)); err == nil && v > 5 && r.wpp != v {
			r.wpp = v; r.chapCache = make(map[int][]PageContent); r.pages = r.getChap(r.currentChap)
			if r.currentPage >= len(r.pages) { r.currentPage = 0 }
		}
		if tp, err := strconv.Atoi(strings.TrimSpace(pEnt.Text)); err == nil {
			if tp < 1 { tp = 1 } else if tp > gTot { tp = gTot }
			sum := 0
			for c := 0; c < len(r.spinePaths); c++ {
				sz := len(r.getChap(c))
				if sum+sz >= tp { r.currentChap, r.pages, r.currentPage = c, r.getChap(c), (tp-1)-sum; break }
				sum += sz
			}
		}
		r.rt.isConfiguring = false; r.myApp.Settings().SetTheme(r.rt); r.render(); r.save()
	})
	d.Resize(fyne.NewSize(310, 390)); d.Show()
}

func (r *ReaderApp) upTypo() {
	r.textLabel.TextStyle = r.rt.fontStyle; r.rt.fontStyle.Bold = r.isBold
	if !r.rt.isConfiguring { r.myApp.Settings().SetTheme(r.rt) }
	r.render()
}

func (r *ReaderApp) openBook(fn string) {
	if r.rc != nil { r.rc.Close() }
	rc, err := epub.OpenReader(filepath.Join(r.epubsDir, fn))
	if err != nil || len(rc.Rootfiles) == 0 { return }
	r.rc, r.currentBook, r.chapCache = rc, fn, make(map[int][]PageContent)
	m := make(map[string]epub.Item)
	for _, it := range rc.Rootfiles[0].Manifest.Items { m[it.ID] = it }
	r.spinePaths = nil
	for _, s := range rc.Rootfiles[0].Spine.Itemrefs {
		if it, ok := m[s.IDREF]; ok { r.spinePaths = append(r.spinePaths, it.HREF) }
	}
	r.window.SetContent(r.buildReaderScreen())
	if prg, ok := r.trackingState.BookTracking[fn]; ok {
		r.currentChap, r.pages, r.currentPage = prg.Chapter, r.getChap(prg.Chapter), prg.Page
		if r.currentPage >= len(r.pages) { r.currentPage = 0 }
		r.render()
	} else { r.loadChap(0) }
}

func (r *ReaderApp) loadChap(idx int) {
	if idx >= 0 && idx < len(r.spinePaths) { r.currentChap, r.pages, r.currentPage = idx, r.getChap(idx), 0; r.render(); r.save() }
}

func (r *ReaderApp) turnPage(d int) {
	n := r.currentPage + d
	if n < 0 && r.currentChap > 0 {
		r.currentChap--; r.pages = r.getChap(r.currentChap); r.currentPage = len(r.pages) - 1; r.render(); r.save()
	} else if n >= len(r.pages) && r.currentChap < len(r.spinePaths)-1 {
		r.loadChap(r.currentChap + 1)
	} else if n >= 0 && n < len(r.pages) {
		r.currentPage = n; r.render(); r.save()
	}
}

func (r *ReaderApp) render() {
	if r.textLabel == nil || r.contentBox == nil { return }
	r.contentBox.Objects = nil
	if len(r.pages) == 0 || r.currentPage >= len(r.pages) {
		r.textLabel.SetText("(empty page)"); r.contentBox.Add(r.textLabel)
	} else if cp := r.pages[r.currentPage]; cp.IsImage {
		r.imageCanvas.Image = cp.ImgData; r.contentBox.Add(r.imageCanvas); r.imageCanvas.Refresh()
	} else {
		txt := cp.Text
		if r.isJustified {
			lw := int(r.window.Canvas().Size().Width) / 10
			if lw < 15 { lw = 35 }
			txt = justify(txt, lw)
		}
		r.textLabel.SetText(txt); r.contentBox.Add(r.textLabel); r.textLabel.Refresh()
	}
	r.contentBox.Refresh()
	if r.zoneAdjust != nil { r.zoneAdjust.Refresh() }
}

func (r *ReaderApp) getChap(idx int) []PageContent {
	if cached, ok := r.chapCache[idx]; ok { return cached }
	var curItems []epub.Item
	for _, it := range r.rc.Rootfiles[0].Manifest.Items {
		if it.HREF == r.spinePaths[idx] { curItems = append(curItems, it); break }
	}
	if len(curItems) == 0 { return []PageContent{{Text: "[Missing]"}} }
	fd, err := curItems[0].Open()
	if err != nil { return []PageContent{{Text: "[Error]"}} }
	buf := new(strings.Builder); _, _ = io.Copy(buf, fd); fd.Close(); hSrc := buf.String()
	
	var dPgs []PageContent
	for _, m := range regexp.MustCompile(`(?i)<img\s+[^>]*src=["']([^"']+)["'][^>]*>|<image\s+[^>]*href=["']([^"']+)["'][^>]*>`).FindAllStringSubmatch(hSrc, -1) {
		src := m[1]; if src == "" { src = m[2] }
		if src == "" { continue }
		rImg := path.Clean(path.Join(path.Dir(r.spinePaths[idx]), src))
		for _, it := range r.rc.Rootfiles[0].Manifest.Items {
			if it.HREF == rImg || strings.EqualFold(it.HREF, rImg) || strings.EqualFold(path.Base(it.HREF), path.Base(rImg)) {
				if f, err := it.Open(); err == nil {
					b, _ := io.ReadAll(f); f.Close()
					if dec, _, err := image.Decode(bytes.NewReader(b)); err == nil { dPgs = append(dPgs, PageContent{IsImage: true, ImgData: dec}) }
				}
				break
			}
		}
	}
	
	hSrc = reB(`<style[^>]*>[\s\S]*?</style>`).ReplaceAllString(hSrc, "")
	hSrc = reB(`<script[^>]*>[\s\S]*?</script>`).ReplaceAllString(hSrc, "")
	hSrc = reB(`<h[1-6][^>]*>([\s\S]*?)</h[1-6]>`).ReplaceAllStringFunc(hSrc, func(m string) string {
		return " " + strings.ToUpper(strings.TrimSpace(html.UnescapeString(reB(`<[^>]*>`).ReplaceAllString(m, "")))) + " "
	})
	for _, tag := range []string{"p", "div", "section", "article", "blockquote", "li", "tr"} { hSrc = reB(`</` + tag + `>`).ReplaceAllString(hSrc, " ") }
	w := strings.Fields(strings.TrimSpace(reB(`\s+`).ReplaceAllString(html.UnescapeString(reB(`<[^>]*>`).ReplaceAllString(reB(`<br\s*/?>`).ReplaceAllString(hSrc, " "), "")), " ")))
	
	if len(w) == 0 && len(dPgs) == 0 { return []PageContent{{Text: "(empty)"}} }
	for i := 0; i < len(w); i += r.wpp {
		e := i + r.wpp; if e > len(w) { e = len(w) }
		dPgs = append(dPgs, PageContent{Text: strings.Join(w[i:e], " ")})
	}
	r.chapCache[idx] = dPgs
	return dPgs
}

func justify(t string, lw int) string {
	w := strings.Fields(t); if len(w) == 0 || lw <= 0 { return t }
	var res strings.Builder
	var line []string
	cl := 0
	for _, s := range w {
		if cl+len(line)+len(s) > lw {
			if len(line) > 0 {
				sk := len(line) - 1
				if sk <= 0 { res.WriteString(strings.Join(line, "") + "\n") } else {
					tot := lw - cl; b, r := tot/sk, tot%sk
					var sB strings.Builder
					for i, p := range line {
						sB.WriteString(p)
						if i < sk { sp := b; if i < r { sp++ }; sB.WriteString(strings.Repeat(" ", sp)) }
					}
					res.WriteString(sB.String() + "\n")
				}
				line, cl = []string{s}, len(s)
			} else { res.WriteString(s + "\n"); line, cl = nil, 0 }
		} else { line = append(line, s); cl += len(s) }
	}
	if len(line) > 0 { res.WriteString(strings.Join(line, " ")) }
	return res.String()
}

func reB(p string) *regexp.Regexp { return regexp.MustCompile(`(?i)` + p) }

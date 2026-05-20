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
	"path/filepath"
	"regexp"
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

// ─────────────────────────────────────────────────────────────────────
//  fixedLayout — stamps every child at (0,0) with exact size.
//  Used for root containers to clamp the window.
// ─────────────────────────────────────────────────────────────────────

type fixedLayout struct{ size fyne.Size }

func (f fixedLayout) Layout(objs []fyne.CanvasObject, _ fyne.Size) {
	for _, o := range objs {
		o.Move(fyne.NewPos(0, 0))
		o.Resize(f.size)
	}
}
func (f fixedLayout) MinSize(_ []fyne.CanvasObject) fyne.Size { return f.size }

// ─────────────────────────────────────────────────────────────────────
//  vBiasLayout — positions its single child vertically at biasPct%
//  of the slack space (0=top, 50=center, 100=bottom).
//  This is the ONLY layout that applies the bias; it never touches the
//  root/window container.
// ─────────────────────────────────────────────────────────────────────

type vBiasLayout struct {
	childW  float32
	childH  float32
	biasPct float32
}

func (v *vBiasLayout) Layout(objs []fyne.CanvasObject, size fyne.Size) {
	slack := size.Height - v.childH
	if slack < 0 {
		slack = 0
	}
	y := slack * v.biasPct / 100.0
	x := (size.Width - v.childW) / 2
	if x < 0 {
		x = 0
	}
	for _, o := range objs {
		o.Move(fyne.NewPos(x, y))
		o.Resize(fyne.NewSize(v.childW, v.childH))
	}
}

func (v *vBiasLayout) MinSize(_ []fyne.CanvasObject) fyne.Size {
	return fyne.NewSize(v.childW, v.childH)
}

// ─────────────────────────────────────────────────────────────────────
//  SwipeableCanvas — drag left → next, drag right → prev
// ─────────────────────────────────────────────────────────────────────

type SwipeableCanvas struct {
	widget.BaseWidget
	content   fyne.CanvasObject
	onLeft    func()
	onRight   func()
	dragTotal float32
}

func NewSwipeableCanvas(content fyne.CanvasObject, onLeft, onRight func()) *SwipeableCanvas {
	s := &SwipeableCanvas{content: content, onLeft: onLeft, onRight: onRight}
	s.ExtendBaseWidget(s)
	return s
}
func (s *SwipeableCanvas) CreateRenderer() fyne.WidgetRenderer {
	return widget.NewSimpleRenderer(s.content)
}
func (s *SwipeableCanvas) Dragged(e *fyne.DragEvent) { s.dragTotal += e.Dragged.DX }
func (s *SwipeableCanvas) DragEnd() {
	const threshold float32 = 40
	if s.dragTotal < -threshold && s.onLeft != nil {
		s.onLeft()
	} else if s.dragTotal > threshold && s.onRight != nil {
		s.onRight()
	}
	s.dragTotal = 0
}

// ─────────────────────────────────────────────────────────────────────
//  Theme
// ─────────────────────────────────────────────────────────────────────

type ReaderTheme struct {
	fyne.Theme
	fontSize float32
}

func (t *ReaderTheme) Size(n fyne.ThemeSizeName) float32 {
	if n == theme.SizeNameText {
		return t.fontSize
	}
	return t.Theme.Size(n)
}

func (t *ReaderTheme) Color(n fyne.ThemeColorName, _ fyne.ThemeVariant) color.Color {
	switch n {
	case theme.ColorNameBackground:
		return color.Black
	case theme.ColorNameForeground:
		return color.White
	case theme.ColorNameButton:
		return color.RGBA{R: 20, G: 20, B: 20, A: 255}
	case theme.ColorNameDisabledButton:
		return color.RGBA{R: 10, G: 10, B: 10, A: 255}
	case theme.ColorNameSeparator:
		return color.RGBA{R: 36, G: 36, B: 36, A: 255}
	case theme.ColorNameScrollBar:
		return color.RGBA{R: 50, G: 50, B: 50, A: 255}
	case theme.ColorNameInputBackground:
		return color.RGBA{R: 18, G: 18, B: 18, A: 255}
	}
	return t.Theme.Color(n, theme.VariantDark)
}

// ─────────────────────────────────────────────────────────────────────
//  Constants
// ─────────────────────────────────────────────────────────────────────

const (
	winW     float32 = 390
	winH     float32 = 680
	textBoxW float32 = 358
	textBoxH float32 = 510
)

// ─────────────────────────────────────────────────────────────────────
//  Data types
// ─────────────────────────────────────────────────────────────────────

type BookProgress struct {
	Chapter int `json:"chapter"`
	Page    int `json:"page"`
}

// SaveData persists both reading positions AND user settings.
type SaveData struct {
	Books       map[string]BookProgress `json:"books"`
	FontSize    float32                 `json:"font_size"`
	WordsPerPg  int                     `json:"words_per_page"`
	VertBias    float32                 `json:"vert_bias"`
	BoldText    bool                    `json:"bold_text"`
	Monospace   bool                    `json:"monospace"`
}

type PageContent struct {
	IsImage bool
	Text    string
	ImgData image.Image
}

// ─────────────────────────────────────────────────────────────────────
//  App state
// ─────────────────────────────────────────────────────────────────────

type ReaderApp struct {
	rc          *epub.ReadCloser
	currentBook string
	spinePaths  []string
	currentChap int
	pages       []PageContent
	currentPage int
	wordsPerPage int
	chapCache   map[int][]PageContent

	myApp     fyne.App
	window    fyne.Window
	rt        *ReaderTheme
	epubsDir  string
	savePath  string
	available []string
	saveData  SaveData

	// reader UI refs (valid only while reader screen is shown)
	textLabel   *widget.Label
	imageCanvas *canvas.Image
	contentBox  *fyne.Container
	pageLabel   *canvas.Text
	readerBias  *vBiasLayout // pointer so settings slider updates it live

	// settings state
	isBold    bool
	isMono    bool
	inReader  bool
}

// ─────────────────────────────────────────────────────────────────────
//  main
// ─────────────────────────────────────────────────────────────────────

func main() {
	myApp := app.New()
	myWindow := myApp.NewWindow("Reader")

	rt := &ReaderTheme{Theme: theme.DarkTheme(), fontSize: 17}
	myApp.Settings().SetTheme(rt)

	dir := "./library"
	_ = os.MkdirAll(dir, 0755)

	state := &ReaderApp{
		myApp:        myApp,
		window:       myWindow,
		rt:           rt,
		epubsDir:     dir,
		savePath:     filepath.Join(dir, "progress.json"),
		chapCache:    make(map[int][]PageContent),
		wordsPerPage: 55,
		saveData:     SaveData{Books: make(map[string]BookProgress)},
	}

	state.loadProgress()

	// Apply persisted settings
	if state.saveData.FontSize >= 11 {
		state.rt.fontSize = state.saveData.FontSize
	}
	if state.saveData.WordsPerPg > 5 {
		state.wordsPerPage = state.saveData.WordsPerPg
	}
	state.isBold = state.saveData.BoldText
	state.isMono = state.saveData.Monospace

	state.refreshLibrary()

	myWindow.Canvas().SetOnTypedKey(func(k *fyne.KeyEvent) {
		if k.Name == fyne.KeyEscape && state.inReader {
			state.goToLibrary()
		}
	})

	myWindow.SetContent(state.libraryScreen())
	myWindow.SetFixedSize(true)
	myWindow.Resize(fyne.NewSize(winW, winH))
	myWindow.CenterOnScreen()
	myWindow.ShowAndRun()

	if state.rc != nil {
		state.rc.Close()
	}
}

// ─────────────────────────────────────────────────────────────────────
//  Screen: Library
// ─────────────────────────────────────────────────────────────────────

func (r *ReaderApp) libraryScreen() fyne.CanvasObject {
	r.inReader = false
	r.refreshLibrary()

	// Header
	heading := canvas.NewText("LIBRARY", color.White)
	heading.TextSize = 13
	heading.TextStyle = fyne.TextStyle{Bold: true}
	heading.Alignment = fyne.TextAlignCenter

	// Book list — two-line rows: title + progress hint
	list := widget.NewList(
		func() int { return len(r.available) },
		func() fyne.CanvasObject {
			title := widget.NewLabel("")
			title.Truncation = fyne.TextTruncateEllipsis
			sub := canvas.NewText("", dimColor(80))
			sub.TextSize = 11
			return container.NewVBox(title, sub)
		},
		func(id widget.ListItemID, o fyne.CanvasObject) {
			c := o.(*fyne.Container)
			titleLbl := c.Objects[0].(*widget.Label)
			subTxt := c.Objects[1].(*canvas.Text)
			raw := r.available[id]
			titleLbl.SetText(strings.TrimSuffix(raw, ".epub"))
			if p, ok := r.saveData.Books[raw]; ok {
				subTxt.Text = fmt.Sprintf("Last read: ch %d · p %d", p.Chapter+1, p.Page+1)
			} else {
				subTxt.Text = "Not started"
			}
			subTxt.Refresh()
		},
	)
	list.OnSelected = func(id widget.ListItemID) {
		list.UnselectAll()
		r.openBook(r.available[id])
	}

	// Empty state
	empty := canvas.NewText("Add .epub files to ./library/ or import below.", dimColor(65))
	empty.TextSize = 13
	empty.Alignment = fyne.TextAlignCenter

	var body fyne.CanvasObject
	if len(r.available) == 0 {
		body = container.NewCenter(empty)
	} else {
		body = list
	}

	// Import button
	importBtn := widget.NewButton("＋  Import ePub", func() {
		fd := dialog.NewFileOpen(func(uc fyne.URIReadCloser, err error) {
			if err != nil || uc == nil {
				return
			}
			defer uc.Close()
			dst := filepath.Join(r.epubsDir, uc.URI().Name())
			out, err := os.Create(dst)
			if err != nil {
				dialog.ShowError(err, r.window)
				return
			}
			defer out.Close()
			_, _ = io.Copy(out, uc)
			r.window.SetContent(r.libraryScreen())
		}, r.window)
		fd.SetFilter(storage.NewExtensionFileFilter([]string{".epub"}))
		fd.Show()
	})

	top := container.NewVBox(
		vSpace(10),
		container.NewCenter(heading),
		vSpace(6),
		hRule(),
	)
	bottom := container.NewVBox(
		hRule(),
		container.NewPadded(importBtn),
		vSpace(4),
	)

	bg := canvas.NewRectangle(color.Black)
	content := container.NewBorder(top, bottom, nil, nil, container.NewPadded(body))
	return container.New(fixedLayout{fyne.NewSize(winW, winH)}, bg, content)
}

// ─────────────────────────────────────────────────────────────────────
//  Screen: Reader
// ─────────────────────────────────────────────────────────────────────

func (r *ReaderApp) readerScreen() fyne.CanvasObject {
	r.inReader = true

	r.textLabel = widget.NewLabel("")
	r.textLabel.Wrapping = fyne.TextWrapWord
	r.textLabel.Alignment = fyne.TextAlignLeading // leading is correct default for reading
	r.textLabel.TextStyle = fyne.TextStyle{Bold: r.isBold, Monospace: r.isMono}

	r.imageCanvas = canvas.NewImageFromImage(nil)
	r.imageCanvas.FillMode = canvas.ImageFillContain

	r.contentBox = container.NewMax(r.textLabel)

	// Page counter — very dim, centered in top bar
	r.pageLabel = canvas.NewText("", dimColor(60))
	r.pageLabel.TextSize = 11
	r.pageLabel.Alignment = fyne.TextAlignCenter

	// ── Top bar ─────────────────────────────────────────────
	backBtn := ghostBtn("←", func() { r.goToLibrary() })
	settingsBtn := widget.NewButtonWithIcon("", theme.SettingsIcon(), func() {
		r.showSettings()
	})
	settingsBtn.Importance = widget.LowImportance

	topBar := container.NewBorder(nil, nil,
		padded(backBtn),
		padded(settingsBtn),
		container.NewCenter(r.pageLabel),
	)

	// ── Bottom bar: large tap-friendly arrows ────────────────
	prevBtn := ghostBtn("◀", func() { r.turnPage(-1) })
	nextBtn := ghostBtn("▶", func() { r.turnPage(1) })

	bottomBar := container.NewBorder(nil, nil,
		padded(prevBtn),
		padded(nextBtn),
		nil,
	)

	// ── Text area with vertical bias ─────────────────────────
	// textClamped: hard-sized box so the label never expands the window
	textClamped := container.New(
		fixedLayout{fyne.NewSize(textBoxW, textBoxH)},
		r.contentBox,
	)

	// vBiasLayout positions textClamped vertically within the reading area
	r.readerBias = &vBiasLayout{
		childW:  textBoxW,
		childH:  textBoxH,
		biasPct: r.saveData.VertBias,
	}
	readingArea := container.New(r.readerBias, textClamped)

	// Swipe surface covers the reading area for page-turn gestures
	swipe := NewSwipeableCanvas(readingArea,
		func() { r.turnPage(1) },
		func() { r.turnPage(-1) },
	)

	bg := canvas.NewRectangle(color.Black)
	inner := container.NewBorder(
		container.NewVBox(padded(topBar), hRule()),
		container.NewVBox(hRule(), padded(bottomBar)),
		nil, nil,
		swipe,
	)
	return container.New(fixedLayout{fyne.NewSize(winW, winH)}, bg, inner)
}

// ─────────────────────────────────────────────────────────────────────
//  Settings dialog
// ─────────────────────────────────────────────────────────────────────

func (r *ReaderApp) showSettings() {
	// Font size
	fontDown := widget.NewButton("A−", func() {
		if r.rt.fontSize > 11 {
			r.rt.fontSize--
			r.myApp.Settings().SetTheme(r.rt)
			if r.textLabel != nil {
				r.textLabel.Refresh()
			}
		}
	})
	fontUp := widget.NewButton("A+", func() {
		if r.rt.fontSize < 28 {
			r.rt.fontSize++
			r.myApp.Settings().SetTheme(r.rt)
			if r.textLabel != nil {
				r.textLabel.Refresh()
			}
		}
	})

	// Style toggles
	boldCheck := widget.NewCheck("Bold", func(v bool) {
		r.isBold = v
		if r.textLabel != nil {
			r.textLabel.TextStyle.Bold = v
			r.textLabel.Refresh()
		}
	})
	boldCheck.SetChecked(r.isBold)

	monoCheck := widget.NewCheck("Monospace", func(v bool) {
		r.isMono = v
		if r.textLabel != nil {
			r.textLabel.TextStyle.Monospace = v
			r.textLabel.Refresh()
		}
	})
	monoCheck.SetChecked(r.isMono)

	// Words per page slider
	wordsSlider := widget.NewSlider(10, 120)
	wordsSlider.Step = 5
	wordsSlider.SetValue(float64(r.wordsPerPage))
	wordsHint := canvas.NewText(fmt.Sprintf("%d words / page", r.wordsPerPage), dimColor(160))
	wordsHint.TextSize = 11
	wordsSlider.OnChanged = func(v float64) {
		wordsHint.Text = fmt.Sprintf("%d words / page", int(v))
		wordsHint.Refresh()
	}

	// Vertical position slider
	biasSlider := widget.NewSlider(0, 100)
	biasSlider.Step = 1
	biasSlider.SetValue(float64(r.saveData.VertBias))
	biasHint := canvas.NewText(biasLabel(r.saveData.VertBias), dimColor(160))
	biasHint.TextSize = 11
	biasSlider.OnChanged = func(v float64) {
		r.saveData.VertBias = float32(v)
		biasHint.Text = biasLabel(float32(v))
		biasHint.Refresh()
		// Live update: move the text block in the reader without rebuilding the screen
		if r.readerBias != nil {
			r.readerBias.biasPct = float32(v)
			r.window.Content().Refresh()
		}
	}

	form := container.NewVBox(
		sectionLabel("FONT SIZE"),
		container.NewGridWithColumns(2, fontDown, fontUp),
		vSpace(6),
		sectionLabel("STYLE"),
		container.NewHBox(boldCheck, monoCheck),
		vSpace(6),
		sectionLabel("WORDS PER PAGE"),
		wordsSlider,
		container.NewCenter(wordsHint),
		vSpace(6),
		sectionLabel("TEXT POSITION  (top → bottom)"),
		biasSlider,
		container.NewCenter(biasHint),
	)

	d := dialog.NewCustom("Settings", "Done", form, r.window)
	d.SetOnClosed(func() {
		newWords := int(wordsSlider.Value)
		if newWords > 5 && newWords != r.wordsPerPage {
			r.wordsPerPage = newWords
			r.chapCache = make(map[int][]PageContent)
			r.pages = r.chapPages(r.currentChap)
			if r.currentPage >= len(r.pages) {
				r.currentPage = 0
			}
			r.render()
		}
		// Persist all settings
		r.saveData.FontSize = r.rt.fontSize
		r.saveData.WordsPerPg = r.wordsPerPage
		r.saveData.BoldText = r.isBold
		r.saveData.Monospace = r.isMono
		r.saveProgress()
	})
	d.Resize(fyne.NewSize(330, 440))
	d.Show()
}

func biasLabel(v float32) string {
	switch {
	case v < 15:
		return "Top"
	case v < 38:
		return "Upper"
	case v < 63:
		return "Center"
	case v < 86:
		return "Lower"
	default:
		return "Bottom"
	}
}

// ─────────────────────────────────────────────────────────────────────
//  Navigation helpers
// ─────────────────────────────────────────────────────────────────────

func (r *ReaderApp) goToLibrary() {
	r.saveProgress()
	r.inReader = false
	r.window.SetContent(r.libraryScreen())
}

func (r *ReaderApp) openBook(filename string) {
	if r.rc != nil {
		r.rc.Close()
		r.rc = nil
	}

	rc, err := epub.OpenReader(filepath.Join(r.epubsDir, filename))
	if err != nil {
		dialog.ShowError(fmt.Errorf("cannot open book:\n%v", err), r.window)
		return
	}
	if len(rc.Rootfiles) == 0 {
		dialog.ShowError(fmt.Errorf("invalid ePub: no root file"), r.window)
		rc.Close()
		return
	}

	book := rc.Rootfiles[0]
	manifest := make(map[string]epub.Item)
	for _, item := range book.Manifest.Items {
		manifest[item.ID] = item
	}

	var paths []string
	for _, ref := range book.Spine.Itemrefs {
		if item, ok := manifest[ref.IDREF]; ok {
			paths = append(paths, item.HREF)
		}
	}

	r.rc = rc
	r.currentBook = filename
	r.spinePaths = paths
	r.chapCache = make(map[int][]PageContent)

	r.window.SetContent(r.readerScreen())

	if saved, ok := r.saveData.Books[filename]; ok {
		r.currentChap = saved.Chapter
		r.pages = r.chapPages(r.currentChap)
		r.currentPage = saved.Page
		if r.currentPage >= len(r.pages) {
			r.currentPage = 0
		}
		r.render()
	} else {
		r.loadChapter(0)
	}
}

func (r *ReaderApp) loadChapter(idx int) {
	if r.rc == nil || idx < 0 || idx >= len(r.spinePaths) {
		return
	}
	r.currentChap = idx
	r.pages = r.chapPages(idx)
	r.currentPage = 0
	r.render()
	r.saveProgress()
}

func (r *ReaderApp) turnPage(delta int) {
	if r.rc == nil {
		return
	}
	next := r.currentPage + delta
	if next < 0 {
		if r.currentChap > 0 {
			r.currentChap--
			r.pages = r.chapPages(r.currentChap)
			r.currentPage = len(r.pages) - 1
			r.render()
			r.saveProgress()
		}
		return
	}
	if next >= len(r.pages) {
		if r.currentChap < len(r.spinePaths)-1 {
			r.loadChapter(r.currentChap + 1)
		}
		return
	}
	r.currentPage = next
	r.render()
	r.saveProgress()
}

// ─────────────────────────────────────────────────────────────────────
//  render — update content box + page counter
// ─────────────────────────────────────────────────────────────────────

func (r *ReaderApp) render() {
	if r.textLabel == nil || r.contentBox == nil || r.pageLabel == nil {
		return
	}

	if len(r.pages) == 0 || r.currentPage >= len(r.pages) {
		r.contentBox.Objects = []fyne.CanvasObject{r.textLabel}
		r.textLabel.SetText("(empty)")
		r.contentBox.Refresh()
		return
	}

	pg := r.pages[r.currentPage]
	if pg.IsImage {
		r.imageCanvas.Image = pg.ImgData
		r.imageCanvas.Refresh()
		r.contentBox.Objects = []fyne.CanvasObject{r.imageCanvas}
	} else {
		r.textLabel.SetText(pg.Text)
		r.textLabel.Refresh()
		r.contentBox.Objects = []fyne.CanvasObject{r.textLabel}
	}
	r.contentBox.Refresh()

	// Page counter
	global := r.currentPage + 1
	for i := 0; i < r.currentChap; i++ {
		global += len(r.chapPages(i))
	}
	total := 0
	for i := range r.spinePaths {
		total += len(r.chapPages(i))
	}
	r.pageLabel.Text = fmt.Sprintf("%d / %d", global, total)
	r.pageLabel.Refresh()
}

// ─────────────────────────────────────────────────────────────────────
//  Paging — interleaves images with text in HTML document order
// ─────────────────────────────────────────────────────────────────────

func (r *ReaderApp) chapPages(idx int) []PageContent {
	if cached, ok := r.chapCache[idx]; ok {
		return cached
	}
	pages := r.parseChapter(idx)
	r.chapCache[idx] = pages
	return pages
}

func (r *ReaderApp) parseChapter(idx int) []PageContent {
	path := r.spinePaths[idx]
	book := r.rc.Rootfiles[0]

	var item *epub.Item
	for i := range book.Manifest.Items {
		if book.Manifest.Items[i].HREF == path {
			item = &book.Manifest.Items[i]
			break
		}
	}
	if item == nil {
		return []PageContent{{Text: fmt.Sprintf("[missing: %s]", path)}}
	}

	fd, err := item.Open()
	if err != nil {
		return []PageContent{{Text: fmt.Sprintf("[error: %v]", err)}}
	}
	defer fd.Close()

	buf := new(strings.Builder)
	_, _ = io.Copy(buf, fd)
	raw := buf.String()

	// Walk the HTML, collecting text segments and images in document order.
	imgRe := regexp.MustCompile(`(?i)<img\s+[^>]*src=["']([^"']+)["'][^>]*>`)
	locs := imgRe.FindAllStringSubmatchIndex(raw, -1)
	srcs := imgRe.FindAllStringSubmatch(raw, -1)

	var pages []PageContent
	prevEnd := 0

	for i, loc := range locs {
		// Text segment before this <img>
		seg := htmlToPlainText(raw[prevEnd:loc[0]])
		pages = appendTextPages(pages, seg, r.wordsPerPage)

		// The image itself
		imgSrc := srcs[i][1]
		resolved := filepath.ToSlash(
			filepath.Clean(filepath.Join(filepath.Dir(path), imgSrc)),
		)
		for _, mi := range book.Manifest.Items {
			if mi.HREF == resolved {
				if imgFile, e := mi.Open(); e == nil {
					data, _ := io.ReadAll(imgFile)
					imgFile.Close()
					if img, _, e2 := image.Decode(bytes.NewReader(data)); e2 == nil {
						pages = append(pages, PageContent{IsImage: true, ImgData: img})
					}
				}
				break
			}
		}
		prevEnd = loc[1]
	}

	// Remaining text after the last image (or entire chapter if no images)
	pages = appendTextPages(pages, htmlToPlainText(raw[prevEnd:]), r.wordsPerPage)

	if len(pages) == 0 {
		return []PageContent{{Text: "(empty chapter)"}}
	}
	return pages
}

// appendTextPages splits plain text into wordsPerPage-word chunks and appends them.
func appendTextPages(pages []PageContent, text string, n int) []PageContent {
	words := strings.Fields(text)
	for i := 0; i < len(words); i += n {
		end := i + n
		if end > len(words) {
			end = len(words)
		}
		pages = append(pages, PageContent{Text: strings.Join(words[i:end], " ")})
	}
	return pages
}

// ─────────────────────────────────────────────────────────────────────
//  HTML → plain text
// ─────────────────────────────────────────────────────────────────────

func htmlToPlainText(src string) string {
	src = reBlock(`<style[^>]*>[\s\S]*?</style>`).ReplaceAllString(src, "")
	src = reBlock(`<script[^>]*>[\s\S]*?</script>`).ReplaceAllString(src, "")
	src = reBlock(`<h[1-6][^>]*>([\s\S]*?)</h[1-6]>`).ReplaceAllStringFunc(src, func(m string) string {
		inner := reBlock(`<[^>]*>`).ReplaceAllString(m, "")
		return " " + strings.ToUpper(strings.TrimSpace(html.UnescapeString(inner))) + " "
	})
	for _, tag := range []string{"p", "div", "section", "article", "blockquote", "li", "tr"} {
		src = reBlock(`</` + tag + `>`).ReplaceAllString(src, " ")
	}
	src = reBlock(`<br\s*/?>`).ReplaceAllString(src, " ")
	src = reBlock(`<[^>]*>`).ReplaceAllString(src, "")
	src = html.UnescapeString(src)
	src = reBlock(`\s+`).ReplaceAllString(src, " ")
	return strings.TrimSpace(src)
}

func reBlock(p string) *regexp.Regexp { return regexp.MustCompile(`(?i)` + p) }

// ─────────────────────────────────────────────────────────────────────
//  Persistence
// ─────────────────────────────────────────────────────────────────────

func (r *ReaderApp) saveProgress() {
	if r.currentBook != "" {
		r.saveData.Books[r.currentBook] = BookProgress{
			Chapter: r.currentChap,
			Page:    r.currentPage,
		}
	}
	f, err := os.Create(r.savePath)
	if err != nil {
		return
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	enc.SetIndent("", "  ")
	_ = enc.Encode(r.saveData)
}

func (r *ReaderApp) loadProgress() {
	f, err := os.Open(r.savePath)
	if err != nil {
		return
	}
	defer f.Close()
	_ = json.NewDecoder(f).Decode(&r.saveData)
	if r.saveData.Books == nil {
		r.saveData.Books = make(map[string]BookProgress)
	}
}

// ─────────────────────────────────────────────────────────────────────
//  Library
// ─────────────────────────────────────────────────────────────────────

func (r *ReaderApp) refreshLibrary() {
	files, _ := os.ReadDir(r.epubsDir)
	var list []string
	for _, f := range files {
		if !f.IsDir() && strings.HasSuffix(strings.ToLower(f.Name()), ".epub") {
			list = append(list, f.Name())
		}
	}
	r.available = list
}

// ─────────────────────────────────────────────────────────────────────
//  UI helpers
// ─────────────────────────────────────────────────────────────────────

func ghostBtn(label string, fn func()) *widget.Button {
	b := widget.NewButton(label, fn)
	b.Importance = widget.LowImportance
	return b
}

func hRule() *canvas.Rectangle {
	r := canvas.NewRectangle(color.RGBA{R: 30, G: 30, B: 30, A: 255})
	r.SetMinSize(fyne.NewSize(winW, 1))
	return r
}

func vSpace(h float32) fyne.CanvasObject {
	s := canvas.NewRectangle(color.Transparent)
	s.SetMinSize(fyne.NewSize(1, h))
	return s
}

func padded(o fyne.CanvasObject) *fyne.Container {
	return container.NewPadded(o)
}

func dimColor(brightness uint8) color.Color {
	return color.RGBA{R: brightness, G: brightness, B: brightness, A: 255}
}

func sectionLabel(text string) *canvas.Text {
	t := canvas.NewText(text, dimColor(90))
	t.TextSize = 10
	t.TextStyle = fyne.TextStyle{Bold: true}
	return t
}

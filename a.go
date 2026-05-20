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

// ────────────────────────────────────────────────────────────────
//  Mobile Gesture Surface (Swipe Detection using Draggable)
// ────────────────────────────────────────────────────────────────

type SwipeableCanvas struct {
	widget.BaseWidget
	content   fyne.CanvasObject
	onSwipeL  func()
	onSwipeR  func()
	dragTotal float32
}

func NewSwipeableCanvas(content fyne.CanvasObject, onLeft, onRight func()) *SwipeableCanvas {
	s := &SwipeableCanvas{content: content, onSwipeL: onLeft, onSwipeR: onRight}
	s.ExtendBaseWidget(s)
	return s
}

func (s *SwipeableCanvas) CreateRenderer() fyne.WidgetRenderer {
	return widget.NewSimpleRenderer(s.content)
}

func (s *SwipeableCanvas) Dragged(e *fyne.DragEvent) {
	s.dragTotal += e.Dragged.DX
}

func (s *SwipeableCanvas) DragEnd() {
	const swipeThreshold float32 = 35.0

	if s.dragTotal < -swipeThreshold {
		if s.onSwipeL != nil {
			s.onSwipeL()
		}
	} else if s.dragTotal > swipeThreshold {
		if s.onSwipeR != nil {
			s.onSwipeR()
		}
	}
	s.dragTotal = 0
}

// ────────────────────────────────────────────────────────────────
//  Fixed layout — Pointer-Based Position Engine
// ────────────────────────────────────────────────────────────────

type fixedLayout struct {
	size        fyne.Size
	vBiasPct    float32 // 0 = Top, 50 = Center, 100 = Bottom
}

func NewFixedLayout(size fyne.Size, bias float32) *fixedLayout {
	return &fixedLayout{size: size, vBiasPct: bias}
}

func (f *fixedLayout) Layout(objs []fyne.CanvasObject, parentSize fyne.Size) {
	posX := (parentSize.Width - f.size.Width) / 2
	
	emptySpaceY := parentSize.Height - f.size.Height
	if emptySpaceY < 0 {
		emptySpaceY = 0
	}
	
	posY := emptySpaceY * (f.vBiasPct / 100.0)

	for _, o := range objs {
		o.Move(fyne.NewPos(posX, posY))
		o.Resize(f.size)
	}
}

func (f *fixedLayout) MinSize(_ []fyne.CanvasObject) fyne.Size { 
	return f.size 
}

// ────────────────────────────────────────────────────────────────
//  Theme & Typography Configurations
// ────────────────────────────────────────────────────────────────

type ReaderTheme struct {
	fyne.Theme
	fontSize  float32
	fontStyle fyne.TextStyle
}

func (t *ReaderTheme) Size(name fyne.ThemeSizeName) float32 {
	if name == theme.SizeNameText {
		return t.fontSize
	}
	return t.Theme.Size(name)
}

func (t *ReaderTheme) Color(name fyne.ThemeColorName, variant fyne.ThemeVariant) color.Color {
	switch name {
	case theme.ColorNameBackground:
		return color.Black
	case theme.ColorNameForeground:
		return color.White
	case theme.ColorNameButton:
		return color.RGBA{R: 25, G: 25, B: 25, A: 255}
	case theme.ColorNameDisabledButton:
		return color.RGBA{R: 12, G: 12, B: 12, A: 255}
	case theme.ColorNameSeparator:
		return color.RGBA{R: 45, G: 45, B: 45, A: 255}
	}
	return t.Theme.Color(name, theme.VariantDark)
}

// ────────────────────────────────────────────────────────────────
//  Dimensions (Portrait Mobile Viewport)
// ────────────────────────────────────────────────────────────────

const (
	winW float32 = 390
	winH float32 = 680

	textBoxW float32 = 358
	textBoxH float32 = 540
)

type BookProgress struct {
	Chapter int `json:"chapter"`
	Page    int `json:"page"`
}

type AppStateData struct {
	BookTracking map[string]BookProgress `json:"book_tracking"`
	VerticalBias float32                 `json:"vertical_bias"`
}

type PageContent struct {
	IsImage bool
	Text    string
	ImgData image.Image
}

type ReaderApp struct {
	rc           *epub.ReadCloser
	currentBook  string
	spinePaths   []string
	currentChap  int
	pages        []PageContent
	currentPage  int
	wordsPerPage int

	myApp     fyne.App
	window    fyne.Window
	rt        *ReaderTheme
	epubsDir  string
	dbPath    string
	available []string
	chapCache map[int][]PageContent

	trackingState AppStateData

	// UI Controls
	textLabel    *widget.Label
	imageCanvas  *canvas.Image
	contentBox   *fyne.Container
	readerLayout *fixedLayout // Direct pointer storage to prevent dynamic type assertion crashes

	// Mobile UX state tracking
	inReaderView bool

	isBold      bool
	isJustified bool
	currentFace string
}

func main() {
	myApp := app.New()
	myWindow := myApp.NewWindow("Reader")

	rt := &ReaderTheme{
		Theme:     theme.DarkTheme(),
		fontSize:  17,
		fontStyle: fyne.TextStyle{},
	}
	myApp.Settings().SetTheme(rt)

	dir := "./library"
	_ = os.MkdirAll(dir, 0755)
	dbFilePath := filepath.Join(dir, "state.json")

	state := &ReaderApp{
		myApp:        myApp,
		window:       myWindow,
		rt:           rt,
		epubsDir:     dir,
		dbPath:       dbFilePath,
		chapCache:    make(map[int][]PageContent),
		wordsPerPage: 55,
		currentFace:  "Sans-Serif",
		trackingState: AppStateData{
			BookTracking: make(map[string]BookProgress),
			VerticalBias: 0.0, 
		},
	}

	state.loadStateFromFile()

	myWindow.Canvas().SetOnTypedKey(func(k *fyne.KeyEvent) {
		if k.Name == fyne.KeyEscape {
			if state.inReaderView {
				state.handleMobileBackGesture()
				return
			}
		}
	})

	myWindow.SetContent(state.buildLibraryScreen())
	myWindow.SetFixedSize(true)
	myWindow.Resize(fyne.NewSize(winW, winH))
	myWindow.CenterOnScreen()
	myWindow.ShowAndRun()

	if state.rc != nil {
		state.rc.Close()
	}
}

func (r *ReaderApp) handleMobileBackGesture() {
	r.saveStateToDisk()
	r.inReaderView = false
	r.window.SetContent(r.buildLibraryScreen())
}

// ────────────────────────────────────────────────────────────────
//  Persistence Engine
// ────────────────────────────────────────────────────────────────

func (r *ReaderApp) loadStateFromFile() {
	file, err := os.Open(r.dbPath)
	if err != nil {
		return
	}
	defer file.Close()

	decoder := json.NewDecoder(file)
	_ = decoder.Decode(&r.trackingState)
	if r.trackingState.BookTracking == nil {
		r.trackingState.BookTracking = make(map[string]BookProgress)
	}
}

func (r *ReaderApp) saveStateToDisk() {
	if r.currentBook == "" {
		return
	}

	r.trackingState.BookTracking[r.currentBook] = BookProgress{
		Chapter: r.currentChap,
		Page:    r.currentPage,
	}

	file, err := os.Create(r.dbPath)
	if err != nil {
		return
	}
	defer file.Close()

	encoder := json.NewEncoder(file)
	encoder.SetIndent("", "  ")
	_ = encoder.Encode(r.trackingState)
}

// ────────────────────────────────────────────────────────────────
//  Screen: Library
// ────────────────────────────────────────────────────────────────

func (r *ReaderApp) buildLibraryScreen() fyne.CanvasObject {
	r.inReaderView = false
	r.refreshLibrary()

	title := canvas.NewText("MY LIBRARY", color.White)
	title.TextSize = 18
	title.TextStyle = fyne.TextStyle{Bold: true}
	title.Alignment = fyne.TextAlignCenter

	importBtn := widget.NewButton("+ Import ePub", func() {
		fd := dialog.NewFileOpen(func(uc fyne.URIReadCloser, err error) {
			if err != nil || uc == nil {
				return
			}
			defer uc.Close()

			dstPath := filepath.Join(r.epubsDir, uc.URI().Name())
			out, err := os.Create(dstPath)
			if err != nil {
				dialog.ShowError(err, r.window)
				return
			}
			defer out.Close()

			if _, err := io.Copy(out, uc); err != nil {
				dialog.ShowError(err, r.window)
				return
			}

			r.window.SetContent(r.buildLibraryScreen())
		}, r.window)
		fd.SetFilter(storage.NewExtensionFileFilter([]string{".epub"}))
		fd.Show()
	})
	importBtn.Importance = widget.HighImportance

	list := widget.NewList(
		func() int { return len(r.available) },
		func() fyne.CanvasObject {
			lbl := widget.NewLabel("")
			lbl.Truncation = fyne.TextTruncateEllipsis
			return lbl
		},
		func(id widget.ListItemID, o fyne.CanvasObject) {
			name := r.available[id]
			display := strings.TrimSuffix(name, ".epub")
			o.(*widget.Label).SetText(display)
		},
	)
	list.OnSelected = func(id widget.ListItemID) {
		list.UnselectAll()
		r.openBook(r.available[id])
	}

	emptyMsg := widget.NewLabel("Drop .epub files into ./library/\nor use Import above.")
	emptyMsg.Alignment = fyne.TextAlignCenter
	emptyMsg.Wrapping = fyne.TextWrapWord
	emptyMsg.TextStyle = fyne.TextStyle{Italic: true}

	var body fyne.CanvasObject
	if len(r.available) == 0 {
		body = container.NewCenter(emptyMsg)
	} else {
		body = list
	}

	topBar := container.NewVBox(
		container.NewCenter(title),
		widget.NewSeparator(),
		container.NewPadded(importBtn),
		widget.NewSeparator(),
	)

	bg := canvas.NewRectangle(color.Black)
	content := container.NewBorder(topBar, nil, nil, nil, container.NewPadded(body))

	return container.New(NewFixedLayout(fyne.NewSize(winW, winH), 0),
		bg,
		content,
	)
}

// ────────────────────────────────────────────────────────────────
//  Screen: Reader (Swipe Architecture Integration)
// ────────────────────────────────────────────────────────────────

func (r *ReaderApp) buildReaderScreen() fyne.CanvasObject {
	r.inReaderView = true

	r.textLabel = widget.NewLabel("")
	r.textLabel.Wrapping = fyne.TextWrapWord
	r.textLabel.Alignment = fyne.TextAlignLeading

	r.imageCanvas = canvas.NewImageFromImage(nil)
	r.imageCanvas.FillMode = canvas.ImageFillContain

	r.contentBox = container.NewMax(r.textLabel)

	optionsBtn := widget.NewButtonWithIcon("", theme.SettingsIcon(), func() {
		r.showOptionsDialog()
	})

	topBarLayout := container.NewBorder(nil, nil, nil, optionsBtn, container.NewCenter(canvas.NewText(" ", color.Transparent)))

	textClampedBlock := container.New(
		NewFixedLayout(fyne.NewSize(textBoxW-20, textBoxH-20), 0),
		r.contentBox,
	)

	cardContentLayout := container.New(
		NewFixedLayout(fyne.NewSize(textBoxW, textBoxH), 0),
		canvas.NewRectangle(color.Black),
		textClampedBlock,
	)

	gestureSurface := NewSwipeableCanvas(cardContentLayout,
		func() { r.turnPage(1) },
		func() { r.turnPage(-1) },
	)

	bg := canvas.NewRectangle(color.Black)

	inner := container.NewBorder(
		container.NewVBox(container.NewPadded(topBarLayout), widget.NewSeparator()),
		nil, nil, nil,
		gestureSurface,
	)

	// Save direct pointer reference to the custom layout instantiation
	r.readerLayout = NewFixedLayout(fyne.NewSize(winW, winH), r.trackingState.VerticalBias)

	return container.New(r.readerLayout, bg, inner)
}

// ────────────────────────────────────────────────────────────────
//  Configuration Overlay Options Form (With Spectrum Slider)
// ────────────────────────────────────────────────────────────────

func (r *ReaderApp) showOptionsDialog() {
	fontDown := widget.NewButton("A-", func() {
		if r.rt.fontSize > 11 {
			r.rt.fontSize -= 1
			r.myApp.Settings().SetTheme(r.rt)
			r.textLabel.Refresh()
		}
	})
	fontUp := widget.NewButton("A+", func() {
		if r.rt.fontSize < 30 {
			r.rt.fontSize += 1
			r.myApp.Settings().SetTheme(r.rt)
			r.textLabel.Refresh()
		}
	})
	sizeRow := container.NewGridWithColumns(2, fontDown, fontUp)

	boldCheck := widget.NewCheck("Bold Text", func(checked bool) {
		r.isBold = checked
		r.applyTypographyRules()
	})
	boldCheck.SetChecked(r.isBold)

	justifyCheck := widget.NewCheck("Justified Text", func(checked bool) {
		r.isJustified = checked
		if r.isJustified {
			r.textLabel.Alignment = fyne.TextAlignLeading
		} else {
			r.textLabel.Alignment = fyne.TextAlignCenter
		}
		r.render()
	})
	justifyCheck.SetChecked(r.isJustified)

	fontFaceSelect := widget.NewSelect([]string{"Sans-Serif", "Serif", "Monospace"}, func(chosen string) {
		r.currentFace = chosen
		switch chosen {
		case "Serif":
			r.rt.fontStyle.Monospace = false
		case "Monospace":
			r.rt.fontStyle.Monospace = true
		default:
			r.rt.fontStyle.Monospace = false
		}
		r.applyTypographyRules()
	})
	fontFaceSelect.SetSelected(r.currentFace)

	wordsEntry := widget.NewEntry()
	wordsEntry.SetText(strconv.Itoa(r.wordsPerPage))

	biasSlider := widget.NewSlider(0, 100)
	biasSlider.SetValue(float64(r.trackingState.VerticalBias))
	biasSlider.OnChanged = func(val float64) {
		r.trackingState.VerticalBias = float32(val)
		if r.readerLayout != nil {
			r.readerLayout.vBiasPct = float32(val)
			r.window.Content().Refresh() // Safely update layout constraints across the app frame
		}
	}

	optionsForm := widget.NewForm(
		widget.NewFormItem("Font Face", fontFaceSelect),
		widget.NewFormItem("Text Scaling", sizeRow),
		widget.NewFormItem("Format Style", container.NewVBox(boldCheck, justifyCheck)),
		widget.NewFormItem("Words Per Page", wordsEntry),
		widget.NewFormItem("Vertical Position (Top -> Bottom)", biasSlider),
	)

	d := dialog.NewCustom("Text Options", "Save & Apply", optionsForm, r.window)
	d.SetOnClosed(func() {
		if val, err := strconv.Atoi(strings.TrimSpace(wordsEntry.Text)); err == nil && val > 5 {
			if r.wordsPerPage != val {
				r.wordsPerPage = val
				r.chapCache = make(map[int][]PageContent)
				r.pages = r.chapPages(r.currentChap)
				if r.currentPage >= len(r.pages) {
					r.currentPage = 0
				}
				r.render()
			}
		}
		r.saveStateToDisk()
	})
	d.Resize(fyne.NewSize(320, 360))
	d.Show()
}

func (r *ReaderApp) applyTypographyRules() {
	r.rt.fontStyle.Bold = r.isBold
	r.textLabel.TextStyle = r.rt.fontStyle
	r.myApp.Settings().SetTheme(r.rt)
	r.textLabel.Refresh()
}

// ────────────────────────────────────────────────────────────────
//  Book loading
// ────────────────────────────────────────────────────────────────

func (r *ReaderApp) openBook(filename string) {
	if r.rc != nil {
		r.rc.Close()
		r.rc = nil
	}

	fullPath := filepath.Join(r.epubsDir, filename)
	rc, err := epub.OpenReader(fullPath)
	if err != nil {
		dialog.ShowError(fmt.Errorf("cannot open %s:\n%v", filename, err), r.window)
		return
	}
	if len(rc.Rootfiles) == 0 {
		dialog.ShowError(fmt.Errorf("invalid ePub: no root file"), r.window)
		rc.Close()
		return
	}

	book := rc.Rootfiles[0]
	manifestMap := make(map[string]epub.Item)
	for _, item := range book.Manifest.Items {
		manifestMap[item.ID] = item
	}

	var spinePaths []string
	for _, itemref := range book.Spine.Itemrefs {
		if item, ok := manifestMap[itemref.IDREF]; ok {
			spinePaths = append(spinePaths, item.HREF)
		}
	}

	r.rc = rc
	r.currentBook = filename
	r.spinePaths = spinePaths
	r.chapCache = make(map[int][]PageContent)

	screen := r.buildReaderScreen()
	r.window.SetContent(screen)

	savedProgress, found := r.trackingState.BookTracking[filename]
	if found {
		r.currentChap = savedProgress.Chapter
		r.pages = r.chapPages(r.currentChap)
		r.currentPage = savedProgress.Page
		if r.currentPage >= len(r.pages) {
			r.currentPage = 0
		}
		r.render()
	} else {
		r.loadChapter(0)
	}
}

// ────────────────────────────────────────────────────────────────
//  Navigation & Rendering
// ────────────────────────────────────────────────────────────────

func (r *ReaderApp) loadChapter(idx int) {
	if r.rc == nil || idx < 0 || idx >= len(r.spinePaths) {
		return
	}
	r.currentChap = idx
	r.pages = r.chapPages(idx)
	r.currentPage = 0
	r.render()
	r.saveStateToDisk()
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
			r.saveStateToDisk()
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
	r.saveStateToDisk()
}

func (r *ReaderApp) render() {
	if r.textLabel == nil || r.contentBox == nil {
		return
	}

	r.contentBox.Objects = nil

	if len(r.pages) == 0 || r.currentPage >= len(r.pages) {
		r.textLabel.SetText("(empty page)")
		r.contentBox.Add(r.textLabel)
		r.contentBox.Refresh()
		return
	}

	currentPageContent := r.pages[r.currentPage]

	if currentPageContent.IsImage {
		r.imageCanvas.Image = currentPageContent.ImgData
		r.contentBox.Add(r.imageCanvas)
		r.imageCanvas.Refresh()
	} else {
		text := currentPageContent.Text
		if r.isJustified {
			text = justifyTextBlock(text, int(textBoxW)/9)
		}
		r.textLabel.SetText(text)
		r.contentBox.Add(r.textLabel)
		r.textLabel.Refresh()
	}

	r.contentBox.Refresh()
}

// ────────────────────────────────────────────────────────────────
//  Paging & Parsing Logic
// ────────────────────────────────────────────────────────────────

func (r *ReaderApp) chapPages(idx int) []PageContent {
	if cached, ok := r.chapCache[idx]; ok {
		return cached
	}
	pages := r.parseAndPaginateChapter(idx)
	r.chapCache[idx] = pages
	return pages
}

func (r *ReaderApp) parseAndPaginateChapter(idx int) []PageContent {
	targetPath := r.spinePaths[idx]
	book := r.rc.Rootfiles[0]

	var targetItem *epub.Item
	for i := range book.Manifest.Items {
		if book.Manifest.Items[i].HREF == targetPath {
			targetItem = &book.Manifest.Items[i]
			break
		}
	}

	if targetItem == nil {
		return []PageContent{{IsImage: false, Text: "[Missing chapter]"}}
	}

	fd, err := targetItem.Open()
	if err != nil {
		return []PageContent{{IsImage: false, Text: "[Error opening chapter]"}}
	}
	defer fd.Close()

	buf := new(strings.Builder)
	_, _ = io.Copy(buf, fd)
	htmlContent := buf.String()

	imgRegexp := regexp.MustCompile(`(?i)<img\s+[^>]*src=["']([^"']+)["'][^>]*>`)
	matches := imgRegexp.FindAllStringSubmatch(htmlContent, -1)

	var dynamicPages []PageContent

	for _, match := range matches {
		imgSrc := match[1]
		baseDir := filepath.Dir(targetPath)
		resolvedImgPath := filepath.Clean(filepath.Join(baseDir, imgSrc))
		resolvedImgPath = strings.ReplaceAll(resolvedImgPath, "\\", "/")

		for _, item := range book.Manifest.Items {
			if item.HREF == resolvedImgPath {
				if imgFile, err := item.Open(); err == nil {
					imgBytes, _ := io.ReadAll(imgFile)
					imgFile.Close()
					if decodedImg, _, err := image.Decode(bytes.NewReader(imgBytes)); err == nil {
						dynamicPages = append(dynamicPages, PageContent{
							IsImage: true,
							ImgData: decodedImg,
						})
					}
				}
				break
			}
		}
	}

	plainText := htmlToPlainText(htmlContent)
	words := strings.Fields(plainText)

	if len(words) == 0 && len(dynamicPages) == 0 {
		return []PageContent{{IsImage: false, Text: "(empty chapter)"}}
	}

	for i := 0; i < len(words); i += r.wordsPerPage {
		end := i + r.wordsPerPage
		if end > len(words) {
			end = len(words)
		}
		dynamicPages = append(dynamicPages, PageContent{
			IsImage: false,
			Text:     strings.Join(words[i:end], " "),
		})
	}

	return dynamicPages
}

func justifyTextBlock(text string, targetLineWidth int) string {
	words := strings.Fields(text)
	if len(words) == 0 || targetLineWidth <= 0 {
		return text
	}

	var result strings.Builder
	var currentLine []string
	currentLen := 0

	for _, w := range words {
		if currentLen+len(currentLine)+len(w) > targetLineWidth {
			result.WriteString(fillLineSpaces(currentLine, currentLen, targetLineWidth) + "\n")
			currentLine = []string{w}
			currentLen = len(w)
		} else {
			currentLine = append(currentLine, w)
			currentLen += len(w)
		}
	}
	if len(currentLine) > 0 {
		result.WriteString(strings.Join(currentLine, " "))
	}
	return result.String()
}

func fillLineSpaces(words []string, currentLen, targetWidth int) string {
	slots := len(words) - 1
	if slots <= 0 {
		return strings.Join(words, "")
	}

	totalSpacesNeeded := targetWidth - currentLen
	baseSpaces := totalSpacesNeeded / slots
	remainder := totalSpacesNeeded % slots

	var s strings.Builder
	for i, w := range words {
		s.WriteString(w)
		if i < slots {
			spacesToInsert := baseSpaces
			if i < remainder {
				spacesToInsert++
			}
			s.WriteString(strings.Repeat(" ", spacesToInsert))
		}
	}
	return s.String()
}

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

func reBlock(pattern string) *regexp.Regexp {
	return regexp.MustCompile(`(?i)` + pattern)
}

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

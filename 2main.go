package main

import (
	"fmt"
	"html"
	"io"
	"regexp"
	"strings"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/app"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"
	"github.com/taylorskalyo/goreader/epub"
)

// ─────────────────────────────────────────────
//  Fixed-size layout
//  Forces its single child to exactly `size`,
//  and reports that same size as MinSize so the
//  window cannot be pushed open by label text.
// ─────────────────────────────────────────────

type fixedLayout struct{ size fyne.Size }

func (f fixedLayout) Layout(objs []fyne.CanvasObject, _ fyne.Size) {
	for _, o := range objs {
		o.Move(fyne.NewPos(0, 0))
		o.Resize(f.size)
	}
}
func (f fixedLayout) MinSize(_ []fyne.CanvasObject) fyne.Size { return f.size }

// ─────────────────────────────────────────────
//  Theme — B&W only, adjustable font size
// ─────────────────────────────────────────────

type ReaderTheme struct {
	fyne.Theme
	fontSize float32
}

func (t *ReaderTheme) Size(name fyne.ThemeSizeName) float32 {
	if name == theme.SizeNameText {
		return t.fontSize
	}
	return t.Theme.Size(name)
}

// ─────────────────────────────────────────────
//  Constants
// ─────────────────────────────────────────────

const (
	wordsPerPage = 20

	winW float32 = 380
	winH float32 = 280

	// text box sits between the two toolbars
	textW float32 = winW - 16 // 8px padding each side
	textH float32 = 160
)

// ─────────────────────────────────────────────
//  App State
// ─────────────────────────────────────────────

type ReaderApp struct {
	rc          *epub.ReadCloser
	spinePaths  []string
	currentChap int
	pages       []string
	currentPage int

	// global page index across entire book
	chapPageOffset []int // chapPageOffset[i] = first global page of chapter i
	totalPages     int

	myApp       fyne.App
	window      fyne.Window
	rt          *ReaderTheme
	textLabel   *widget.Label
	statusLabel *widget.Label

	chapCache map[int][]string
}

// ─────────────────────────────────────────────
//  Entry Point
// ─────────────────────────────────────────────

func main() {
	myApp := app.New()
	myWindow := myApp.NewWindow("Reader")

	rc, err := epub.OpenReader("sample.epub")
	if err != nil {
		panic("Cannot open sample.epub: " + err.Error())
	}
	if len(rc.Rootfiles) == 0 {
		panic("Invalid ePub: no root file found.")
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

	rt := &ReaderTheme{Theme: theme.LightTheme(), fontSize: 18}
	myApp.Settings().SetTheme(rt)

	textLabel := widget.NewLabel("")
	textLabel.Wrapping = fyne.TextWrapWord
	textLabel.Alignment = fyne.TextAlignLeading

	statusLabel := widget.NewLabel("")
	statusLabel.Alignment = fyne.TextAlignCenter
	statusLabel.TextStyle = fyne.TextStyle{Italic: true}

	state := &ReaderApp{
		rc:          rc,
		spinePaths:  spinePaths,
		myApp:       myApp,
		window:      myWindow,
		rt:          rt,
		textLabel:   textLabel,
		statusLabel: statusLabel,
		chapCache:   make(map[int][]string),
	}

	// Must set content BEFORE Resize so Fyne measures correctly
	myWindow.SetContent(state.buildUI())
	myWindow.SetFixedSize(true)
	myWindow.Resize(fyne.NewSize(winW, winH))
	myWindow.CenterOnScreen()

	state.loadChapter(0)
	myWindow.ShowAndRun()
	rc.Close()
}

// ─────────────────────────────────────────────
//  UI Construction
// ─────────────────────────────────────────────

func (r *ReaderApp) buildUI() fyne.CanvasObject {
	fontDown := widget.NewButton("A-", func() {
		if r.rt.fontSize > 12 {
			r.rt.fontSize -= 2
			r.myApp.Settings().SetTheme(r.rt)
		}
	})
	fontUp := widget.NewButton("A+", func() {
		if r.rt.fontSize < 32 {
			r.rt.fontSize += 2
			r.myApp.Settings().SetTheme(r.rt)
		}
	})
	prevChap := widget.NewButton("« Ch", func() { r.loadChapter(r.currentChap - 1) })
	nextChap := widget.NewButton("Ch »", func() { r.loadChapter(r.currentChap + 1) })

	topBar := container.NewBorder(nil, nil,
		container.NewHBox(fontDown, fontUp),
		container.NewHBox(prevChap, nextChap),
		nil,
	)

	prevPage := widget.NewButton("◀", func() { r.turnPage(-1) })
	nextPage := widget.NewButton("▶", func() { r.turnPage(1) })

	bottomBar := container.NewBorder(nil, nil,
		prevPage, nextPage,
		container.NewCenter(r.statusLabel),
	)

	// Clamp the text label to a fixed box — this is what keeps the window fixed.
	// Without this, TextWrapWord makes the label report a huge MinSize.
	textBox := container.New(fixedLayout{fyne.NewSize(textW, textH)}, r.textLabel)

	return container.NewBorder(
		container.NewVBox(topBar, widget.NewSeparator()),
		container.NewVBox(widget.NewSeparator(), bottomBar),
		nil, nil,
		container.NewCenter(textBox),
	)
}

// ─────────────────────────────────────────────
//  Navigation
// ─────────────────────────────────────────────

func (r *ReaderApp) loadChapter(idx int) {
	if idx < 0 || idx >= len(r.spinePaths) {
		return
	}
	r.currentChap = idx
	r.pages = r.chapPages(idx)
	r.currentPage = 0
	r.render()
}

func (r *ReaderApp) turnPage(delta int) {
	next := r.currentPage + delta
	if next < 0 {
		if r.currentChap > 0 {
			r.currentChap--
			r.pages = r.chapPages(r.currentChap)
			r.currentPage = len(r.pages) - 1
			r.render()
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
}

func (r *ReaderApp) render() {
	text := "(empty)"
	if len(r.pages) > 0 && r.currentPage < len(r.pages) {
		text = r.pages[r.currentPage]
	}
	r.textLabel.SetText(text)

	// Global page = sum of all pages in previous chapters + currentPage
	global := r.currentPage + 1
	for i := 0; i < r.currentChap; i++ {
		global += len(r.chapPages(i))
	}
	globalTotal := 0
	for i := range r.spinePaths {
		globalTotal += len(r.chapPages(i))
	}

	r.statusLabel.SetText(fmt.Sprintf("p %d / %d", global, globalTotal))
	r.window.SetTitle(fmt.Sprintf("Reader  —  p %d / %d", global, globalTotal))
}

// ─────────────────────────────────────────────
//  Paging  (word-based, 10 words per page)
// ─────────────────────────────────────────────

func (r *ReaderApp) chapPages(idx int) []string {
	if cached, ok := r.chapCache[idx]; ok {
		return cached
	}
	raw := r.readChapter(idx)
	pages := paginateWords(raw, wordsPerPage)
	r.chapCache[idx] = pages
	return pages
}

// paginateWords splits text into pages of exactly n words each.
func paginateWords(text string, n int) []string {
	words := strings.Fields(text) // splits on any whitespace, drops empties
	if len(words) == 0 {
		return []string{"(empty chapter)"}
	}

	var pages []string
	for i := 0; i < len(words); i += n {
		end := i + n
		if end > len(words) {
			end = len(words)
		}
		pages = append(pages, strings.Join(words[i:end], " "))
	}
	return pages
}

// ─────────────────────────────────────────────
//  ePub Reading
// ─────────────────────────────────────────────

func (r *ReaderApp) readChapter(idx int) string {
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
		return fmt.Sprintf("[Could not locate: %s]", targetPath)
	}

	fd, err := targetItem.Open()
	if err != nil {
		return fmt.Sprintf("[Error opening: %v]", err)
	}
	defer fd.Close()

	buf := new(strings.Builder)
	if _, err = io.Copy(buf, fd); err != nil {
		return fmt.Sprintf("[Error reading: %v]", err)
	}
	return htmlToPlainText(buf.String())
}

// ─────────────────────────────────────────────
//  HTML → Plain Text
// ─────────────────────────────────────────────

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

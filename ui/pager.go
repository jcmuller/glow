package ui

import (
	"fmt"
	"math"
	"path/filepath"
	"strings"
	"time"

	"github.com/atotto/clipboard"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/glamour"
	"github.com/charmbracelet/glow/v2/utils"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/log"
	"github.com/fsnotify/fsnotify"
	runewidth "github.com/mattn/go-runewidth"
	"github.com/muesli/reflow/ansi"
	"github.com/muesli/reflow/truncate"
	"github.com/muesli/termenv"
)

const (
	statusBarHeight = 1
	lineNumberWidth = 4
)

var (
	pagerHelpHeight int

	mintGreen = lipgloss.AdaptiveColor{Light: "#89F0CB", Dark: "#89F0CB"}
	darkGreen = lipgloss.AdaptiveColor{Light: "#1C8760", Dark: "#1C8760"}

	lineNumberFg = lipgloss.AdaptiveColor{Light: "#656565", Dark: "#7D7D7D"}

	statusBarNoteFg = lipgloss.AdaptiveColor{Light: "#656565", Dark: "#7D7D7D"}
	statusBarBg     = lipgloss.AdaptiveColor{Light: "#E6E6E6", Dark: "#242424"}

	statusBarScrollPosStyle = lipgloss.NewStyle().
				Foreground(lipgloss.AdaptiveColor{Light: "#949494", Dark: "#5A5A5A"}).
				Background(statusBarBg).
				Render

	statusBarNoteStyle = lipgloss.NewStyle().
				Foreground(statusBarNoteFg).
				Background(statusBarBg).
				Render

	statusBarHelpStyle = lipgloss.NewStyle().
				Foreground(statusBarNoteFg).
				Background(lipgloss.AdaptiveColor{Light: "#DCDCDC", Dark: "#323232"}).
				Render

	statusBarMessageStyle = lipgloss.NewStyle().
				Foreground(mintGreen).
				Background(darkGreen).
				Render

	statusBarMessageScrollPosStyle = lipgloss.NewStyle().
					Foreground(mintGreen).
					Background(darkGreen).
					Render

	statusBarMessageHelpStyle = lipgloss.NewStyle().
					Foreground(lipgloss.Color("#B6FFE4")).
					Background(green).
					Render

	helpViewStyle = lipgloss.NewStyle().
			Foreground(statusBarNoteFg).
			Background(lipgloss.AdaptiveColor{Light: "#f2f2f2", Dark: "#1B1B1B"}).
			Render

	lineNumberStyle = lipgloss.NewStyle().
			Foreground(lineNumberFg).
			Render
)

type (
	contentRenderedMsg string
	reloadMsg          struct{}
)

// slide is one slide's rendered body and any speaker notes that
// followed a bare `???` line in the source.
type slide struct {
	body  string
	notes string
}

type pagerState int

const (
	pagerStateBrowse pagerState = iota
	pagerStateStatusMessage
)

type pagerModel struct {
	common   *commonModel
	viewport viewport.Model
	state    pagerState
	showHelp bool

	statusMessage      string
	statusMessageTimer *time.Timer

	// Current document being rendered, sans-glamour rendering. We cache
	// it here so we can re-render it on resize.
	currentDocument markdown

	watcher *fsnotify.Watcher

	// Slide navigation: track slides and current position
	slides              []slide // Each slide's body + optional speaker notes
	currentSlide        int     // Current slide index (0-based)
	slideMode           bool    // Whether we're in slide presentation mode
	originalContent     string  // Full document content
	renderedContent     string  // For backwards compatibility
	resetScrollPosition bool    // Track if we should reset scroll position on next render
}

func newPagerModel(common *commonModel) pagerModel {
	// Init viewport
	vp := viewport.New(0, 0)
	vp.YPosition = 0
	vp.HighPerformanceRendering = config.HighPerformancePager

	m := pagerModel{
		common:   common,
		state:    pagerStateBrowse,
		viewport: vp,
	}
	m.initWatcher()
	return m
}

func (m *pagerModel) setSize(w, h int) {
	m.viewport.Width = w
	m.viewport.Height = h - statusBarHeight

	if m.showHelp {
		if pagerHelpHeight == 0 {
			pagerHelpHeight = strings.Count(m.helpView(), "\n")
		}
		m.viewport.Height -= (statusBarHeight + pagerHelpHeight)
	}
}

func (m *pagerModel) setContent(s string) {
	m.viewport.SetContent(s)
	m.renderedContent = s
}

func (m *pagerModel) toggleHelp() {
	m.showHelp = !m.showHelp
	m.setSize(m.common.width, m.common.height)
	if m.viewport.PastBottom() {
		m.viewport.GotoBottom()
	}
}

type pagerStatusMessage struct {
	message string
	isError bool
}

// Perform stuff that needs to happen after a successful markdown stash. Note
// that the returned command should be sent back the through the pager
// update function.
func (m *pagerModel) showStatusMessage(msg pagerStatusMessage) tea.Cmd {
	// Show a success message to the user
	m.state = pagerStateStatusMessage
	m.statusMessage = msg.message
	if m.statusMessageTimer != nil {
		m.statusMessageTimer.Stop()
	}
	m.statusMessageTimer = time.NewTimer(statusMessageTimeout)

	return waitForStatusMessageTimeout(pagerContext, m.statusMessageTimer)
}

func (m *pagerModel) unload() {
	log.Debug("unload")
	if m.showHelp {
		m.toggleHelp()
	}
	if m.statusMessageTimer != nil {
		m.statusMessageTimer.Stop()
	}
	m.state = pagerStateBrowse
	m.viewport.SetContent("")
	m.viewport.YOffset = 0
	m.unwatchFile()

	// Reset slide mode
	m.slides = nil
	m.slideMode = false
	m.currentSlide = 0
	m.originalContent = ""
}

func (m pagerModel) update(msg tea.Msg) (pagerModel, tea.Cmd) {
	var (
		cmd  tea.Cmd
		cmds []tea.Cmd
	)

	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "q", keyEsc:
			if m.state != pagerStateBrowse {
				m.state = pagerStateBrowse
				return m, nil
			}
		case "home", "g":
			m.viewport.GotoTop()
			if m.viewport.HighPerformanceRendering {
				cmds = append(cmds, viewport.Sync(m.viewport))
			}
		case "end", "G":
			m.viewport.GotoBottom()
			if m.viewport.HighPerformanceRendering {
				cmds = append(cmds, viewport.Sync(m.viewport))
			}

		case "d":
			m.viewport.HalfViewDown()
			if m.viewport.HighPerformanceRendering {
				cmds = append(cmds, viewport.Sync(m.viewport))
			}

		case "u":
			m.viewport.HalfViewUp()
			if m.viewport.HighPerformanceRendering {
				cmds = append(cmds, viewport.Sync(m.viewport))
			}

		case "e":
			lineno := int(math.RoundToEven(float64(m.viewport.TotalLineCount()) * m.viewport.ScrollPercent()))
			if m.viewport.AtTop() {
				lineno = 0
			}
			log.Info(
				"opening editor",
				"file", m.currentDocument.localPath,
				"line", fmt.Sprintf("%d/%d", lineno, m.viewport.TotalLineCount()),
			)
			return m, openEditor(m.currentDocument.localPath, lineno)

		case "c":
			// Copy using OSC 52
			termenv.Copy(m.currentDocument.Body)
			// Copy using native system clipboard
			_ = clipboard.WriteAll(m.currentDocument.Body)
			cmds = append(cmds, m.showStatusMessage(pagerStatusMessage{"Copied contents", false}))

		case "r":
			return m, loadLocalMarkdown(&m.currentDocument)

		case "?":
			m.toggleHelp()
			if m.viewport.HighPerformanceRendering {
				cmds = append(cmds, viewport.Sync(m.viewport))
			}

		case "n", "right", keySpace:
			if cmd := m.nextPage(); cmd != nil {
				cmds = append(cmds, cmd)
			}

		case "p", "left", "backspace":
			if cmd := m.previousPage(); cmd != nil {
				cmds = append(cmds, cmd)
			}
		}

	// Glow has rendered the content
	case contentRenderedMsg:
		log.Info("content rendered", "state", m.state)

		m.setContent(string(msg))

		// Reset scroll position if we just switched slides
		if m.resetScrollPosition {
			m.viewport.YOffset = 0
			m.resetScrollPosition = false
		}

		if m.viewport.HighPerformanceRendering {
			cmds = append(cmds, viewport.Sync(m.viewport))
		}
		cmds = append(cmds, m.watchFile)

	// The file was changed on disk and we're reloading it
	case reloadMsg:
		m.slides = nil
		m.slideMode = false
		m.currentSlide = 0
		return m, loadLocalMarkdown(&m.currentDocument)

	// We've finished editing the document, potentially making changes. Let's
	// retrieve the latest version of the document so that we display
	// up-to-date contents.
	case editorFinishedMsg:
		m.slides = nil
		m.slideMode = false
		m.currentSlide = 0
		return m, loadLocalMarkdown(&m.currentDocument)

	// We've received terminal dimensions, either for the first time or
	// after a resize
	case tea.WindowSizeMsg:
		// Parse slides if we haven't already and presentation mode is enabled
		if len(m.slides) == 0 && m.currentDocument.Body != "" {
			m.parseSlides()
		}

		// Render the current slide if in slide mode, otherwise full content
		if m.slideMode && len(m.slides) > 0 {
			return m, renderWithGlamour(m, m.slides[m.currentSlide].body)
		}
		return m, renderWithGlamour(m, m.currentDocument.Body)

	case statusMessageTimeoutMsg:
		m.state = pagerStateBrowse
	}

	m.viewport, cmd = m.viewport.Update(msg)
	cmds = append(cmds, cmd)

	return m, tea.Batch(cmds...)
}

func (m pagerModel) View() string {
	var b strings.Builder
	fmt.Fprint(&b, m.viewport.View()+"\n")

	// Footer
	m.statusBarView(&b)

	if m.showHelp {
		fmt.Fprint(&b, "\n"+m.helpView())
	}

	return b.String()
}

func (m pagerModel) statusBarView(b *strings.Builder) {
	const (
		minPercent               float64 = 0.0
		maxPercent               float64 = 1.0
		percentToStringMagnitude float64 = 100.0
	)

	showStatusMessage := m.state == pagerStateStatusMessage

	// Logo
	logo := glowLogoView()

	// Scroll percent
	percent := math.Max(minPercent, math.Min(maxPercent, m.viewport.ScrollPercent()))
	scrollPercent := fmt.Sprintf(" %3.f%% ", percent*percentToStringMagnitude)
	if showStatusMessage {
		scrollPercent = statusBarMessageScrollPosStyle(scrollPercent)
	} else {
		scrollPercent = statusBarScrollPosStyle(scrollPercent)
	}

	// "Help" note
	var helpNote string
	if showStatusMessage {
		helpNote = statusBarMessageHelpStyle(" ? Help ")
	} else {
		helpNote = statusBarHelpStyle(" ? Help ")
	}

	// Note
	var note string
	if showStatusMessage {
		note = m.statusMessage
	} else {
		note = m.currentDocument.Note
		// Add slide indicator if in slide mode
		if m.slideMode && len(m.slides) > 0 {
			slideIndicator := fmt.Sprintf(" [Slide %d/%d]", m.currentSlide+1, len(m.slides))
			note = note + slideIndicator
		}
	}
	note = truncate.StringWithTail(" "+note+" ", uint(max(0, //nolint:gosec
		m.common.width-
			ansi.PrintableRuneWidth(logo)-
			ansi.PrintableRuneWidth(scrollPercent)-
			ansi.PrintableRuneWidth(helpNote),
	)), ellipsis)
	if showStatusMessage {
		note = statusBarMessageStyle(note)
	} else {
		note = statusBarNoteStyle(note)
	}

	// Empty space
	padding := max(0,
		m.common.width-
			ansi.PrintableRuneWidth(logo)-
			ansi.PrintableRuneWidth(note)-
			ansi.PrintableRuneWidth(scrollPercent)-
			ansi.PrintableRuneWidth(helpNote),
	)
	emptySpace := strings.Repeat(" ", padding)
	if showStatusMessage {
		emptySpace = statusBarMessageStyle(emptySpace)
	} else {
		emptySpace = statusBarNoteStyle(emptySpace)
	}

	fmt.Fprintf(b, "%s%s%s%s%s",
		logo,
		note,
		emptySpace,
		scrollPercent,
		helpNote,
	)
}

func (m pagerModel) helpView() (s string) {
	col1 := []string{
		"g/home  go to top",
		"G/end   go to bottom",
		"n       next slide",
		"p       previous slide",
		"c       copy contents",
		"e       edit this document",
		"r       reload this document",
		"esc     back to files",
		"q       quit",
	}

	s += "\n"
	s += "k/↑      up                  " + col1[0] + "\n"
	s += "j/↓      down                " + col1[1] + "\n"
	s += "b/pgup   page up             " + col1[2] + "\n"
	s += "f/pgdn   page down           " + col1[3] + "\n"
	s += "u        ½ page up           " + col1[4] + "\n"
	s += "d        ½ page down         " + col1[5] + "\n"
	s += "                             " + col1[6]

	if len(col1) > 7 {
		s += "\n                             " + col1[7]
	}
	if len(col1) > 8 {
		s += "\n                             " + col1[8]
	}

	s = indent(s, 2)

	// Fill up empty cells with spaces for background coloring
	if m.common.width > 0 {
		lines := strings.Split(s, "\n")
		for i := 0; i < len(lines); i++ {
			l := runewidth.StringWidth(lines[i])
			n := max(m.common.width-l, 0)
			lines[i] += strings.Repeat(" ", n)
		}

		s = strings.Join(lines, "\n")
	}

	return helpViewStyle(s)
}

// parseSlides runs splitSlides against the current document body and
// flips the model into slide mode when slides came back.
func (m *pagerModel) parseSlides() {
	m.slides = nil
	m.slideMode = false

	if !m.common.cfg.PresentationMode {
		return
	}
	if m.currentDocument.Body == "" {
		return
	}

	m.slides = splitSlides(m.currentDocument.Body)
	if len(m.slides) > 0 {
		m.slideMode = true
		m.currentSlide = 0
		m.originalContent = m.currentDocument.Body
		log.Info("slide mode enabled", "slides", len(m.slides))
	} else {
		log.Debug("no slide separators found - slide mode disabled")
	}
}

// splitSlides breaks a markdown body into slides. A bare `---` line
// outside a fenced code block switches on thematic-break mode; with no
// such line, splitSlides falls back to splitting on numbered H1
// headers (`# 1. Foo`, `# 2. Bar`), discarding any preamble before the
// first numbered header. Each slide has its speaker notes, introduced
// by a bare `???` or `Notes:` line, moved into slide.notes.
func splitSlides(body string) []slide {
	if body == "" {
		return nil
	}

	lines := strings.Split(body, "\n")

	var bodies []string
	if hasThematicBreak(lines) {
		bodies = splitOnThematicBreaks(lines)
	} else {
		bodies = splitOnNumberedH1(lines)
	}

	if len(bodies) == 0 {
		return nil
	}

	out := make([]slide, 0, len(bodies))
	for _, b := range bodies {
		bodyPart, notesPart := extractNotes(b)
		out = append(out, slide{body: bodyPart, notes: notesPart})
	}
	return out
}

// isFenceLine reports whether a trimmed line opens or closes a fenced
// code block (``` or ~~~, 3+ chars, optionally with an info string).
func isFenceLine(trimmed string) bool {
	return strings.HasPrefix(trimmed, "```") || strings.HasPrefix(trimmed, "~~~")
}

func hasThematicBreak(lines []string) bool {
	inFence := false
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if isFenceLine(trimmed) {
			inFence = !inFence
			continue
		}
		if !inFence && trimmed == "---" {
			return true
		}
	}
	return false
}

func splitOnThematicBreaks(lines []string) []string {
	var result []string
	var cur []string
	inFence := false
	flush := func() {
		joined := joinTrimEmpty(cur)
		if joined != "" {
			result = append(result, joined)
		}
		cur = nil
	}
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if isFenceLine(trimmed) {
			inFence = !inFence
			cur = append(cur, line)
			continue
		}
		if !inFence && trimmed == "---" {
			flush()
			continue
		}
		cur = append(cur, line)
	}
	flush()
	return result
}

// joinTrimEmpty joins lines with newline, discarding leading and
// trailing lines whose trimmed content is empty.
func joinTrimEmpty(lines []string) string {
	start := 0
	for start < len(lines) && strings.TrimSpace(lines[start]) == "" {
		start++
	}
	end := len(lines)
	for end > start && strings.TrimSpace(lines[end-1]) == "" {
		end--
	}
	if start >= end {
		return ""
	}
	return strings.Join(lines[start:end], "\n")
}

// splitOnNumberedH1 preserves the original presentation-mode behavior:
// any content before the first `# <digit>` header is discarded, and
// each subsequent numbered H1 starts a new slide.
func splitOnNumberedH1(lines []string) []string {
	var result []string
	var cur []string
	foundFirst := false
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		isNumberedH1 := false
		if after, ok := strings.CutPrefix(trimmed, "# "); ok {
			headerText := strings.TrimSpace(after)
			if len(headerText) > 0 && headerText[0] >= '0' && headerText[0] <= '9' {
				isNumberedH1 = true
				foundFirst = true
			}
		}
		if isNumberedH1 && len(cur) > 0 {
			result = append(result, strings.Join(cur, "\n"))
			cur = nil
		}
		if foundFirst {
			cur = append(cur, line)
		}
	}
	if len(cur) > 0 {
		result = append(result, strings.Join(cur, "\n"))
	}
	return result
}

// isNotesMarker recognizes the notes separators. A line whose trimmed
// content is exactly `???` or exactly `Notes:` starts speaker notes.
func isNotesMarker(trimmed string) bool {
	return trimmed == "???" || trimmed == "Notes:"
}

// extractNotes splits a slide body at the first notes marker that sits
// outside a fenced code block. The first marker wins, so any later
// `???` or `Notes:` line is kept verbatim inside the notes.
func extractNotes(body string) (string, string) {
	lines := strings.Split(body, "\n")
	inFence := false
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if isFenceLine(trimmed) {
			inFence = !inFence
			continue
		}
		if !inFence && isNotesMarker(trimmed) {
			bodyOut := strings.TrimRight(strings.Join(lines[:i], "\n"), "\n")
			notesOut := strings.TrimSpace(strings.Join(lines[i+1:], "\n"))
			return bodyOut, notesOut
		}
	}
	return body, ""
}

// nextPage navigates to the next slide.
func (m *pagerModel) nextPage() tea.Cmd {
	if !m.slideMode {
		m.parseSlides()
	}

	if !m.slideMode || len(m.slides) == 0 {
		log.Debug("no slides found for navigation")
		return nil
	}

	if m.currentSlide < len(m.slides)-1 {
		m.currentSlide++
		m.resetScrollPosition = true
		log.Debug("navigating to next slide", "slide", m.currentSlide+1, "total", len(m.slides))
		return renderWithGlamour(*m, m.slides[m.currentSlide].body)
	}

	log.Debug("already at last slide")
	return nil
}

// previousPage navigates to the previous slide.
func (m *pagerModel) previousPage() tea.Cmd {
	if !m.slideMode {
		m.parseSlides()
	}

	if !m.slideMode || len(m.slides) == 0 {
		log.Debug("no slides found for navigation")
		return nil
	}

	if m.currentSlide > 0 {
		m.currentSlide--
		m.resetScrollPosition = true
		log.Debug("navigating to previous slide", "slide", m.currentSlide+1, "total", len(m.slides))
		return renderWithGlamour(*m, m.slides[m.currentSlide].body)
	}

	log.Debug("already at first slide")
	return nil
}

// COMMANDS

func renderWithGlamour(m pagerModel, md string) tea.Cmd {
	return func() tea.Msg {
		s, err := glamourRender(m, md)
		if err != nil {
			log.Error("error rendering with Glamour", "error", err)
			return errMsg{err}
		}
		return contentRenderedMsg(s)
	}
}

// This is where the magic happens.
func glamourRender(m pagerModel, markdown string) (string, error) {
	trunc := lipgloss.NewStyle().MaxWidth(m.viewport.Width - lineNumberWidth).Render

	if !config.GlamourEnabled {
		return markdown, nil
	}

	isCode := !utils.IsMarkdownFile(m.currentDocument.Note)
	width := max(0, min(int(m.common.cfg.GlamourMaxWidth), m.viewport.Width)) //nolint:gosec
	if isCode {
		width = 0
	}

	options := []glamour.TermRendererOption{
		utils.GlamourStyle(m.common.cfg.GlamourStyle, isCode),
		glamour.WithWordWrap(width),
	}

	if m.common.cfg.PreserveNewLines {
		options = append(options, glamour.WithPreservedNewLines())
	}
	r, err := glamour.NewTermRenderer(options...)
	if err != nil {
		return "", fmt.Errorf("error creating glamour renderer: %w", err)
	}

	if isCode {
		markdown = utils.WrapCodeBlock(markdown, filepath.Ext(m.currentDocument.Note))
	}

	out, err := r.Render(markdown)
	if err != nil {
		return "", fmt.Errorf("error rendering markdown: %w", err)
	}

	if isCode {
		out = strings.TrimSpace(out)
	}

	// trim lines
	lines := strings.Split(out, "\n")

	var content strings.Builder
	for i, s := range lines {
		if isCode || m.common.cfg.ShowLineNumbers {
			content.WriteString(lineNumberStyle(fmt.Sprintf("%"+fmt.Sprint(lineNumberWidth)+"d", i+1)))
			content.WriteString(trunc(s))
		} else {
			content.WriteString(s)
		}

		// don't add an artificial newline after the last split
		if i+1 < len(lines) {
			content.WriteRune('\n')
		}
	}

	return content.String(), nil
}

func (m *pagerModel) initWatcher() {
	var err error
	m.watcher, err = fsnotify.NewWatcher()
	if err != nil {
		log.Error("error creating fsnotify watcher", "error", err)
	}
}

func (m *pagerModel) watchFile() tea.Msg {
	dir := m.localDir()

	if err := m.watcher.Add(dir); err != nil {
		log.Error("error adding dir to fsnotify watcher", "error", err)
		return nil
	}

	log.Info("fsnotify watching dir", "dir", dir)

	for {
		select {
		case event, ok := <-m.watcher.Events:
			if !ok || event.Name != m.currentDocument.localPath {
				continue
			}

			if !event.Has(fsnotify.Write) && !event.Has(fsnotify.Create) {
				continue
			}

			log.Debug("fsnotify event", "file", event.Name, "event", event.Op)
			return reloadMsg{}
		case err, ok := <-m.watcher.Errors:
			if !ok {
				continue
			}
			log.Debug("fsnotify error", "dir", dir, "error", err)
		}
	}
}

func (m *pagerModel) unwatchFile() {
	dir := m.localDir()

	err := m.watcher.Remove(dir)
	if err == nil {
		log.Debug("fsnotify dir unwatched", "dir", dir)
	} else {
		log.Error("fsnotify fail to unwatch dir", "dir", dir, "error", err)
	}
}

func (m *pagerModel) localDir() string {
	return filepath.Dir(m.currentDocument.localPath)
}

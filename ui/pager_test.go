package ui

import (
	"reflect"
	"strings"
	"testing"

	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
)

// newTestPagerModel builds a pagerModel without calling newPagerModel
// so tests avoid the fsnotify watcher and Bubble Tea program loop.
func newTestPagerModel(cfg Config) pagerModel {
	return pagerModel{
		common:   &commonModel{cfg: cfg, width: 80, height: 24},
		viewport: viewport.New(0, 0),
	}
}

func TestPagerUpdateSlideNavigationKeys(t *testing.T) {
	newModel := func(start int) pagerModel {
		return pagerModel{
			common:   &commonModel{cfg: Config{PresentationMode: true}, width: 80, height: 24},
			viewport: viewport.New(0, 0),
			slides:   []slide{{body: "A"}, {body: "B"}},

			slideMode:    true,
			currentSlide: start,
		}
	}

	nextCases := []struct {
		name string
		key  tea.KeyMsg
		want int
	}{
		{"space advances", tea.KeyMsg{Type: tea.KeySpace}, 1},
		{"n advances", tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'n'}}, 1},
		{"right arrow advances", tea.KeyMsg{Type: tea.KeyRight}, 1},
	}

	for _, tc := range nextCases {
		t.Run("next/"+tc.name, func(t *testing.T) {
			next, _ := newModel(0).update(tc.key)
			if next.currentSlide != tc.want {
				t.Errorf("currentSlide after %q = %d, want %d",
					tc.key.String(), next.currentSlide, tc.want)
			}
		})
	}

	prevCases := []struct {
		name string
		key  tea.KeyMsg
		want int
	}{
		{"p retreats", tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'p'}}, 0},
		{"left arrow retreats", tea.KeyMsg{Type: tea.KeyLeft}, 0},
		{"backspace retreats", tea.KeyMsg{Type: tea.KeyBackspace}, 0},
	}

	for _, tc := range prevCases {
		t.Run("prev/"+tc.name, func(t *testing.T) {
			prev, _ := newModel(1).update(tc.key)
			if prev.currentSlide != tc.want {
				t.Errorf("currentSlide after %q = %d, want %d",
					tc.key.String(), prev.currentSlide, tc.want)
			}
		})
	}
}

func TestSetSizeReservesSpeakerNotesHeight(t *testing.T) {
	const termH = 24
	cases := []struct {
		name string
		cfg  Config
		want int
	}{
		{
			name: "presenter reserves notes height",
			cfg:  Config{SpeakerPresenter: true},
			want: termH - statusBarHeight - speakerNotesHeight,
		},
		{
			name: "viewer does not reserve",
			cfg:  Config{SpeakerViewer: true},
			want: termH - statusBarHeight,
		},
		{
			name: "neither reserves",
			cfg:  Config{},
			want: termH - statusBarHeight,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := newTestPagerModel(tc.cfg)
			m.setSize(80, termH)
			if got := m.viewport.Height; got != tc.want {
				t.Errorf("viewport.Height = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestViewSpeakerNotesPanelIsPresenterOnly(t *testing.T) {
	build := func(cfg Config) pagerModel {
		m := newTestPagerModel(cfg)
		m.common.width = 40
		m.common.height = 20
		m.viewport = viewport.New(40, 10)
		m.slides = []slide{{body: "# One", notes: "hello notes"}}
		m.slideMode = true
		return m
	}

	cases := []struct {
		name      string
		cfg       Config
		wantPanel bool
	}{
		{"presenter shows notes panel", Config{SpeakerPresenter: true}, true},
		{"viewer hides notes panel", Config{SpeakerViewer: true}, false},
		{"neither hides notes panel", Config{}, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := build(tc.cfg).View()
			hasPanel := strings.Contains(out, "NOTES:")
			if hasPanel != tc.wantPanel {
				t.Errorf("NOTES panel visible = %v, want %v\noutput:\n%s",
					hasPanel, tc.wantPanel, out)
			}
		})
	}
}

func TestSplitSlides(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want []slide
	}{
		{
			name: "numbered h1 fallback with two slides",
			in:   "# 1. Foo\nfoo body\n# 2. Bar\nbar body",
			want: []slide{
				{body: "# 1. Foo\nfoo body"},
				{body: "# 2. Bar\nbar body"},
			},
		},
		{
			name: "numbered h1 drops preamble before first header",
			in:   "preamble text\n# 1. Only\nhello",
			want: []slide{
				{body: "# 1. Only\nhello"},
			},
		},
		{
			name: "thematic break splits two slides",
			in:   "# A\nfoo\n---\n# B\nbar",
			want: []slide{
				{body: "# A\nfoo"},
				{body: "# B\nbar"},
			},
		},
		{
			name: "thematic break wins when numbered h1 also present",
			in:   "# 1. A\nfoo\n---\n# 2. B\nbar",
			want: []slide{
				{body: "# 1. A\nfoo"},
				{body: "# 2. B\nbar"},
			},
		},
		{
			name: "triple-dash inside fenced block does not split numbered-h1 deck",
			in:   "# 1. A\n```\n---\n```\ncontent\n# 2. B\nmore",
			want: []slide{
				{body: "# 1. A\n```\n---\n```\ncontent"},
				{body: "# 2. B\nmore"},
			},
		},
		{
			name: "tilde fence shields triple-dash in thematic-break mode",
			in:   "# A\n~~~\n---\n~~~\nbefore\n---\n# B\nafter",
			want: []slide{
				{body: "# A\n~~~\n---\n~~~\nbefore"},
				{body: "# B\nafter"},
			},
		},
		{
			name: "triple-question marker extracts notes",
			in:   "# 1. A\nbody line\n???\nspeaker note here",
			want: []slide{
				{body: "# 1. A\nbody line", notes: "speaker note here"},
			},
		},
		{
			name: "Notes: marker extracts notes",
			in:   "# 1. A\nbody line\nNotes:\nspeaker note here",
			want: []slide{
				{body: "# 1. A\nbody line", notes: "speaker note here"},
			},
		},
		{
			name: "Notes: marker is trim-sensitive with leading space",
			in:   "# 1. A\nbody line\n  Notes:  \nspeaker note here",
			want: []slide{
				{body: "# 1. A\nbody line", notes: "speaker note here"},
			},
		},
		{
			name: "first marker wins when both appear",
			in:   "# 1. A\nbody\n???\nnote 1\nNotes:\nnote 2",
			want: []slide{
				{body: "# 1. A\nbody", notes: "note 1\nNotes:\nnote 2"},
			},
		},
		{
			name: "triple-question inside fenced block stays in body",
			in:   "# 1. A\n```\n???\n```\nafter",
			want: []slide{
				{body: "# 1. A\n```\n???\n```\nafter"},
			},
		},
		{
			name: "Notes: inside fenced block stays in body",
			in:   "# 1. A\n```\nNotes:\n```\nafter",
			want: []slide{
				{body: "# 1. A\n```\nNotes:\n```\nafter"},
			},
		},
		{
			name: "mixed markers across slides with thematic break",
			in:   "# A\nbody a\n???\nnote a\n---\n# B\nbody b\nNotes:\nnote b",
			want: []slide{
				{body: "# A\nbody a", notes: "note a"},
				{body: "# B\nbody b", notes: "note b"},
			},
		},
		{
			name: "empty body produces no slides",
			in:   "",
			want: nil,
		},
		{
			name: "no separators and no numbered h1 produces no slides",
			in:   "just some text\nno headers",
			want: nil,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := splitSlides(tc.in)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("splitSlides(%q) =\n  %#v\nwant\n  %#v", tc.in, got, tc.want)
			}
		})
	}
}

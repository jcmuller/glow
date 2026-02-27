package ui

import (
	"testing"

	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
)

func TestPagerUpdateSlideNavigationKeys(t *testing.T) {
	newModel := func(start int) pagerModel {
		return pagerModel{
			common:       &commonModel{cfg: Config{PresentationMode: true}, width: 80, height: 24},
			viewport:     viewport.New(0, 0),
			slides:       []string{"A", "B"},
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

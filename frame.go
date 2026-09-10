package main

import (
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// frameChrome is what a frame costs its body: a border column and a pad column
// on each side, and a border row top and bottom.
const (
	frameCols = 4
	frameRows = 2
)

// frame draws a rounded box of the given total width around body, with title
// inset in the top edge.
//
// It builds the border by hand rather than through a lipgloss border style,
// because lipgloss draws a plain edge and offers no way to write a title into
// it. Both the title and the body keep whatever styling they arrived with, so a
// chart's own colors and a legend's dots survive. That means the caller styles
// the title, not frame.
func frame(title, body string, width int) string {
	if width < frameCols+2 {
		return body
	}
	inner := width - frameCols

	var b strings.Builder
	b.WriteString(dimStyle.Render("╭─ ") + title + " ")
	// 3 for the "╭─ " lead, 1 for the space after the title, 1 for the corner.
	if rule := width - lipgloss.Width(title) - 5; rule > 0 {
		b.WriteString(dimStyle.Render(strings.Repeat("─", rule)))
	}
	b.WriteString(dimStyle.Render("╮") + "\n")

	edge := dimStyle.Render("│")
	// MaxWidth counts columns rather than bytes and keeps the escape sequences,
	// so a long line loses its tail instead of pushing the right border out.
	fit := lipgloss.NewStyle().MaxWidth(inner)
	for _, line := range strings.Split(body, "\n") {
		line = fit.Render(line)
		// Measure with lipgloss, not len: a styled line carries escape bytes
		// that take no columns, and a braille rune takes three bytes.
		pad := inner - lipgloss.Width(line)
		if pad < 0 {
			pad = 0
		}
		b.WriteString(edge + " " + line + strings.Repeat(" ", pad) + " " + edge + "\n")
	}

	b.WriteString(dimStyle.Render("╰" + strings.Repeat("─", width-2) + "╯"))
	return b.String()
}

// legend renders "● name" per entry, for a chart that carries several series.
func legend(names []string, palette []lipgloss.Style) string {
	var parts []string
	for i, n := range names {
		parts = append(parts, palette[i%len(palette)].Render("●")+dimStyle.Render(" "+n))
	}
	return strings.Join(parts, "   ")
}

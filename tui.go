package main

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/help"
	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/spinner"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

type viewID int

const (
	viewNow viewID = iota
	viewHistory
	viewTokens
	viewConfig
)

var tabNames = []string{"Now", "History", "Tokens", "Config"}

// cronCheckInterval is how often the model asks whether the schedule selects
// the current minute. tea.Tick does not align to the clock, so a whole-minute
// check would drift past a matching minute and miss it.
const cronCheckInterval = 20 * time.Second

// fetchTimeout bounds one API round trip.
const fetchTimeout = 60 * time.Second

type keyMap struct {
	Now     key.Binding
	History key.Binding
	Tokens  key.Binding
	Config  key.Binding

	Refresh key.Binding
	Auto    key.Binding
	Window  key.Binding
	Span    key.Binding
	Help    key.Binding
	Quit    key.Binding
}

func newKeyMap() keyMap {
	return keyMap{
		Now:     key.NewBinding(key.WithKeys("1"), key.WithHelp("1", "now")),
		History: key.NewBinding(key.WithKeys("2"), key.WithHelp("2", "history")),
		Tokens:  key.NewBinding(key.WithKeys("3"), key.WithHelp("3", "tokens")),
		Config:  key.NewBinding(key.WithKeys("4"), key.WithHelp("4", "config")),
		Refresh: key.NewBinding(key.WithKeys("r"), key.WithHelp("r", "fetch now")),
		Auto:    key.NewBinding(key.WithKeys("a"), key.WithHelp("a", "auto-fetch")),
		Window:  key.NewBinding(key.WithKeys("tab"), key.WithHelp("tab", "window")),
		Span:    key.NewBinding(key.WithKeys("s"), key.WithHelp("s", "span")),
		Help:    key.NewBinding(key.WithKeys("?"), key.WithHelp("?", "help")),
		Quit:    key.NewBinding(key.WithKeys("q", "ctrl+c"), key.WithHelp("q", "quit")),
	}
}

func (k keyMap) ShortHelp() []key.Binding {
	return []key.Binding{k.Now, k.History, k.Tokens, k.Config, k.Refresh, k.Auto, k.Help, k.Quit}
}

func (k keyMap) FullHelp() [][]key.Binding {
	return [][]key.Binding{
		{k.Now, k.History, k.Tokens, k.Config},
		{k.Window, k.Span, k.Refresh, k.Auto},
		{k.Help, k.Quit},
	}
}

// historySpans are the selectable history ranges cycled by the s key.
var historySpans = []struct {
	label string
	dur   time.Duration
}{
	{"6h", 6 * time.Hour},
	{"24h", 24 * time.Hour},
	{"7d", 7 * 24 * time.Hour},
	{"30d", 30 * 24 * time.Hour},
}

type fetchedMsg struct {
	r    Reading
	auto bool // fired by the schedule rather than the r key
	at   time.Time
}
type fetchErrMsg struct {
	err  error
	auto bool
}
type historyMsg struct{ readings []Reading }
type tokensMsg struct {
	samples []TokenSample
	total   tokenUse
	calls   int
}

// cronTickMsg carries the sequence of the chain that armed it. A tick armed
// before a pause/resume carries a stale sequence and is dropped, so repeated
// toggling cannot leave several chains ticking at once.
type cronTickMsg struct {
	t   time.Time
	seq int
}

type model struct {
	db  *sql.DB
	cfg Config
	// cfgPath is shown on the config tab so the user knows what to edit.
	cfgPath string

	keys keyMap
	help help.Model
	spin spinner.Model

	active   viewID
	fetching bool
	// autoFetch is a session toggle over the configured schedule. It starts on
	// whenever the schedule parses.
	autoFetch bool
	lastCron  time.Time // minute of the last scheduled fetch, so one minute fires once
	cronSeq   int       // current tick chain; older ticks are stale

	latest   Reading
	hasData  bool
	history  []Reading
	spanIdx  int
	selected int // which window the history tab graphs

	// tokens is what clusage spent on its own probe calls over the current
	// span; tokenTotal and tokenCalls are all time, not span limited.
	tokens     []TokenSample
	tokenTotal tokenUse
	tokenCalls int

	err      error
	lastAuto string // sticky "auto 14:15 ok" marker for the tab bar

	width, height int
}

func newModel(db *sql.DB, cfg Config, cfgPath string, latest Reading, hasData bool) model {
	sp := spinner.New()
	sp.Spinner = spinner.Dot
	sp.Style = lipgloss.NewStyle().Foreground(accent)

	km := newKeyMap()
	scheduled := cronValid(cfg.FetchCron)
	if !scheduled {
		// Nothing to toggle without a schedule; keep a to itself out of the help
		// rather than advertise a no-op.
		km.Auto.SetEnabled(false)
	}

	return model{
		db:        db,
		cfg:       cfg,
		cfgPath:   cfgPath,
		keys:      km,
		help:      help.New(),
		spin:      sp,
		autoFetch: scheduled,
		latest:    latest,
		hasData:   hasData,
		spanIdx:   1, // 24h
	}
}

func (m model) Init() tea.Cmd {
	return tea.Batch(m.historyCmd(), m.tokensCmd(), m.cronTick())
}

// cronTick arms the next schedule check, or returns nil (a no-op in a Batch)
// when auto-fetch is off. Each tick re-arms the next one, so dropping the
// returned command ends the loop.
func (m model) cronTick() tea.Cmd {
	if !m.autoFetch {
		return nil
	}
	seq := m.cronSeq
	return tea.Tick(cronCheckInterval, func(t time.Time) tea.Msg {
		return cronTickMsg{t: t, seq: seq}
	})
}

// fetchCmd calls the API off the UI goroutine. auto marks a scheduled fetch,
// whose failure is flagged on the tab bar instead of taking over the body.
func fetchCmd(db *sql.DB, model string, auto bool) tea.Cmd {
	return func() tea.Msg {
		token, err := loadToken()
		if err != nil {
			return fetchErrMsg{err: err, auto: auto}
		}
		ctx, cancel := context.WithTimeout(context.Background(), fetchTimeout)
		defer cancel()
		headers, used, err := fetchUsage(ctx, token, model)
		if err != nil {
			return fetchErrMsg{err: err, auto: auto}
		}
		if len(headers) == 0 {
			return fetchErrMsg{err: fmt.Errorf("no anthropic-ratelimit-* headers on the response"), auto: auto}
		}
		r := Reading{FetchedAt: time.Now(), Model: model, Headers: headers}
		if err := saveReading(db, r); err != nil {
			return fetchErrMsg{err: err, auto: auto}
		}
		// A rejected call bills nothing, so it has no usage block. Recording a
		// zero sample would add a flat point to the token chart and count a
		// call that cost nothing.
		if used.total() > 0 {
			if err := saveTokens(db, TokenSample{CalledAt: r.FetchedAt, Model: model, Used: used}); err != nil {
				return fetchErrMsg{err: err, auto: auto}
			}
		}
		return fetchedMsg{r: r, auto: auto, at: r.FetchedAt}
	}
}

func (m model) historyCmd() tea.Cmd {
	db, span := m.db, m.historySpan()
	return func() tea.Msg {
		rs, err := readingsSince(db, time.Now().Add(-span))
		if err != nil {
			return fetchErrMsg{err: err, auto: true} // a history read failure must not blank the body
		}
		return historyMsg{readings: rs}
	}
}

// tokensCmd reads the probe call costs for the current span, plus the all-time
// totals, off the UI goroutine.
func (m model) tokensCmd() tea.Cmd {
	db, span := m.db, m.historySpan()
	return func() tea.Msg {
		ss, err := tokensSince(db, time.Now().Add(-span))
		if err != nil {
			return fetchErrMsg{err: err, auto: true} // must not blank the body
		}
		total, calls, err := tokenTotals(db)
		if err != nil {
			return fetchErrMsg{err: err, auto: true}
		}
		return tokensMsg{samples: ss, total: total, calls: calls}
	}
}

func (m model) historySpan() time.Duration {
	span := historySpans[m.spanIdx].dur
	if max := time.Duration(m.cfg.HistoryHours) * time.Hour; max > 0 && span > max {
		return max
	}
	return span
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.help.Width = msg.Width
		return m, nil

	case spinner.TickMsg:
		if m.fetching {
			var cmd tea.Cmd
			m.spin, cmd = m.spin.Update(msg)
			return m, cmd
		}
		return m, nil

	case fetchedMsg:
		m.fetching = false
		m.err = nil
		m.latest, m.hasData = msg.r, true
		if msg.auto {
			m.lastAuto = "auto " + msg.at.Local().Format("15:04") + " ✓"
		}
		return m, tea.Batch(m.historyCmd(), m.tokensCmd())

	case fetchErrMsg:
		m.fetching = false
		if msg.auto && m.hasData {
			// Keep the last good reading on screen and flag the failure on the
			// tab bar; a scheduled fetch failing must not hide the numbers.
			m.lastAuto = "auto " + time.Now().Local().Format("15:04") + " ✗"
			return m, nil
		}
		m.err = msg.err
		return m, nil

	case historyMsg:
		m.history = msg.readings
		if n := len(currentWindows(m)); m.selected >= n && n > 0 {
			m.selected = 0
		}
		return m, nil

	case tokensMsg:
		m.tokens, m.tokenTotal, m.tokenCalls = msg.samples, msg.total, msg.calls
		return m, nil

	case cronTickMsg:
		if !m.autoFetch || msg.seq != m.cronSeq {
			return m, nil // paused, or a stale tick from an old chain; let it die
		}
		minute := msg.t.Truncate(time.Minute)
		if m.fetching || !m.cfg.fetchDue(msg.t) || minute.Equal(m.lastCron) {
			return m, m.cronTick()
		}
		m.lastCron = minute
		m.fetching = true
		return m, tea.Batch(m.spin.Tick, fetchCmd(m.db, m.cfg.Model, true), m.cronTick())

	case tea.KeyMsg:
		return m.handleKey(msg)
	}
	return m, nil
}

func (m model) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch {
	case key.Matches(msg, m.keys.Quit):
		return m, tea.Quit

	case key.Matches(msg, m.keys.Help):
		m.help.ShowAll = !m.help.ShowAll
		return m, nil

	case key.Matches(msg, m.keys.Refresh):
		if m.fetching {
			return m, nil // a fetch is already in flight; don't race a second one
		}
		m.fetching = true
		m.err = nil
		return m, tea.Batch(m.spin.Tick, fetchCmd(m.db, m.cfg.Model, false))

	case key.Matches(msg, m.keys.Auto):
		m.autoFetch = !m.autoFetch
		if m.autoFetch {
			// Start a fresh chain; bumping the sequence kills any tick still
			// pending from before the pause.
			m.cronSeq++
			return m, m.cronTick()
		}
		// Pausing leaves no tick to clear a stale marker, and the user
		// acknowledged the state by pausing, so drop it.
		m.lastAuto = ""
		return m, nil

	case key.Matches(msg, m.keys.Window):
		if n := len(currentWindows(m)); n > 0 {
			m.selected = (m.selected + 1) % n
		}
		return m, nil

	case key.Matches(msg, m.keys.Span):
		m.spanIdx = (m.spanIdx + 1) % len(historySpans)
		return m, tea.Batch(m.historyCmd(), m.tokensCmd())

	case key.Matches(msg, m.keys.Now):
		m.active = viewNow
		return m, nil
	case key.Matches(msg, m.keys.History):
		m.active = viewHistory
		return m, nil
	case key.Matches(msg, m.keys.Tokens):
		m.active = viewTokens
		return m, nil
	case key.Matches(msg, m.keys.Config):
		m.active = viewConfig
		return m, nil
	}
	return m, nil
}

func (m model) View() string {
	if m.width == 0 {
		return "loading…"
	}
	top := m.renderTabs()
	bottom := footerStyle.Width(m.width).Render(m.help.View(m.keys))
	bodyH := m.height - lipgloss.Height(top) - lipgloss.Height(bottom) - 1
	if bodyH < 3 {
		bodyH = 3
	}

	var body string
	switch {
	case m.err != nil:
		body = errorStyle.Render("fetch failed: ") + m.err.Error() +
			"\n\n" + dimStyle.Render("press r to retry")
	case !m.hasData && m.fetching:
		body = m.spin.View() + " fetching usage…"
	case !m.hasData:
		body = dimStyle.Render("no readings yet — press r to fetch")
	default:
		switch m.active {
		case viewNow:
			body = m.nowView(bodyH)
		case viewHistory:
			body = m.historyView(bodyH)
		case viewTokens:
			body = m.tokensView(bodyH)
		case viewConfig:
			body = m.configView()
		}
	}
	return lipgloss.JoinVertical(lipgloss.Left, top, body, bottom)
}

func (m model) renderTabs() string {
	var tabs []string
	for i, name := range tabNames {
		if viewID(i) == m.active {
			tabs = append(tabs, tabActiveStyle.Render(name))
		} else {
			tabs = append(tabs, tabInactiveStyle.Render(name))
		}
	}
	bar := lipgloss.JoinHorizontal(lipgloss.Top, tabs...)
	// Markers append most-volatile first: the bar is clipped from the right, so
	// a narrow terminal should lose the standing schedule label before it loses
	// an in-flight fetch or a failure the user has not seen.
	if m.fetching {
		bar += "  " + m.spin.View()
	}
	if m.lastAuto != "" {
		style := dimStyle
		if strings.HasSuffix(m.lastAuto, "✗") {
			style = errorStyle
		}
		bar += "  " + style.Render(m.lastAuto)
	}
	if cronValid(m.cfg.FetchCron) {
		label := m.cfg.FetchCron
		if !m.autoFetch {
			label += " paused"
		}
		bar += dimStyle.Render("  cron ") + valueStyle.Render(label)
	}
	return tabBarStyle.MaxWidth(m.width).Render(bar)
}

// currentWindows is the window list the Now and History tabs share, so the tab
// key's selection index means the same thing on both.
func currentWindows(m model) []window {
	if !m.hasData {
		return nil
	}
	return parseWindows(m.latest.Headers)
}

func runTUI() error {
	cfg, path, err := loadConfig()
	if err != nil {
		return err
	}
	db, err := openDB()
	if err != nil {
		return err
	}
	defer db.Close()
	latest, ok, err := latestReading(db)
	if err != nil {
		return err
	}
	p := tea.NewProgram(newModel(db, cfg, path, latest, ok), tea.WithAltScreen())
	_, err = p.Run()
	return err
}

package main

import (
	"bufio"
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"time"

	"golang.org/x/term"
)

// version is overridden at release build time via -ldflags "-X main.version=…".
var version = "dev"

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "clusage:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	// --version is handled before the command dispatch below, which would
	// otherwise route a leading flag to the TUI and open the alt screen.
	if len(args) > 0 && (args[0] == "--version" || args[0] == "-version") {
		fmt.Println("clusage", version)
		return nil
	}
	if len(args) > 0 && isHelpFlag(args[0]) {
		return printHelp("")
	}
	cmd := "tui"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		cmd, args = args[0], args[1:]
	}
	// Checked before the command runs: usage fails on an unset source before
	// it parses flags, and the TUI would open the alt screen.
	if _, ok := commandHelp[cmd]; ok && slices.ContainsFunc(args, isHelpFlag) {
		return printHelp(cmd)
	}
	switch cmd {
	case "help":
		if len(args) > 0 {
			return printHelp(args[0])
		}
		return printHelp("")
	case "setup":
		return setup()
	case "usage":
		return usage(args)
	case "hook":
		return hook(args)
	case "guard":
		return guardCmd(args)
	case "guard-config":
		return guardConfig()
	case "statusline":
		return statusline()
	case "doctor":
		return doctor()
	case "tui":
		return runTUI()
	default:
		return fmt.Errorf("unknown command %q (want: %s). Run clusage help for details", cmd, strings.Join(commandNames(), ", "))
	}
}

func setup() error {
	fmt.Print("Claude Code OAuth token: ")
	raw, err := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Println()
	token := strings.TrimSpace(string(raw))
	if err != nil {
		// Not a terminal, read the line normally.
		line, rerr := readTokenLine(os.Stdin)
		if rerr != nil {
			return err
		}
		token = line
	}
	if token == "" {
		return fmt.Errorf("no token entered")
	}
	if err := saveToken(token); err != nil {
		return err
	}
	_, path, err := loadConfig()
	if err != nil {
		return err
	}
	fmt.Println("token stored in login keychain (service: clusage)")
	fmt.Println("config:", path)
	return nil
}

// readTokenLine reads one token from a non-terminal stdin. A last line with no
// trailing newline still counts, because a piped token often has none.
func readTokenLine(r io.Reader) (string, error) {
	line, err := bufio.NewReader(r).ReadString('\n')
	token := strings.TrimSpace(line)
	if err != nil && (!errors.Is(err, io.EOF) || token == "") {
		return "", err
	}
	return token, nil
}

func usage(args []string) error {
	cfg, cfgPath, err := loadConfig()
	if err != nil {
		return err
	}
	fs := flag.NewFlagSet("usage", flag.ContinueOnError)
	source := fs.String("source", cfg.Source, "source to read, overriding config.json")
	model := fs.String("model", cfg.Model, "model to ping")
	threshold := fs.Int("threshold", cfg.ThresholdMinutes, "minutes before a new call is made")
	force := fs.Bool("force", false, "ignore the cache and call the API")
	verbose := fs.Bool("verbose", false, "print every rate limit header")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg.Source = *source
	cfg.ThresholdMinutes = *threshold

	db, err := openDB()
	if err != nil {
		return err
	}
	defer db.Close()

	u, err := readReport(db, cfg, cfgPath, *model, *force)
	if err != nil {
		return err
	}
	// The report goes out before the writes. The API call is already paid for,
	// so a failed write must not swallow the numbers.
	report(u.r, u.hist, u.now, u.cached, *verbose)
	if u.cached {
		return nil
	}
	u.save(db, *model)
	used := u.used
	if *verbose {
		total, calls, err := tokenTotals(db)
		if err != nil {
			return err
		}
		fmt.Printf("this call: %d in, %d out, %d cache read, %d cache write\n",
			used.Input, used.Output, used.CacheRead, used.CacheCreate)
		fmt.Printf("all time:  %d tokens over %d calls (%d cached)\n",
			total.total(), calls, total.cached())
	}
	return nil
}

// usageRead is one reading as clusage usage reports it.
type usageRead struct {
	r    Reading
	hist []Reading // readings for the rate, the new reading included
	now  time.Time
	// cached is true when the reading came from the database, so there is
	// nothing new to save.
	cached bool
	used   tokenUse
}

// readReport reads usage the way clusage usage does: a stored reading younger
// than cfg.ThresholdMinutes, else a new one from the source chain. The guard
// rail hook reads through here too, so its failures land in the same table.
func readReport(db *sql.DB, cfg Config, cfgPath, model string, force bool) (usageRead, error) {
	// Checked before the cache, so an unset source is reported even while a
	// stored reading is fresh enough to print.
	if !slices.Contains(sources, cfg.Source) {
		return usageRead{}, errNoSource(cfgPath, cfg.Source)
	}
	now := time.Now()
	last, ok, err := latestReadingFrom(db, cfg.readingModels(model)...)
	if err != nil {
		return usageRead{}, err
	}
	// The rate column needs history. Seven days covers the longest window's
	// smoothing horizon, and these rows are small.
	hist := loadHistory(db, now.Add(-7*24*time.Hour))
	if ok && !force && now.Sub(last.FetchedAt) < time.Duration(cfg.ThresholdMinutes)*time.Minute {
		return usageRead{r: last, hist: hist, now: now, cached: true}, nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	r, used, fresh, err := readUsage(ctx, db, cfg, cfgPath, model)
	if err != nil {
		return usageRead{}, err
	}
	if !fresh {
		return usageRead{r: r, hist: hist, now: now, cached: true}, nil
	}
	// The new reading is saved later, so add it here for the rate.
	return usageRead{r: r, hist: append(hist, r), now: time.Now(), used: used}, nil
}

// save stores a new reading and the tokens its call spent. A failed write is
// reported and not returned, because the numbers are already in hand.
func (u usageRead) save(db *sql.DB, model string) {
	if err := saveReading(db, u.r); err != nil {
		fmt.Fprintln(os.Stderr, "clusage: save reading:", err)
	}
	// Only the probe bills tokens. A usage endpoint reading or a rejected probe
	// has no usage block, and a zero sample would count a call that cost nothing.
	if u.used.total() > 0 {
		if err := saveTokens(db, TokenSample{CalledAt: u.r.FetchedAt, Model: model, Used: u.used}); err != nil {
			fmt.Fprintln(os.Stderr, "clusage: save tokens:", err)
		}
	}
}

// loadHistory reads readings for the burn rate column. The rate is an
// enhancement, so a read failure here must not take down the report the
// guard rail hook depends on. It falls back to no history instead, which
// makes burnRate report unknown and rateLabel render the column blank.
func loadHistory(db *sql.DB, since time.Time) []Reading {
	hist, err := readingsSince(db, since)
	if err != nil {
		fmt.Fprintln(os.Stderr, "clusage: read history:", err)
		return nil
	}
	return hist
}

func report(r Reading, hist []Reading, now time.Time, cached bool, verbose bool) {
	for _, w := range parseWindows(r.Headers) {
		rate, ok := burnRate(hist, w.Name, now)
		// The rate column sits between the status and the reset text. The
		// CLUSAGE_GUARD_FIXTURE parser reads $1, $2 and $4 and then searches
		// for "resets", so a field added here moves nothing it depends on.
		line := fmt.Sprintf("%-9s%-11s%-18s%-10s%s",
			// The trailing space keeps a long name such as 7d-sonnet off its percent.
			w.Name+" ", percentUsed(w.Utilization), w.Status, rateLabel(rate, ok),
			formatReset(w.Reset, now))
		fmt.Println(strings.TrimRight(line, " "))
	}
	if verbose {
		fmt.Println()
		for k, v := range r.Headers {
			fmt.Printf("%s: %s\n", k, v)
		}
	}
	age := now.Sub(r.FetchedAt).Round(time.Second)
	src := "live"
	if cached {
		src = "cached"
	}
	fmt.Printf("\n%s, %s ago (%s)\n", src, age, readingSource(r))
}

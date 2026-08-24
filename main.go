package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"os"
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
	cmd := "tui"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		cmd, args = args[0], args[1:]
	}
	switch cmd {
	case "setup":
		return setup()
	case "usage":
		return usage(args)
	case "tui":
		return runTUI()
	default:
		return fmt.Errorf("unknown command %q (want: tui, setup, usage)", cmd)
	}
}

func setup() error {
	fmt.Print("Claude Code OAuth token: ")
	raw, err := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Println()
	token := strings.TrimSpace(string(raw))
	if err != nil {
		// Not a terminal, read the line normally.
		line, rerr := bufio.NewReader(os.Stdin).ReadString('\n')
		if rerr != nil {
			return err
		}
		token = strings.TrimSpace(line)
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

func usage(args []string) error {
	cfg, _, err := loadConfig()
	if err != nil {
		return err
	}
	fs := flag.NewFlagSet("usage", flag.ContinueOnError)
	model := fs.String("model", cfg.Model, "model to ping")
	threshold := fs.Int("threshold", cfg.ThresholdMinutes, "minutes before a new call is made")
	force := fs.Bool("force", false, "ignore the cache and call the API")
	verbose := fs.Bool("verbose", false, "print every rate limit header")
	if err := fs.Parse(args); err != nil {
		return err
	}

	db, err := openDB()
	if err != nil {
		return err
	}
	defer db.Close()

	now := time.Now()
	last, ok, err := latestReading(db)
	if err != nil {
		return err
	}
	if ok && !*force && now.Sub(last.FetchedAt) < time.Duration(*threshold)*time.Minute {
		report(last, now, true, *verbose)
		return nil
	}

	token, err := loadToken()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	headers, used, err := fetchUsage(ctx, token, *model)
	if err != nil {
		return err
	}
	if len(headers) == 0 {
		return fmt.Errorf("no anthropic-ratelimit-* headers on the response")
	}
	r := Reading{FetchedAt: time.Now(), Model: *model, Headers: headers}
	if err := saveReading(db, r); err != nil {
		return err
	}
	if err := saveTokens(db, TokenSample{CalledAt: r.FetchedAt, Model: *model, Used: used}); err != nil {
		return err
	}
	report(r, time.Now(), false, *verbose)
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

func report(r Reading, now time.Time, cached bool, verbose bool) {
	for _, w := range parseWindows(r.Headers) {
		line := fmt.Sprintf("%-9s%-11s%-18s%s",
			w.Name, percentUsed(w.Utilization), w.Status, formatReset(w.Reset, now))
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
	fmt.Printf("\n%s, %s ago (%s)\n", src, age, r.Model)
}

// sss-node is the Super Speedy Search node daemon and companion CLI.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"syscall"
	"time"

	"super-speedy-search/internal/api"
	"super-speedy-search/internal/config"
	"super-speedy-search/internal/content"
	"super-speedy-search/internal/db"
	"super-speedy-search/internal/discovery"
	"super-speedy-search/internal/scanner"
	"super-speedy-search/internal/search"
	"super-speedy-search/internal/watcher"
)

const version = "0.1.0"

func main() {
	// This CLI intentionally has no external command framework. The binary has
	// only a few commands, so a small hand-rolled dispatcher keeps startup easy
	// to follow: the first non-flag argument is the command, and "run" is the
	// default when no command is provided.
	args := os.Args[1:]
	cmd := "run"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		cmd, args = args[0], args[1:]
	}
	var err error
	switch cmd {
	case "run":
		err = cmdRun(args)
	case "scan":
		err = cmdScan(args)
	case "discover":
		err = cmdDiscover(args)
	case "search":
		err = cmdSearch(args)
	case "version":
		fmt.Println("sss-node " + version)
	case "help", "-h", "--help":
		usage()
	default:
		usage()
		err = fmt.Errorf("unknown command %q", cmd)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Println(`sss-node — Super Speedy Search node

Usage:
  sss-node run       [-config path]           run the node daemon (default)
  sss-node scan      [-config path] [-root id] run one scan and exit
  sss-node discover  [-timeout 3s]            list nodes advertised on the LAN
  sss-node search    [flags] <query...>       fan-out metadata search
  sss-node version

Search flags:
  -node URL     node base URL (repeatable); default: discover via mDNS
  -token T      bearer token (or set SSS_TOKEN)
  -ext .pdf     filter by extension (repeatable)
  -limit N      max results per node (default 50)
  -timeout D    discovery timeout (default 3s)`)
}

func defaultConfigPath() string {
	// Configuration is local to each node. The search GUI talks to nodes over
	// HTTP, but it does not own their scan roots or indexes, so every machine
	// can be installed and tuned independently.
	if p := os.Getenv("SSS_CONFIG"); p != "" {
		return p
	}
	home, _ := os.UserHomeDir()
	candidates := []string{"/config/config.yaml"} // Docker convention
	if runtime.GOOS == "darwin" {
		candidates = append(candidates, filepath.Join(home, "Library/Application Support/SuperSpeedySearch/config.yaml"))
	}
	candidates = append(candidates,
		filepath.Join(home, ".config/super-speedy-search/config.yaml"),
		"/etc/super-speedy-search/config.yaml",
		"config.yaml",
	)
	for _, c := range candidates {
		if _, err := os.Stat(c); err == nil {
			return c
		}
	}
	return "config.yaml"
}

func loadConfig(fs *flag.FlagSet, args []string) (*config.Config, error) {
	cfgPath := fs.String("config", defaultConfigPath(), "path to config.yaml")
	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return nil, fmt.Errorf("load config %s: %w", *cfgPath, err)
	}
	return cfg, nil
}

func cmdRun(args []string) error {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	cfg, err := loadConfig(fs, args)
	if err != nil {
		return err
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	token, generated, err := cfg.EnsureAuthToken()
	if err != nil {
		return fmt.Errorf("auth token: %w", err)
	}
	switch {
	case generated:
		log.Info("generated auth token", "token", token, "persisted_to", cfg.TokenPath())
	case token != "":
		log.Info("auth enabled", "token_source", "config")
	default:
		log.Warn("auth disabled by config; anyone on the network can query this node")
	}

	database, err := db.Open(cfg.Database.Path)
	if err != nil {
		return fmt.Errorf("open database %s: %w", cfg.Database.Path, err)
	}
	defer database.Close()

	if err := database.SyncRoots(rootRows(cfg), time.Now().Unix()); err != nil {
		return fmt.Errorf("sync roots: %w", err)
	}

	// A context is Go's standard cancellation signal. When the process gets
	// Ctrl-C or SIGTERM, this context is cancelled and long-running pieces
	// (HTTP server, scanner, watcher, mDNS advertiser) can shut down together.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// The daemon wires together small packages instead of putting all behavior
	// in main: db owns SQLite, scanner owns reconciliation, search/content
	// answer queries, api exposes HTTP, discovery exposes mDNS, and watcher is
	// only a fast-update helper.
	scn := scanner.New(database, cfg, log)
	pdf, err := content.NewPDFExtractor(cfg.Content.PDF)
	if err != nil {
		log.Warn("pdf search unavailable", "err", err)
	}
	srv := &api.Server{
		Cfg:       cfg,
		DB:        database,
		Scanner:   scn,
		Searcher:  &search.Searcher{DB: database, Cfg: cfg, NodeID: cfg.Node.ID},
		Content:   &content.Searcher{DB: database, Cfg: cfg, NodeID: cfg.Node.ID, Log: log, PDF: pdf},
		Version:   version,
		StartedAt: time.Now(),
		Log:       log,
	}
	httpSrv := &http.Server{Addr: cfg.Node.ListenAddr, Handler: srv.Handler()}
	go func() {
		log.Info("api listening", "addr", cfg.Node.ListenAddr)
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("http server failed", "err", err)
			stop()
		}
	}()

	if cfg.Node.AdvertiseOn() {
		// mDNS/Bonjour is a convenience for same-LAN discovery. Manual node URLs
		// still work when mDNS is blocked by Docker networking, VLANs, VPNs, or
		// firewalls.
		if err := discovery.Advertise(ctx, cfg.Node.ID, cfg.Node.Name, cfg.Node.ListenAddr, version, cfg.Node.AuthRequiredOn()); err != nil {
			log.Warn("mdns advertisement failed; manual node URLs still work", "err", err)
		} else {
			log.Info("advertising on mdns", "service", discovery.Service, "instance", cfg.Node.ID)
		}
	}

	if cfg.Scan.Watch.EnabledOn() {
		// File watchers are treated as an acceleration layer only. The periodic
		// scanner below remains the source of truth because network mounts,
		// Docker bind mounts, and sleep/wake cycles can all drop events.
		w := watcher.New(database, cfg, log)
		if err := w.Start(ctx); err != nil {
			log.Warn("filesystem watcher unavailable; relying on periodic scans", "err", err)
		}
	}

	// Initial scan, then periodic reconciliation. The first pass makes the node
	// useful immediately; later passes heal missed watcher events and detect
	// files that were deleted while the daemon was offline.
	go func() {
		if err := scn.Run(ctx, ""); err != nil && !errors.Is(err, scanner.ErrScanRunning) {
			log.Error("initial scan failed", "err", err)
		}
		ticker := time.NewTicker(time.Duration(cfg.Scan.Interval))
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := scn.Run(ctx, ""); err != nil && !errors.Is(err, scanner.ErrScanRunning) {
					log.Error("periodic scan failed", "err", err)
				}
			}
		}
	}()

	<-ctx.Done()
	log.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return httpSrv.Shutdown(shutdownCtx)
}

func cmdScan(args []string) error {
	fs := flag.NewFlagSet("scan", flag.ExitOnError)
	rootID := fs.String("root", "", "scan only this root id")
	cfg, err := loadConfig(fs, args)
	if err != nil {
		return err
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	database, err := db.Open(cfg.Database.Path)
	if err != nil {
		return err
	}
	defer database.Close()
	if err := database.SyncRoots(rootRows(cfg), time.Now().Unix()); err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return scanner.New(database, cfg, log).Run(ctx, *rootID)
}

func cmdDiscover(args []string) error {
	fs := flag.NewFlagSet("discover", flag.ExitOnError)
	timeout := fs.Duration("timeout", 3*time.Second, "how long to browse")
	if err := fs.Parse(args); err != nil {
		return err
	}
	nodes, err := discovery.Browse(context.Background(), *timeout)
	if err != nil {
		return err
	}
	if len(nodes) == 0 {
		fmt.Println("no nodes found")
		return nil
	}
	for _, n := range nodes {
		fmt.Printf("%-24s %s  %s\n", n.Instance, n.URL(), strings.Join(n.TXT, " "))
	}
	return nil
}

type multiFlag []string

func (m *multiFlag) String() string     { return strings.Join(*m, ",") }
func (m *multiFlag) Set(v string) error { *m = append(*m, v); return nil }

func cmdSearch(args []string) error {
	fs := flag.NewFlagSet("search", flag.ExitOnError)
	var nodeURLs, exts multiFlag
	fs.Var(&nodeURLs, "node", "node base URL (repeatable)")
	fs.Var(&exts, "ext", "extension filter (repeatable)")
	token := fs.String("token", os.Getenv("SSS_TOKEN"), "bearer token")
	limit := fs.Int("limit", 50, "max results per node")
	timeout := fs.Duration("timeout", 3*time.Second, "discovery timeout")
	if err := fs.Parse(args); err != nil {
		return err
	}
	query := strings.TrimSpace(strings.Join(fs.Args(), " "))
	if query == "" {
		return errors.New("usage: sss-node search [flags] <query>")
	}

	urls := []string(nodeURLs)
	if len(urls) == 0 {
		nodes, err := discovery.Browse(context.Background(), *timeout)
		if err != nil {
			return err
		}
		for _, n := range nodes {
			urls = append(urls, n.URL())
		}
	}
	if len(urls) == 0 {
		return errors.New("no nodes found via mDNS and none given with -node")
	}

	body, _ := json.Marshal(search.Params{Query: query, Extensions: exts, Limit: *limit})
	type nodeResults struct {
		url     string
		results []search.Result
		err     error
	}
	out := make(chan nodeResults, len(urls))
	for _, u := range urls {
		// Each node owns its own SQLite index, so fan-out search means asking all
		// selected nodes concurrently and merging their answers locally.
		go func(u string) {
			r, err := queryNode(u, *token, body)
			out <- nodeResults{url: u, results: r, err: err}
		}(u)
	}

	var all []search.Result
	for range urls {
		nr := <-out
		if nr.err != nil {
			fmt.Fprintf(os.Stderr, "warn: %s: %v\n", nr.url, nr.err)
			continue
		}
		all = append(all, nr.results...)
	}
	// Global ranking from raw signals: match type first, then recency.
	rank := map[string]int{search.MatchFilenameExact: 3, search.MatchFilename: 2, search.MatchPath: 1}
	sort.SliceStable(all, func(i, j int) bool {
		if rank[all[i].MatchType] != rank[all[j].MatchType] {
			return rank[all[i].MatchType] > rank[all[j].MatchType]
		}
		return all[i].ModifiedAt > all[j].ModifiedAt
	})
	if len(all) == 0 {
		fmt.Println("no results")
		return nil
	}
	for _, r := range all {
		fmt.Printf("%-14s %-10s %s\n", r.MatchType, humanSize(r.SizeBytes), r.DisplayPath)
	}
	return nil
}

func queryNode(baseURL, token string, body []byte) ([]search.Result, error) {
	req, err := http.NewRequest("POST", strings.TrimSuffix(baseURL, "/")+"/v1/search/metadata", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		var e struct {
			Error string `json:"error"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&e)
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, e.Error)
	}
	var parsed struct {
		Results []search.Result `json:"results"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return nil, err
	}
	return parsed.Results, nil
}

func humanSize(b int64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%dB", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f%cB", float64(b)/float64(div), "KMGTPE"[exp])
}

func rootRows(cfg *config.Config) []db.RootRow {
	rows := make([]db.RootRow, 0, len(cfg.Roots))
	for _, r := range cfg.Roots {
		rows = append(rows, db.RootRow{
			ID:            r.ID,
			Path:          r.Path,
			DisplayPrefix: r.DisplayPrefix,
			OpenURIPrefix: r.OpenURIPrefix,
			Enabled:       r.EnabledOn(),
		})
	}
	return rows
}

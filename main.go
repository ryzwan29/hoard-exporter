// hoard-exporter scrapes a self-hosted Hoard server's own JSON endpoints
// (/v1/health and /v1/admin/overview, both reverse-engineered from the
// hoard-server source — there is no documented or stable API contract
// behind them) and re-exposes the numbers as Prometheus metrics on /metrics.
//
// Because /v1/admin/overview is not a public API, a new Hoard release can
// change its shape without notice. This exporter degrades on purpose: if a
// field is missing or a request fails outright, it logs the problem, sets
// hoard_up 0 (or just skips the stats block) and keeps serving whatever it
// last had — it never crashes the scrape loop over one bad response.
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ---- Config ---------------------------------------------------------------

type config struct {
	baseURL    string            // e.g. http://hoard-server:12421 (no trailing slash)
	token      string            // admin device token, "hoard_v1_..."
	listen     string            // exporter's own listen address, e.g. ":9477"
	interval   time.Duration     // how often to poll hoard-server
	timeout    time.Duration     // HTTP client timeout per request
	userTokens map[string]string // username -> that user's own device token (HOARD_USER_TOKENS)
	mint       bool              // mint per-user tokens through the admin API (HOARD_MINT_USER_TOKENS)
	stateFile  string            // where minted tokens are persisted
}

func loadConfig() config {
	c := config{
		baseURL:    strings.TrimRight(getenv("HOARD_URL", ""), "/"),
		token:      getenv("HOARD_TOKEN", ""),
		listen:     getenv("LISTEN_ADDR", ":9477"),
		interval:   getenvDuration("SCRAPE_INTERVAL", 15*time.Second),
		timeout:    getenvDuration("HOARD_TIMEOUT", 10*time.Second),
		userTokens: map[string]string{},
		mint:       getenv("HOARD_MINT_USER_TOKENS", "") == "true",
		stateFile:  getenv("HOARD_STATE_FILE", "/data/hoard-exporter-tokens.json"),
	}
	if c.baseURL == "" {
		log.Fatal("HOARD_URL is required, e.g. http://hoard-server:12421")
	}
	if c.token == "" {
		log.Fatal("HOARD_TOKEN is required (an admin device token, hoard_v1_...)")
	}
	// HOARD_USER_TOKENS="alice=hoard_v1_aaa,bob=hoard_v1_bbb"
	for _, kv := range strings.Split(getenv("HOARD_USER_TOKENS", ""), ",") {
		name, tok, ok := strings.Cut(strings.TrimSpace(kv), "=")
		if ok && name != "" && tok != "" {
			c.userTokens[name] = tok
		}
	}
	return c
}

func getenv(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return def
}

func getenvDuration(key string, def time.Duration) time.Duration {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		log.Printf("warn: %s=%q is not a valid duration, using default %s", key, v, def)
		return def
	}
	return d
}

// ---- Hoard's own JSON shapes ------------------------------------------------
// Mirrors crates/hoard-server/src/routes/health.rs and routes/admin.rs
// (AdminOverview/Totals/UserRow) as of the version this was written against.
// Fields are read defensively: anything missing just zero-values out instead
// of failing the whole decode, since these structs are not a stable contract.

type health struct {
	Status     string `json:"status"`
	Version    string `json:"version"`
	UptimeSecs uint64 `json:"uptime_secs"`
}

type adminOverview struct {
	Totals totals    `json:"totals"`
	Users  []userRow `json:"users"`
}

type totals struct {
	Users            int64   `json:"users"`
	Admins           int64   `json:"admins"`
	Saves            int64   `json:"saves"`
	Versions         int64   `json:"versions"`
	TrashedVersions  int64   `json:"trashed_versions"`
	LogicalBytes     int64   `json:"logical_bytes"`
	StoredBytes      int64   `json:"stored_bytes"`
	TrashBytes       int64   `json:"trash_bytes"`
	OrphanObjects    int64   `json:"orphan_objects"`
	OrphanBytes      int64   `json:"orphan_bytes"`
	Objects          int64   `json:"objects"`
	DBBytes          *int64  `json:"db_bytes"`
	ClientLogs       int64   `json:"client_logs"`
	OldestSnapshotAt *string `json:"oldest_snapshot_at"`
}

type userRow struct {
	ID          string  `json:"id"`
	Username    string  `json:"username"`
	IsAdmin     bool    `json:"is_admin"`
	UsedBytes   int64   `json:"used_bytes"`
	QuotaBytes  int64   `json:"quota_bytes"`
	StoredBytes int64   `json:"stored_bytes"`
	Saves       int64   `json:"saves"`
	Versions    int64   `json:"versions"`
	Devices     int64   `json:"devices"`
	LastSeenAt  *string `json:"last_seen_at"`
	CreatedAt   string  `json:"created_at"`
}

// Per-user shapes (routes/overview.rs GameRow, routes/playtime.rs
// PlaytimeAggregate). Unlike /v1/admin/overview these are scoped to the bearer
// token's own user, so reading a user's games needs that user's token.

type meOverview struct {
	Games []meGame `json:"games"`
}

type meGame struct {
	Slug         string  `json:"slug"`
	DisplayName  string  `json:"display_name"`
	Saves        int64   `json:"saves"`
	Versions     int64   `json:"versions"`
	Bytes        int64   `json:"bytes"`
	LastBackupAt *string `json:"last_backup_at"`
}

type playtimeAgg struct {
	ByGame    map[string]uint64 `json:"by_game"`
	TotalSecs uint64            `json:"total_secs"`
}

// gameStat is one (user, game) row after merging /v1/me/overview with
// /v1/playtime. A game can have playtime without any save and vice versa.
type gameStat struct {
	slug, name   string
	saves        int64
	versions     int64
	bytes        int64
	lastBackupAt *string
	playSecs     uint64
}

// userData is one user's per-game scrape result.
type userData struct {
	username  string
	skipped   bool // no token available for this user: emit nothing
	err       string
	totalSecs uint64
	games     []gameStat
}

// ---- Scraper ----------------------------------------------------------------

// snapshot is the latest successfully-parsed state, served to every /metrics
// request until the next poll replaces it. Guarded by mu.
type snapshot struct {
	up            bool
	version       string
	uptimeSecs    uint64
	overview      *adminOverview // nil if the admin call failed or hasn't succeeded yet
	users         []userData     // per-user game/playtime data, same order as overview.Users
	lastScrapeErr string
	lastScrapeAt  time.Time
}

type scraper struct {
	cfg    config
	client *http.Client

	mu   sync.RWMutex
	snap snapshot

	minMu  sync.Mutex
	minted map[string]string // user id -> minted device token
}

func newScraper(cfg config) *scraper {
	s := &scraper{
		cfg:    cfg,
		client: &http.Client{Timeout: cfg.timeout},
		minted: map[string]string{},
	}
	if cfg.mint {
		if b, err := os.ReadFile(cfg.stateFile); err == nil {
			if err := json.Unmarshal(b, &s.minted); err != nil {
				log.Printf("warn: %s is not valid JSON, starting with no minted tokens: %v", cfg.stateFile, err)
			}
		}
	}
	return s
}

type httpError struct {
	status int
	msg    string
}

func (e *httpError) Error() string { return e.msg }

func (s *scraper) get(path string, out any) error {
	return s.call(http.MethodGet, path, s.cfg.token, nil, out)
}

func (s *scraper) call(method, path, token string, in, out any) error {
	var rd io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, s.cfg.baseURL+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := s.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		snippet := string(body)
		if len(snippet) > 200 {
			snippet = snippet[:200]
		}
		return &httpError{resp.StatusCode, fmt.Sprintf("%s: unexpected status %d: %s", path, resp.StatusCode, snippet)}
	}
	return json.Unmarshal(body, out)
}

var errNoToken = errors.New("no token for user")

// tokenFor returns the token to read this user's own data with: an explicit
// HOARD_USER_TOKENS entry first, else (opt-in) a device token minted through
// POST /v1/admin/tokens and persisted so a restart doesn't mint another one.
func (s *scraper) tokenFor(u userRow) (tok string, minted bool, err error) {
	if t, ok := s.cfg.userTokens[u.Username]; ok {
		return t, false, nil
	}
	if !s.cfg.mint {
		return "", false, errNoToken
	}
	s.minMu.Lock()
	defer s.minMu.Unlock()
	if t, ok := s.minted[u.ID]; ok {
		return t, true, nil
	}
	var out struct {
		Token string `json:"token"`
	}
	in := map[string]string{"user_id": u.ID, "device_name": "hoard-exporter"}
	if err := s.call(http.MethodPost, "/v1/admin/tokens", s.cfg.token, in, &out); err != nil {
		return "", false, fmt.Errorf("mint token: %w", err)
	}
	if out.Token == "" {
		return "", false, errors.New("mint token: empty token in response")
	}
	s.minted[u.ID] = out.Token
	s.saveMintedLocked()
	log.Printf("minted a device token for user %q (device name: hoard-exporter)", u.Username)
	return out.Token, true, nil
}

func (s *scraper) dropMinted(id string) {
	s.minMu.Lock()
	defer s.minMu.Unlock()
	delete(s.minted, id)
	s.saveMintedLocked()
}

func (s *scraper) saveMintedLocked() {
	b, err := json.Marshal(s.minted)
	if err != nil {
		return
	}
	tmp := s.cfg.stateFile + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		log.Printf("warn: cannot persist minted tokens to %s (will re-mint on restart): %v", s.cfg.stateFile, err)
		return
	}
	if err := os.Rename(tmp, s.cfg.stateFile); err != nil {
		log.Printf("warn: cannot persist minted tokens to %s: %v", s.cfg.stateFile, err)
	}
}

// collectUser reads one user's games and playtime with that user's own token.
func (s *scraper) collectUser(u userRow) userData {
	d := userData{username: u.Username}
	tok, minted, err := s.tokenFor(u)
	if errors.Is(err, errNoToken) {
		d.skipped = true
		return d
	}
	if err == nil {
		var mo meOverview
		var pt playtimeAgg
		if err = s.call(http.MethodGet, "/v1/me/overview", tok, nil, &mo); err == nil {
			err = s.call(http.MethodGet, "/v1/playtime", tok, nil, &pt)
		}
		if err == nil {
			d.totalSecs = pt.TotalSecs
			d.games = mergeGames(mo.Games, pt.ByGame)
			return d
		}
		var he *httpError
		if minted && errors.As(err, &he) && (he.status == 401 || he.status == 403) {
			s.dropMinted(u.ID) // expired or revoked: mint a fresh one next poll
		}
	}
	d.err = err.Error()
	return d
}

func mergeGames(games []meGame, play map[string]uint64) []gameStat {
	by := map[string]*gameStat{}
	for _, g := range games {
		name := g.DisplayName
		if name == "" {
			name = g.Slug
		}
		by[g.Slug] = &gameStat{slug: g.Slug, name: name, saves: g.Saves, versions: g.Versions, bytes: g.Bytes, lastBackupAt: g.LastBackupAt}
	}
	for slug, secs := range play {
		if g, ok := by[slug]; ok {
			g.playSecs = secs
		} else {
			by[slug] = &gameStat{slug: slug, name: slug, playSecs: secs}
		}
	}
	out := make([]gameStat, 0, len(by))
	for _, g := range by {
		out = append(out, *g)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].slug < out[j].slug })
	return out
}

// poll runs one scrape cycle and updates the snapshot. It never panics the
// caller: every failure is captured as lastScrapeErr instead.
func (s *scraper) poll() {
	var h health
	var next snapshot
	next.lastScrapeAt = time.Now()

	if err := s.get("/v1/health", &h); err != nil {
		next.up = false
		next.lastScrapeErr = fmt.Sprintf("health: %v", err)
		s.commit(next)
		log.Printf("scrape failed: %s", next.lastScrapeErr)
		return
	}
	next.up = h.Status == "ok"
	next.version = h.Version
	next.uptimeSecs = h.UptimeSecs

	var ov adminOverview
	if err := s.get("/v1/admin/overview", &ov); err != nil {
		// Health is fine but the admin call isn't (bad/non-admin token, old
		// server without this route, transient DB error on their side).
		// Keep serving health-level metrics; just drop the stats block.
		next.overview = nil
		next.lastScrapeErr = fmt.Sprintf("admin/overview: %v", err)
		s.commit(next)
		log.Printf("admin/overview scrape failed (serving health-only): %v", err)
		return
	}
	next.overview = &ov

	// Per-user games and playtime. One failing user never blocks the others.
	if len(s.cfg.userTokens) > 0 || s.cfg.mint {
		for _, u := range ov.Users {
			d := s.collectUser(u)
			if d.err != "" {
				log.Printf("user %q games/playtime scrape failed: %s", u.Username, d.err)
			}
			next.users = append(next.users, d)
		}
	}
	s.commit(next)
}

func (s *scraper) commit(next snapshot) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.snap = next
}

func (s *scraper) current() snapshot {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.snap
}

func (s *scraper) loop() {
	s.poll() // first poll synchronously so the very first /metrics isn't empty
	ticker := time.NewTicker(s.cfg.interval)
	defer ticker.Stop()
	for range ticker.C {
		s.poll()
	}
}

// ---- Prometheus text exposition ---------------------------------------------
// Hand-written instead of pulling in client_golang: the metric set here is a
// handful of gauges with no histograms, and this keeps the binary dependency
// free (one less thing to break between a `go mod tidy` and a Docker build on
// someone else's machine).

type metricWriter struct {
	b           strings.Builder
	writtenHelp map[string]bool
}

func newMetricWriter() *metricWriter {
	return &metricWriter{writtenHelp: map[string]bool{}}
}

// gauge writes one sample, emitting the HELP/TYPE header only the first time
// a given metric name is seen (so a metric with several label sets doesn't
// repeat its header).
func (w *metricWriter) gauge(name, help string, value float64, labels map[string]string) {
	if !w.writtenHelp[name] {
		fmt.Fprintf(&w.b, "# HELP %s %s\n", name, help)
		fmt.Fprintf(&w.b, "# TYPE %s gauge\n", name)
		w.writtenHelp[name] = true
	}
	fmt.Fprintf(&w.b, "%s%s %s\n", name, formatLabels(labels), formatFloat(value))
}

// formatFloat renders a sample value. Every metric here is integer-valued
// (counts and byte sizes), so whole numbers are printed without scientific
// notation — "5000000000" reads a lot better in a raw scrape or a Grafana
// tooltip than "5e+09", and both are equally valid Prometheus exposition
// format. Anything that isn't a clean int64 (there shouldn't be any) falls
// back to %g.
func formatFloat(f float64) string {
	if f == math.Trunc(f) && f >= math.MinInt64 && f <= math.MaxInt64 {
		return strconv.FormatInt(int64(f), 10)
	}
	return strconv.FormatFloat(f, 'g', -1, 64)
}

func formatLabels(labels map[string]string) string {
	if len(labels) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteByte('{')
	first := true
	for k, v := range labels {
		if !first {
			b.WriteByte(',')
		}
		first = false
		fmt.Fprintf(&b, `%s="%s"`, k, escapeLabelValue(v))
	}
	b.WriteByte('}')
	return b.String()
}

func escapeLabelValue(v string) string {
	v = strings.ReplaceAll(v, `\`, `\\`)
	v = strings.ReplaceAll(v, "\n", `\n`)
	v = strings.ReplaceAll(v, `"`, `\"`)
	return v
}

// unixSeconds parses the RFC3339 timestamps hoard-server writes
// ("2026-01-02T15:04:05Z") into a unix epoch float, or -1 if absent/unparsable
// so it's visible in the metric rather than silently defaulting to 0 (the
// epoch), which would look like a real, very old timestamp.
func unixSeconds(ts *string) float64 {
	if ts == nil || *ts == "" {
		return -1
	}
	t, err := time.Parse(time.RFC3339, *ts)
	if err != nil {
		return -1
	}
	return float64(t.Unix())
}

func boolToFloat(b bool) float64 {
	if b {
		return 1
	}
	return 0
}

func renderMetrics(snap snapshot) string {
	w := newMetricWriter()

	w.gauge("hoard_up", "1 if the last scrape of /v1/health succeeded and reported status ok, else 0.", boolToFloat(snap.up), nil)
	w.gauge("hoard_scrape_success", "1 if the last /v1/admin/overview scrape succeeded, else 0 (health may still be up).", boolToFloat(snap.overview != nil), nil)
	w.gauge("hoard_last_scrape_timestamp_seconds", "Unix time of the most recent scrape attempt, successful or not.", float64(snap.lastScrapeAt.Unix()), nil)

	if snap.version != "" {
		w.gauge("hoard_build_info", "Always 1; the server version is carried as a label.", 1, map[string]string{"version": snap.version})
	}
	w.gauge("hoard_uptime_seconds", "Seconds since the hoard-server process started.", float64(snap.uptimeSecs), nil)

	if snap.overview == nil {
		return w.b.String()
	}
	t := snap.overview.Totals

	w.gauge("hoard_users_total", "Total user accounts on this server.", float64(t.Users), nil)
	w.gauge("hoard_admins_total", "Accounts with the admin flag set.", float64(t.Admins), nil)
	w.gauge("hoard_saves_total", "Total save slots across all users.", float64(t.Saves), nil)
	w.gauge("hoard_versions_total", "Non-trashed snapshot versions across all saves.", float64(t.Versions), nil)
	w.gauge("hoard_versions_trashed_total", "Soft-deleted versions still sitting in trash.", float64(t.TrashedVersions), nil)
	w.gauge("hoard_logical_bytes", "Sum of save sizes before deduplication.", float64(t.LogicalBytes), nil)
	w.gauge("hoard_stored_bytes", "Actual bytes on disk/bucket after deduplication (blobs+chunks).", float64(t.StoredBytes), nil)
	w.gauge("hoard_trash_bytes", "Bytes held by trashed (soft-deleted) versions.", float64(t.TrashBytes), nil)
	w.gauge("hoard_orphan_objects_total", "Blobs/chunks with refcount<=0, awaiting the cleanup sweep. A number that only grows means the sweep is failing.", float64(t.OrphanObjects), nil)
	w.gauge("hoard_orphan_bytes", "Disk occupied by orphan_objects.", float64(t.OrphanBytes), nil)
	w.gauge("hoard_objects_total", "Total blob+chunk rows, live and orphaned.", float64(t.Objects), nil)
	w.gauge("hoard_client_logs_total", "Diagnostic log batches uploaded by clients, awaiting retention sweep.", float64(t.ClientLogs), nil)
	w.gauge("hoard_oldest_snapshot_timestamp_seconds", "Creation time of the oldest snapshot on the server, -1 if there are none.", unixSeconds(t.OldestSnapshotAt), nil)
	if t.DBBytes != nil {
		w.gauge("hoard_db_bytes", "Size of the SQLite database file plus its WAL sidecar. Absent (metric omitted) on a non-SQLite backend.", float64(*t.DBBytes), nil)
	}

	for _, u := range snap.overview.Users {
		labels := map[string]string{"username": u.Username, "is_admin": strconv.FormatBool(u.IsAdmin)}
		w.gauge("hoard_user_used_bytes", "Bytes counted against this user's quota.", float64(u.UsedBytes), labels)
		w.gauge("hoard_user_quota_bytes", "This user's storage quota in bytes (0 may mean unlimited, depending on server config).", float64(u.QuotaBytes), labels)
		w.gauge("hoard_user_stored_bytes", "Actual deduplicated bytes owned by this user.", float64(u.StoredBytes), labels)
		w.gauge("hoard_user_saves_total", "Save slots belonging to this user.", float64(u.Saves), labels)
		w.gauge("hoard_user_versions_total", "Non-trashed versions belonging to this user.", float64(u.Versions), labels)
		w.gauge("hoard_user_devices_total", "Machines registered to this user (online + offline).", float64(u.Devices), labels)
		w.gauge("hoard_user_last_seen_timestamp_seconds", "Last heartbeat from any of this user's devices, -1 if never seen.", unixSeconds(u.LastSeenAt), labels)
	}

	// Per-game / per-playtime metrics. Rankings, per-game totals and growth are
	// PromQL over these (topk, sum by (game), delta), not computed here.
	for _, d := range snap.users {
		if d.skipped {
			continue
		}
		ul := map[string]string{"username": d.username}
		w.gauge("hoard_user_game_scrape_success", "1 if this user's games/playtime were read on the last scrape, else 0.", boolToFloat(d.err == ""), ul)
		if d.err != "" {
			continue
		}
		w.gauge("hoard_user_playtime_seconds", "All-time play time of this user, including time not attributed to a game.", float64(d.totalSecs), ul)
		w.gauge("hoard_user_games_total", "Games this user has a save or play time for.", float64(len(d.games)), ul)
		for _, g := range d.games {
			gl := map[string]string{"username": d.username, "game": g.slug, "game_name": g.name}
			w.gauge("hoard_user_game_playtime_seconds", "All-time play time of this user in this game.", float64(g.playSecs), gl)
			w.gauge("hoard_user_game_saves", "Save slots this user has for this game.", float64(g.saves), gl)
			w.gauge("hoard_user_game_versions", "Non-trashed versions this user has for this game.", float64(g.versions), gl)
			w.gauge("hoard_user_game_bytes", "Logical (pre-dedup) bytes of this user's versions of this game.", float64(g.bytes), gl)
			w.gauge("hoard_user_game_last_backup_timestamp_seconds", "Time of this user's last backup of this game, -1 if none.", unixSeconds(g.lastBackupAt), gl)
		}
	}

	return w.b.String()
}

// ---- HTTP server --------------------------------------------------------

func main() {
	cfg := loadConfig()
	s := newScraper(cfg)
	go s.loop()

	mux := http.NewServeMux()
	mux.HandleFunc("/metrics", func(w http.ResponseWriter, r *http.Request) {
		snap := s.current()
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		fmt.Fprint(w, renderMetrics(snap))
	})
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		// Exporter's own liveness, not hoard-server's. Always 200 if the
		// process answers at all.
		w.WriteHeader(http.StatusOK)
		fmt.Fprintln(w, "ok")
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, "hoard-exporter: see /metrics")
	})

	log.Printf("hoard-exporter listening on %s, polling %s every %s", cfg.listen, cfg.baseURL, cfg.interval)
	if err := http.ListenAndServe(cfg.listen, mux); err != nil {
		log.Fatal(err)
	}
}

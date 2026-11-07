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
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ---- Config ---------------------------------------------------------------

type config struct {
	baseURL  string        // e.g. http://hoard-server:12421 (no trailing slash)
	token    string        // admin device token, "hoard_v1_..."
	listen   string        // exporter's own listen address, e.g. ":9477"
	interval time.Duration // how often to poll hoard-server
	timeout  time.Duration // HTTP client timeout per request
}

func loadConfig() config {
	c := config{
		baseURL:  strings.TrimRight(getenv("HOARD_URL", ""), "/"),
		token:    getenv("HOARD_TOKEN", ""),
		listen:   getenv("LISTEN_ADDR", ":9477"),
		interval: getenvDuration("SCRAPE_INTERVAL", 15*time.Second),
		timeout:  getenvDuration("HOARD_TIMEOUT", 10*time.Second),
	}
	if c.baseURL == "" {
		log.Fatal("HOARD_URL is required, e.g. http://hoard-server:12421")
	}
	if c.token == "" {
		log.Fatal("HOARD_TOKEN is required (an admin device token, hoard_v1_...)")
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

// ---- Scraper ----------------------------------------------------------------

// snapshot is the latest successfully-parsed state, served to every /metrics
// request until the next poll replaces it. Guarded by mu.
type snapshot struct {
	up            bool
	version       string
	uptimeSecs    uint64
	overview      *adminOverview // nil if the admin call failed or hasn't succeeded yet
	lastScrapeErr string
	lastScrapeAt  time.Time
}

type scraper struct {
	cfg    config
	client *http.Client

	mu   sync.RWMutex
	snap snapshot
}

func newScraper(cfg config) *scraper {
	return &scraper{
		cfg:    cfg,
		client: &http.Client{Timeout: cfg.timeout},
	}
}

func (s *scraper) get(path string, out any) error {
	req, err := http.NewRequest(http.MethodGet, s.cfg.baseURL+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+s.cfg.token)
	req.Header.Set("Accept", "application/json")

	resp, err := s.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		snippet := string(body)
		if len(snippet) > 200 {
			snippet = snippet[:200]
		}
		return fmt.Errorf("%s: unexpected status %d: %s", path, resp.StatusCode, snippet)
	}
	return json.Unmarshal(body, out)
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

package main

import (
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/armon/go-socks5"
)

// Config mirrors src/proxy/config.js defaultConfig (subset used by the Go proxy).
// The important part for mods: enableModder + mods array, same schema as the
// Node.js version so config.json files are interchangeable between the two.
type Config struct {
	Port          int        `json:"port"`
	HTTPPort      int        `json:"httpPort"`
	HTTPSPort     int        `json:"httpsPort"`
	Mode          string     `json:"mode"`
	Hostname      string     `json:"hostname"`
	CacheLocation string     `json:"cacheLocation"`
	ServerIP      string     `json:"serverIP"`
	Socks5Port    int        `json:"socks5Port"`
	Socks5Enabled bool       `json:"socks5Enabled"`
	EnableModder  bool       `json:"enableModder"`
	Mods          []ModEntry `json:"mods"`
}

// ModEntry is one entry of the "mods" array in config.json.
// Compatible with the Node.js implementation:
//
//	{ "path": "/path/to/MyMod.mod.json", "git": "...", "allowScripts": false }
//
// The Go proxy additionally honours an optional "enabled" flag
// (default true when omitted) so a mod can be toggled without
// removing it from the list:
//
//	{ "path": "/path/to/MyMod.mod.json", "enabled": false }
//
// Legacy fields from the early Go rewrite (id/name/type/url/branch/autoApply)
// are kept so old configs still parse, but "path" is what locates the mod.
type ModEntry struct {
	Path         string `json:"path"`
	Git          string `json:"git,omitempty"`
	AllowScripts bool   `json:"allowScripts,omitempty"`
	Enabled      *bool  `json:"enabled,omitempty"`

	// Legacy / informational fields, ignored for resolution.
	ID        string `json:"id,omitempty"`
	Name      string `json:"name,omitempty"`
	Type      string `json:"type,omitempty"`
	URL       string `json:"url,omitempty"`
	Branch    string `json:"branch,omitempty"`
	AutoApply bool   `json:"autoApply,omitempty"`
}

// EnabledOrDefault reports whether the mod entry is active.
// Omitted "enabled" means true (matches Node behaviour where every
// listed mod is active while enableModder is on).
func (m ModEntry) EnabledOrDefault() bool {
	if m.Enabled == nil {
		return true
	}
	return *m.Enabled
}

// ModDir returns the directory containing the mod files.
func (m ModEntry) ModDir() string {
	p := os.ExpandEnv(m.Path)
	if strings.HasSuffix(strings.ToLower(p), ".mod.json") {
		return filepath.Dir(p)
	}
	return p
}

func loadConfig() *Config {
	configPath := os.Getenv("DATA_DIR")
	if configPath == "" {
		configPath = "."
	}

	// Try config.json in DATA_DIR first, then the baked-in Docker default.
	// (The image copies docker/config.json to /app/config.json.)
	variants := []string{
		filepath.Join(configPath, "config.json"),
		filepath.Join(configPath, "ProxyData", "config.json"),
		"/app/config.json",
		"config.json",
	}

	for _, p := range variants {
		if data, err := os.ReadFile(p); err == nil {
			var cfg Config
			if err := json.Unmarshal(data, &cfg); err == nil {
				log.Printf("Loaded config from: %s", p)
				return &cfg
			}
		}
	}

	// Return default config if none found
	log.Println("No config.json found, using defaults")
	return &Config{
		Port:         8080,
		Mode:         "http-https",
		Hostname:     "0.0.0.0",
		ServerIP:     "w17k.kancolle-server.com",
		Socks5Port:   1080,
		EnableModder: false,
		Mods:         []ModEntry{},
	}
}

// kcPaths are the URL prefixes the proxy treats as game assets.
// Anything else is passed through untouched (same list as src/proxy/proxy.js).
var kcPaths = []string{"/kcs/", "/kcs2/", "/kcscontents/", "/gadget_html5/", "/html/", "/kca/"}

func isKCPath(p string) bool {
	for _, prefix := range kcPaths {
		if strings.HasPrefix(p, prefix) {
			return true
		}
	}
	return false
}

// ----------------------------------------------------------------------------
// Mod overlay
// ----------------------------------------------------------------------------

// ModRegistry maps a KC request path (e.g. "/kcs2/resources/ship/...png")
// to a local file on disk. Later mods win over earlier ones, mirroring the
// Node implementation where modCache entries are overwritten in config order
// (see src/proxy/mod/patcher.js reloadModCache/prepareDir).
type ModRegistry struct {
	mu    sync.RWMutex
	files map[string]string // request path -> local file path
	mods  []string          // mod dirs in load order (for logging / `mod list`)
}

// NewModRegistry scans every enabled mod in cfg (in order) and builds the
// overlay map. It returns the registry plus a list of non-fatal warnings.
func NewModRegistry(cfg *Config) (*ModRegistry, []string) {
	r := &ModRegistry{files: map[string]string{}}
	var warnings []string

	if !cfg.EnableModder {
		return r, warnings
	}

	for _, m := range cfg.Mods {
		if strings.TrimSpace(m.Path) == "" {
			warnings = append(warnings, "skipping mod with empty path")
			continue
		}
		if !m.EnabledOrDefault() {
			log.Printf("Mod disabled, skipping: %s", m.Path)
			continue
		}
		dir := m.ModDir()
		st, err := os.Stat(dir)
		if err != nil || !st.IsDir() {
			msg := fmt.Sprintf("mod dir not found, skipping: %s (from %s)", dir, m.Path)
			log.Printf("WARNING: %s", msg)
			warnings = append(warnings, msg)
			continue
		}
		count := 0
		err = filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return nil // skip unreadable entries
			}
			if d.IsDir() {
				return nil
			}
			rel, err := filepath.Rel(dir, path)
			if err != nil {
				return nil
			}
			rel = filepath.ToSlash(rel)
			reqPath, ok := modFileToRequestPath(rel)
			if !ok {
				return nil // manifest, readme, originals, scripts, ...
			}
			if prev, exists := r.files[reqPath]; exists {
				log.Printf("Mod override: %s (was %s, now %s)", reqPath, prev, path)
			}
			r.files[reqPath] = path
			count++
			return nil
		})
		if err != nil {
			msg := fmt.Sprintf("error scanning mod %s: %v", dir, err)
			log.Printf("WARNING: %s", msg)
			warnings = append(warnings, msg)
			continue
		}
		r.mods = append(r.mods, dir)
		log.Printf("Mod loaded: %s (%d servable files)", dir, count)
	}

	log.Printf("Mod registry: %d files from %d mod(s)", len(r.files), len(r.mods))
	return r, warnings
}

// Lookup returns the local file for a request path, if any mod provides it.
func (r *ModRegistry) Lookup(requestPath string) (string, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	f, ok := r.files[requestPath]
	return f, ok
}

// List returns the loaded mod directories in priority order.
func (r *ModRegistry) List() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, len(r.mods))
	copy(out, r.mods)
	return out
}

// modFileToRequestPath converts a mod-relative file path to the KC request
// path it serves. It supports two layouts:
//
//  1. Simple mirror (recommended for the Go proxy):
//     <modDir>/kcs2/resources/ship/full/xyz.png -> /kcs2/resources/ship/full/xyz.png
//
//  2. Node-style patched layout (best effort, full-file only):
//     <modDir>/kcs2/resources/ship/patched/xyz.png -> /kcs2/resources/ship/xyz.png
//     Files under original/, patcher/ or ignore/ are skipped: originals are
//     reference images for sprite-diffing (not served), and patcher scripts
//     require the Node runtime (allowScripts), which the Go proxy does not
//     execute.
//
// Anything that does not map to a KC path (manifest *.mod.json, *.md,
// dotfiles, ...) returns ok=false.
//
// NOTE: the Node proxy can splice individual sprites inside a spritesheet via
// image-diffing (src/proxy/mod/patcher.js). The Go proxy does full-file
// override only; sprite-level original/patched pairs must be flattened to
// complete files to work here. See USAGE.md.
func modFileToRequestPath(rel string) (reqPath string, ok bool) {
	base := filepath.ToSlash(rel)

	// Skip metadata / docs / hidden files at any depth.
	lower := strings.ToLower(base)
	if strings.HasSuffix(lower, ".mod.json") || strings.HasSuffix(lower, ".md") {
		return "", false
	}
	for _, seg := range strings.Split(base, "/") {
		if strings.HasPrefix(seg, ".") {
			return "", false
		}
	}

	// Node-style layout markers.
	if strings.Contains(base, "/original/") {
		return "", false // reference image, never served
	}
	if strings.Contains(base, "/patcher/") || strings.Contains(base, "/ignore/") {
		return "", false // scripts / ignored files need the Node runtime
	}
	if idx := strings.Index(base, "/patched/"); idx >= 0 {
		prefix := base[:idx]
		suffix := base[idx+len("/patched/"):]
		if suffix == "" {
			return "", false
		}
		candidate := "/" + prefix + "/" + suffix
		if !isKCPath(candidate) {
			return "", false
		}
		return candidate, true
	}

	// Flat original/patched/patcher filename prefixes (e.g. kcs2/x/patchedFoo.png).
	if slash := strings.LastIndex(base, "/"); slash >= 0 {
		dir, file := base[:slash], base[slash+1:]
		for _, p := range []string{"original", "patched", "patcher", "ignore"} {
			if strings.HasPrefix(strings.ToLower(file), p) && len(file) > len(p) {
				if p != "patched" {
					return "", false
				}
				stripped := file[len(p):]
				candidate := "/" + dir + "/" + stripped
				if !isKCPath(candidate) {
					return "", false
				}
				return candidate, true
			}
		}
	}

	candidate := "/" + base
	if !isKCPath(candidate) {
		return "", false
	}
	return candidate, true
}

func contentTypeFor(path string) string {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".png":
		return "image/png"
	case ".json":
		return "application/json"
	case ".css":
		return "text/css"
	case ".mp3":
		return "audio/mpeg"
	case ".js":
		return "application/x-javascript"
	case ".html":
		return "text/html"
	default:
		return "application/octet-stream"
	}
}

func main() {
	log.Println("KCCacheProxy starting...")

	// `main mod list` prints the resolved mod overlay without starting the proxy.
	// Useful inside Docker: `docker exec kccp /app/main mod list`.
	if len(os.Args) > 1 && os.Args[1] == "mod" {
		runModCommand(os.Args[2:])
		return
	}

	cfg := loadConfig()
	log.Printf("Config: port=%d, mode=%s, server=%s, socks5=%d, enableModder=%v, mods=%d",
		cfg.Port, cfg.Mode, cfg.ServerIP, cfg.Socks5Port, cfg.EnableModder, len(cfg.Mods))

	registry, _ := NewModRegistry(cfg)

	// Initialize SOCKS5 server (in background so the HTTP proxy can start too).
	socksConf := &socks5.Config{
		ResolveTCPFunc: resolveDNS,
		AuthMethods: []socks5.AuthMethod{
			socks5.AuthMethodNone,
		},
	}

	socksServer, err := socks5.New(socksConf)
	if err != nil {
		log.Fatalf("Failed to create SOCKS5 server: %v", err)
	}

	go func() {
		socksAddr := fmt.Sprintf("%s:%d", cfg.Hostname, cfg.Socks5Port)
		log.Printf("SOCKS5 server listening on %s", socksAddr)
		if err := socksServer.ListenAndServe("tcp", socksAddr); err != nil {
			log.Printf("SOCKS5 server stopped: %v", err)
		}
	}()

	// Setup HTTP reverse proxy
	proxy := &httputil.ReverseProxy{
		Director: func(r *http.Request) {
			r.URL.Scheme = "https"
			r.URL.Host = cfg.ServerIP
			// Preserve path for KC paths
			if strings.HasPrefix(r.URL.Path, "/kcs/") ||
				strings.HasPrefix(r.URL.Path, "/kcs2/") ||
				strings.HasPrefix(r.URL.Path, "/kcscontents/") ||
				strings.HasPrefix(r.URL.Path, "/gadget_html5/") ||
				strings.HasPrefix(r.URL.Path, "/html/") ||
				strings.HasPrefix(r.URL.Path, "/kca/") {
				// Keep path as-is
			} else {
				r.URL.Path = "/" + strings.TrimPrefix(r.URL.Path, "/")
			}
		},
	}

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Strip query string for overlay lookup: version tags (?version=xx)
		// must not prevent a mod match.
		lookupPath := r.URL.Path
		if idx := strings.Index(lookupPath, "?"); idx >= 0 {
			lookupPath = lookupPath[:idx]
		}

		if cfg.EnableModder && isKCPath(lookupPath) {
			if localFile, ok := registry.Lookup(lookupPath); ok {
				serveModFile(w, r, lookupPath, localFile)
				return
			}
		}
		proxy.ServeHTTP(w, r)
	})

	// HTTP server
	addr := fmt.Sprintf("%s:%d", cfg.Hostname, cfg.Port)
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		log.Fatalf("Failed to listen on %s: %v", addr, err)
	}
	log.Printf("HTTP proxy listening on %s", addr)

	server := &http.Server{
		Addr:    addr,
		Handler: handler,
	}

	// Handle shutdown
	go func() {
		if err := server.Serve(listener); err != nil && err != http.ErrServerClosed {
			log.Fatalf("Server error: %v", err)
		}
	}()

	log.Println("KCCacheProxy running. Press Ctrl+C to stop.")
	select {}
}

// serveModFile serves a single file from the mod overlay with the same
// headers the Node cacher uses (see src/proxy/cacher.js send()).
func serveModFile(w http.ResponseWriter, r *http.Request, requestPath, localFile string) {
	f, err := os.Open(localFile)
	if err != nil {
		log.Printf("Mod file unreadable %s (%s): %v, falling back to upstream", requestPath, localFile, err)
		http.Error(w, "mod file unreadable", http.StatusInternalServerError)
		return
	}
	defer f.Close()

	st, err := f.Stat()
	if err != nil {
		http.Error(w, "mod file unreadable", http.StatusInternalServerError)
		return
	}

	log.Printf("MOD HIT %s -> %s", requestPath, localFile)
	w.Header().Set("Content-Type", contentTypeFor(localFile))
	w.Header().Set("Server", "nginx")
	w.Header().Set("X-KCCP-Mod", localFile)
	w.Header().Set("Cache-Control", "max-age=2592000, public, immutable")
	w.Header().Set("Pragma", "public")
	http.ServeContent(w, r, filepath.Base(localFile), st.ModTime(), f)
}

// runModCommand implements the `main mod <subcommand>` CLI.
func runModCommand(args []string) {
	sub := ""
	if len(args) > 0 {
		sub = args[0]
	}
	cfg := loadConfig()
	switch sub {
	case "list", "":
		registry, warnings := NewModRegistry(cfg)
		fmt.Printf("enableModder: %v\n", cfg.EnableModder)
		fmt.Printf("config mods entries: %d\n", len(cfg.Mods))
		for i, m := range cfg.Mods {
			fmt.Printf("  [%d] path=%s enabled=%v dir=%s\n", i, m.Path, m.EnabledOrDefault(), m.ModDir())
		}
		fmt.Printf("loaded mod dirs: %d\n", len(registry.List()))
		for _, d := range registry.List() {
			fmt.Printf("  - %s\n", d)
		}
		fmt.Printf("overlay files: %d\n", len(registry.files))
		for _, w := range warnings {
			fmt.Printf("warning: %s\n", w)
		}
		if !cfg.EnableModder {
			fmt.Println("NOTE: enableModder is false, mods are parsed but not served. Set \"enableModder\": true in config.json.")
		}
	default:
		fmt.Fprintf(os.Stderr, "unknown mod subcommand %q (try: list)\n", sub)
		os.Exit(1)
	}
}

func resolveDNS(dest string) (net.IP, error) {
	hosts, err := net.LookupHost(dest)
	if err != nil {
		return nil, err
	}
	if len(hosts) > 0 {
		return net.ParseIP(hosts[0]), nil
	}
	return nil, fmt.Errorf("could not resolve %s", dest)
}

// Silence unused imports when optional features are compiled out.
var (
	_ = tls.VersionTLS12
	_ = io.Discard
	_ = time.Now
)

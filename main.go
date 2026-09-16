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

type Config struct {
	Port          int      `json:"port"`
	HTTPPort      int      `json:"httpPort"`
	HTTPSPort     int      `json:"httpsPort"`
	Mode          string   `json:"mode"`
	Hostname      string   `json:"hostname"`
	CacheLocation string   `json:"cacheLocation"`
	ServerIP      string   `json:"serverIP"`
	Socks5Port    int      `json:"socks5Port"`
	Socks5Enabled bool     `json:"socks5Enabled"`
	Mods          []ModConfig `json:"mods"`
}

type ModConfig struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Type      string `json:"type"`
	URL       string `json:"url"`
	Branch    string `json:"branch"`
	Enabled   bool   `json:"enabled"`
	AutoApply bool   `json:"autoApply"`
}

func loadConfig() *Config {
	configPath := os.Getenv("DATA_DIR")
	if configPath == "" {
		configPath = "."
	}
	
	// Try config.json in current dir, then DATA_DIR
	variants := []string{
		filepath.Join(configPath, "config.json"),
		filepath.Join(configPath, "ProxyData", "config.json"),
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
		Port:    8080,
		Mode:    "http-https",
		Hostname: "0.0.0.0",
		ServerIP: "w17k.kancolle-server.com",
		Socks5Port: 1080,
		Mods:    []ModConfig{},
	}
}

func main() {
	log.Println("KCCacheProxy starting...")

	cfg := loadConfig()
	log.Printf("Config: port=%d, mode=%s, server=%s, socks5=%d", 
		cfg.Port, cfg.Mode, cfg.ServerIP, cfg.Socks5Port)

	// Initialize SOCKS5 server
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

	// Start SOCKS5 listener
	socksAddr := fmt.Sprintf("%s:%d", cfg.Hostname, cfg.Socks5Port)
	if err := socksServer.ListenAndServe("tcp", socksAddr); err != nil {
		log.Fatalf("Failed to start SOCKS5: %v", err)
	}
	log.Printf("SOCKS5 server listening on %s", socksAddr)

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

	// HTTP server
	addr := fmt.Sprintf("%s:%d", cfg.Hostname, cfg.Port)
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		log.Fatalf("Failed to listen on %s: %v", addr, err)
	}
	log.Printf("HTTP proxy listening on %s", addr)

	server := &http.Server{
		Addr: addr,
		Handler: proxy,
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
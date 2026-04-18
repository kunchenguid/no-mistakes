package telemetry

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/buildinfo"
)

const (
	defaultHostname = "cli"
	defaultTitle    = "no-mistakes CLI"
	defaultPath     = "/api/send"
	umamiCloudURL   = "https://cloud.umami.is"

	umamiHostEnv      = "NO_MISTAKES_UMAMI_HOST"
	umamiWebsiteIDEnv = "NO_MISTAKES_UMAMI_WEBSITE_ID"
)

type Fields map[string]any

type Sink interface {
	Track(name string, fields Fields)
	Close(ctx context.Context) error
}

type Config struct {
	Host       string
	WebsiteID  string
	App        string
	Version    string
	GOOS       string
	GOARCH     string
	HTTPClient *http.Client
}

type Client struct {
	endpoint   string
	websiteID  string
	app        string
	version    string
	goos       string
	goarch     string
	httpClient *http.Client

	mu     sync.Mutex
	closed bool
	wg     sync.WaitGroup
}

type noopSink struct{}

type collectRequest struct {
	Type    string         `json:"type"`
	Payload collectPayload `json:"payload"`
}

type collectPayload struct {
	Website   string         `json:"website"`
	Hostname  string         `json:"hostname"`
	Title     string         `json:"title"`
	URL       string         `json:"url"`
	Name      string         `json:"name"`
	Data      map[string]any `json:"data,omitempty"`
	Timestamp int64          `json:"timestamp,omitempty"`
}

var (
	defaultMu   sync.Mutex
	defaultSink Sink
)

func NewClient(cfg Config) (*Client, error) {
	endpoint, err := normalizeEndpoint(cfg.Host)
	if err != nil {
		return nil, err
	}
	if cfg.WebsiteID == "" {
		return nil, fmt.Errorf("website ID is required")
	}
	if cfg.App == "" {
		cfg.App = "no-mistakes"
	}
	if cfg.Version == "" {
		cfg.Version = buildinfo.CurrentVersion()
	}
	if cfg.GOOS == "" {
		cfg.GOOS = runtime.GOOS
	}
	if cfg.GOARCH == "" {
		cfg.GOARCH = runtime.GOARCH
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{Timeout: time.Second}
	}

	return &Client{
		endpoint:   endpoint,
		websiteID:  cfg.WebsiteID,
		app:        cfg.App,
		version:    cfg.Version,
		goos:       cfg.GOOS,
		goarch:     cfg.GOARCH,
		httpClient: cfg.HTTPClient,
	}, nil
}

func Default() Sink {
	defaultMu.Lock()
	defer defaultMu.Unlock()

	if defaultSink != nil {
		return defaultSink
	}

	websiteID := defaultWebsiteID()
	if websiteID == "" {
		defaultSink = noopSink{}
		return defaultSink
	}

	client, err := NewClient(Config{
		Host:      umamiCloudURL,
		WebsiteID: websiteID,
		App:       "no-mistakes",
		Version:   buildinfo.CurrentVersion(),
		GOOS:      runtime.GOOS,
		GOARCH:    runtime.GOARCH,
	})
	if err != nil {
		defaultSink = noopSink{}
		return defaultSink
	}

	defaultSink = client
	return defaultSink
}

func SetDefaultForTesting(sink Sink) func() {
	defaultMu.Lock()
	prev := defaultSink
	if sink == nil {
		sink = noopSink{}
	}
	defaultSink = sink
	defaultMu.Unlock()

	return func() {
		defaultMu.Lock()
		defaultSink = prev
		defaultMu.Unlock()
	}
}

func Track(name string, fields Fields) {
	Default().Track(name, fields)
}

func Close(ctx context.Context) error {
	return Default().Close(ctx)
}

func (c *Client) Track(name string, fields Fields) {
	if c == nil || strings.TrimSpace(name) == "" {
		return
	}

	body, err := c.newRequest(name, fields)
	if err != nil {
		return
	}

	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	c.wg.Add(1)
	c.mu.Unlock()

	go func(payload []byte) {
		defer c.wg.Done()
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		c.send(ctx, payload)
	}(body)
}

func (c *Client) Close(ctx context.Context) error {
	if c == nil {
		return nil
	}

	c.mu.Lock()
	c.closed = true
	c.mu.Unlock()

	done := make(chan struct{})
	go func() {
		defer close(done)
		c.wg.Wait()
	}()

	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (noopSink) Track(string, Fields) {}

func (noopSink) Close(context.Context) error { return nil }

func (c *Client) newRequest(name string, fields Fields) ([]byte, error) {
	data := make(map[string]any, len(fields)+4)
	for k, v := range fields {
		data[k] = v
	}
	data["app_version"] = c.version
	data["goos"] = c.goos
	data["goarch"] = c.goarch
	data["build_channel"] = buildChannel(c.version)

	return json.Marshal(collectRequest{
		Type: "event",
		Payload: collectPayload{
			Website:   c.websiteID,
			Hostname:  defaultHostname,
			Title:     defaultTitle,
			URL:       eventURL(c.app, name),
			Name:      name,
			Data:      data,
			Timestamp: time.Now().Unix(),
		},
	})
}

func (c *Client) send(ctx context.Context, payload []byte) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(payload))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", fmt.Sprintf("no-mistakes/%s telemetry", c.version))

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
}

func normalizeEndpoint(host string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(host))
	if err != nil {
		return "", fmt.Errorf("parse host: %w", err)
	}
	if u.Scheme == "" || u.Host == "" {
		return "", fmt.Errorf("invalid host %q", host)
	}
	if strings.HasSuffix(u.Path, defaultPath) {
		return strings.TrimRight(u.String(), "/"), nil
	}
	base := strings.TrimRight(u.Path, "/")
	if base == "" {
		u.Path = defaultPath
	} else {
		u.Path = base + defaultPath
	}
	return u.String(), nil
}

func eventURL(app, name string) string {
	if app == "" {
		app = "no-mistakes"
	}
	if name == "" {
		return "app://" + app
	}
	return "app://" + app + "/" + strings.ReplaceAll(name, ".", "/")
}

func buildChannel(version string) string {
	if version == "" || version == "dev" {
		return "dev"
	}
	return "release"
}

func defaultWebsiteID() string {
	websiteID := strings.TrimSpace(os.Getenv(umamiWebsiteIDEnv))

	if buildChannel(buildinfo.CurrentVersion()) == "dev" && websiteID == "" {
		values := loadDotEnvValues()
		websiteID = strings.TrimSpace(values[umamiWebsiteIDEnv])
	}

	if websiteID == "" {
		websiteID = strings.TrimSpace(buildinfo.TelemetryWebsiteID)
	}

	return websiteID
}

func loadDotEnvValues() map[string]string {
	wd, err := os.Getwd()
	if err != nil {
		return nil
	}

	path, ok := findDotEnv(wd)
	if !ok {
		return nil
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}

	return parseDotEnv(data)
}

func findDotEnv(dir string) (string, bool) {
	repoRoot, ok := findRepoRoot(dir)
	if !ok {
		path := filepath.Join(dir, ".env")
		if info, err := os.Stat(path); err == nil && !info.IsDir() {
			return path, true
		}
		return "", false
	}

	for {
		path := filepath.Join(dir, ".env")
		if info, err := os.Stat(path); err == nil && !info.IsDir() {
			return path, true
		}
		if dir == repoRoot {
			return "", false
		}

		parent := filepath.Dir(dir)
		dir = parent
	}
}

func findRepoRoot(dir string) (string, bool) {
	for {
		path := filepath.Join(dir, ".git")
		if _, err := os.Stat(path); err == nil {
			return dir, true
		}

		parent := filepath.Dir(dir)
		if parent == dir {
			return "", false
		}
		dir = parent
	}
}

func parseDotEnv(data []byte) map[string]string {
	values := map[string]string{}
	scanner := bufio.NewScanner(bytes.NewReader(data))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "export ") {
			line = strings.TrimSpace(strings.TrimPrefix(line, "export "))
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		if key == "" {
			continue
		}
		values[key] = dotenvValue(strings.TrimSpace(value))
	}
	return values
}

func dotenvValue(value string) string {
	if len(value) >= 2 {
		quote := value[0]
		if (quote == '"' || quote == '\'') && value[len(value)-1] == quote {
			if unquoted, err := strconv.Unquote(value); err == nil {
				return unquoted
			}
			return value[1 : len(value)-1]
		}
	}
	return value
}

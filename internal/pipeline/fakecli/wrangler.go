package main

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// The deployed directory is gone once the deploy returns, so its files are
// captured here.
type fakeWranglerRecord struct {
	Args        []string          `json:"args"`
	Dir         string            `json:"dir"`
	Credentials bool              `json:"credentials"`
	Metrics     string            `json:"metrics"`
	Files       map[string]string `json:"files"`
}

func isFakeWrangler(arg0 string) bool {
	base := filepath.Base(arg0)
	return strings.EqualFold(strings.TrimSuffix(base, filepath.Ext(base)), "wrangler")
}

func fakeWranglerMain(args []string) int {
	if len(args) < 3 || args[0] != "pages" || args[1] != "deploy" {
		fmt.Fprintln(os.Stderr, "fake wrangler only deploys pages")
		return 1
	}
	cwd, _ := os.Getwd()
	record := fakeWranglerRecord{
		Args:        args,
		Dir:         cwd,
		Credentials: os.Getenv("CLOUDFLARE_API_TOKEN") != "" && os.Getenv("CLOUDFLARE_ACCOUNT_ID") != "",
		Metrics:     os.Getenv("WRANGLER_SEND_METRICS"),
		Files:       map[string]string{},
	}
	site := args[2]
	_ = filepath.WalkDir(site, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(site, path)
		record.Files[filepath.ToSlash(rel)] = string(data)
		return nil
	})
	if logPath := os.Getenv("FAKE_WRANGLER_LOG"); logPath != "" {
		line, _ := json.Marshal(record)
		f, err := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if err == nil {
			fmt.Fprintln(f, string(line))
			f.Close()
		}
	}
	if msg := os.Getenv("FAKE_WRANGLER_FAIL"); msg != "" {
		fmt.Fprintln(os.Stderr, msg)
		return 1
	}
	project := ""
	for _, arg := range args {
		if value, ok := strings.CutPrefix(arg, "--project-name="); ok {
			project = value
		}
	}
	deployment := os.Getenv("FAKE_WRANGLER_URL")
	lines := []map[string]any{
		{"type": "wrangler-session", "version": 1, "wrangler_version": "4.156.0", "command_line_args": args, "log_file_path": filepath.Join(cwd, "wrangler.log"), "timestamp": "2026-11-01T20:00:00.000Z"},
		{"type": "pages-deploy", "version": 1, "pages_project": project, "deployment_id": "5d4c3b2a-0000-4000-8000-000000000000", "url": deployment, "timestamp": "2026-11-01T20:00:05.000Z"},
		{"type": "pages-deploy-detailed", "version": 1, "pages_project": project, "deployment_id": "5d4c3b2a-0000-4000-8000-000000000000", "url": deployment, "alias": "https://no-mistakes-evidence." + project + ".pages.dev", "environment": "preview", "production_branch": "main", "deployment_trigger": map[string]any{"metadata": map[string]any{"commit_hash": ""}}, "timestamp": "2026-11-01T20:00:05.000Z"},
	}
	f, err := os.OpenFile(os.Getenv("WRANGLER_OUTPUT_FILE_PATH"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer f.Close()
	for _, line := range lines {
		data, _ := json.Marshal(line)
		fmt.Fprintln(f, string(data))
	}
	fmt.Println("✨ Deployment complete! Take a peek over at " + deployment)
	return 0
}

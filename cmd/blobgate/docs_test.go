package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func read(t *testing.T, p string) string {
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func goSources(t *testing.T) string {
	var all strings.Builder
	for _, dir := range []string{"../../cmd", "../../internal"} {
		_ = filepath.Walk(dir, func(p string, i os.FileInfo, err error) error {
			if err == nil && strings.HasSuffix(p, ".go") && !strings.HasSuffix(p, "_test.go") {
				all.WriteString(read(t, p))
			}
			return nil
		})
	}
	return all.String()
}

// Every setting the program reads has a line in the reference (or in the backup page, which the reference points to); every metric has a line in the metrics page; every command has a line in the
// reference; every error code of the description is in the description of the API. A new setting is its line in the same change.
func TestEverySettingMetricAndCommandIsDocumented(t *testing.T) {
	src := goSources(t)
	docs := read(t, "../../docs/configuration.md") + read(t, "../../docs/backup.md")
	for _, name := range regexp.MustCompile(`CINIM_[A-Z_]+`).FindAllString(src, -1) {
		if !strings.Contains(docs, name) {
			t.Errorf("%s is read by the program and is not in docs/configuration.md or docs/backup.md", name)
		}
	}
	metrics := read(t, "../../docs/metrics.md")
	for _, m := range regexp.MustCompile(`"(blobgate_[a-z_]+)"`).FindAllStringSubmatch(read(t, "../../internal/metrics/metrics.go"), -1) {
		if !strings.Contains(metrics, m[1]) {
			t.Errorf("metric %s is not in docs/metrics.md", m[1])
		}
	}
	for _, m := range regexp.MustCompile(`Name: +"(blobgate_[a-z_]+)"`).FindAllStringSubmatch(read(t, "../../internal/metrics/metrics.go"), -1) {
		if !strings.Contains(metrics, m[1]) {
			t.Errorf("metric %s is not in docs/metrics.md", m[1])
		}
	}
	cmds := read(t, "../../docs/configuration.md")
	for _, c := range []string{"validate", "token", "backup", "backups", "verify", "restore", "version", "help"} {
		if !strings.Contains(cmds, "blobgate "+c) && !strings.Contains(cmds, "`"+c+"`") {
			t.Errorf("the command %s is not in docs/configuration.md", c)
		}
	}
	for _, flag := range []string{"-fallback", "-force", "-all", "-id", "-json", "-secret"} {
		if !strings.Contains(cmds+read(t, "../../docs/backup.md"), flag) {
			t.Errorf("the flag %s is in no page", flag)
		}
	}
	// the error codes of the API are in the description
	api := read(t, "../../internal/api/openapi.yaml")
	for _, code := range regexp.MustCompile(`problem\(w, \d+, "([a-z_0-9]+)"`).FindAllStringSubmatch(src, -1) {
		if !strings.Contains(api, code[1]) {
			t.Errorf("the error code %s is answered by the program and is not in the description of the API", code[1])
		}
	}
}

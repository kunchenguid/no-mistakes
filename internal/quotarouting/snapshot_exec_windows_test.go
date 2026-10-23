package quotarouting

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestReadSnapshotWindowsBatchShims(t *testing.T) {
	for _, ext := range []string{".cmd", ".BAT"} {
		for _, lookup := range []bool{false, true} {
			for _, fails := range []bool{false, true} {
				t.Run(ext+"/"+map[bool]string{false: "explicit", true: "PATH"}[lookup]+"/"+map[bool]string{false: "success", true: "error"}[fails], func(t *testing.T) {
					dir := filepath.Join(t.TempDir(), "quota %NM_QUOTA_TEST_EXPANSION% ! (space) & fixtures")
					if err := os.Mkdir(dir, 0o755); err != nil {
						t.Fatal(err)
					}
					exe := filepath.Join(dir, "quota-axi"+ext)
					script := "@echo off\r\nif not \"%~1\"==\"--full\" exit /b 3\r\nif not \"%~2\"==\"--json\" exit /b 4\r\nif not \"%~3\"==\"\" exit /b 5\r\n"
					if fails {
						script += "echo snapshot fixture failure 1>&2\r\nexit /b 7\r\n"
					} else {
						script += "echo {\"schemaVersion\":5,\"providers\":[]}\r\n"
					}
					if err := os.WriteFile(exe, []byte(script), 0o600); err != nil {
						t.Fatal(err)
					}
					t.Setenv("NM_QUOTA_TEST_EXPANSION", "unexpected-expanded-path")
					t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
					t.Setenv("PATHEXT", ".COM;.EXE;.BAT;.CMD")
					if lookup {
						exe = ""
					}
					snapshot, err := ReadSnapshot(context.Background(), exe, 5*time.Second)
					if fails {
						if err == nil || !strings.Contains(err.Error(), "snapshot fixture failure") {
							t.Fatalf("snapshot=%+v error=%v", snapshot, err)
						}
					} else if err != nil || snapshot == nil || snapshot.SchemaVersion != 5 || len(snapshot.Providers) != 0 {
						t.Fatalf("snapshot=%+v error=%v", snapshot, err)
					}
				})
			}
		}
	}
}

func TestReadSnapshotWindowsBatchTimeout(t *testing.T) {
	exe := filepath.Join(t.TempDir(), "quota-axi.cmd")
	if err := os.WriteFile(exe, []byte("@echo off\r\n:wait\r\ngoto wait\r\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	_, err := ReadSnapshot(context.Background(), exe, 200*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "did not finish within") || time.Since(started) > 5*time.Second {
		t.Fatalf("elapsed=%s error=%v", time.Since(started), err)
	}
}

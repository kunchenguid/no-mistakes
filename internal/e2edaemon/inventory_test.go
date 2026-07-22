package e2edaemon

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

func TestInventoryRegisterUpdateUnregister(t *testing.T) {
	dir := t.TempDir()
	inv, err := OpenDir(dir)
	if err != nil {
		t.Fatalf("OpenDir: %v", err)
	}

	home := filepath.Join(dir, "nm-e2e-case", "nmhome")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := inv.Register("e1", home, "/bin/false", 0, os.Getpid()); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if err := inv.UpdatePID("e1", 4242); err != nil {
		t.Fatalf("UpdatePID: %v", err)
	}
	list, err := inv.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 1 || list[0].PID != 4242 || list[0].NMHome != filepath.Clean(home) {
		t.Fatalf("list = %+v", list)
	}
	if err := inv.Unregister("e1"); err != nil {
		t.Fatalf("Unregister: %v", err)
	}
	list, err = inv.List()
	if err != nil {
		t.Fatalf("List after unregister: %v", err)
	}
	if len(list) != 0 {
		t.Fatalf("want empty list, got %+v", list)
	}
}

func TestInventoryCorruptFileRecoversEmpty(t *testing.T) {
	dir := t.TempDir()
	inv, err := OpenDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(inv.path(), []byte("not-json"), 0o600); err != nil {
		t.Fatal(err)
	}
	list, err := inv.List()
	if err != nil {
		t.Fatalf("List corrupt: %v", err)
	}
	if len(list) != 0 {
		t.Fatalf("corrupt inventory should read as empty, got %+v", list)
	}
}

func TestReapAbandonedInventories(t *testing.T) {
	parent := t.TempDir()
	active := filepath.Join(parent, "run-active")
	live := filepath.Join(parent, "run-live")
	stale := filepath.Join(parent, "run-stale")
	for _, dir := range []string{active, live, stale} {
		inv, err := OpenDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		if err := inv.Register(filepath.Base(dir), filepath.Join(dir, "nm-e2e-case", "nmhome"), "", 0, os.Getpid()); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(active, ownerFileName), []byte(strconv.Itoa(os.Getpid())), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(live, ownerFileName), []byte(strconv.Itoa(os.Getpid())), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stale, ownerFileName), []byte("999999999"), 0o600); err != nil {
		t.Fatal(err)
	}

	if errs := ReapAbandoned(parent, active); len(errs) > 0 {
		t.Fatalf("ReapAbandoned: %v", errs)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatalf("stale inventory still exists: %v", err)
	}
	for _, dir := range []string{active, live} {
		inv, err := OpenDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		entries, err := inv.List()
		if err != nil || len(entries) != 1 {
			t.Fatalf("live inventory %s changed: entries=%+v err=%v", dir, entries, err)
		}
	}
}

func TestCommandMatchesDaemonRoot(t *testing.T) {
	home := "/private/tmp/nm-e2e-x/nmhome"
	cmd := "/tmp/nm-e2e-bin/no-mistakes daemon run --root " + home
	if !commandMatchesDaemonRoot(cmd, home) {
		t.Fatal("expected match for exact root")
	}
	if commandMatchesDaemonRoot(cmd, "/Users/me/.no-mistakes") {
		t.Fatal("must not match shared root")
	}
	if commandMatchesDaemonRoot("sleep 3600", home) {
		t.Fatal("must not match non-daemon")
	}
	if commandMatchesDaemonRoot("/bin/no-mistakes daemon run --root=/private/tmp/nm-e2e-x/nmhome", home) {
		// equals form
	} else {
		t.Fatal("expected --root= form to match")
	}
}

func TestSplitWindowsTokensPreservesRootPath(t *testing.T) {
	command := `"C:\Program Files\no-mistakes.exe" daemon run --root "C:\Users\tester\AppData\Local\Temp\nm-e2e-1\nmhome"`
	tokens := splitWindowsTokens(command)
	want := `C:\Users\tester\AppData\Local\Temp\nm-e2e-1\nmhome`
	root, ok := extractRootTokens(tokens)
	if !ok || root != want {
		t.Fatalf("root = %q, %v; want %q, true; tokens=%q", root, ok, want, tokens)
	}
}

func TestReapAllRetainsEntryWhenProcessCheckIsInconclusive(t *testing.T) {
	dir := t.TempDir()
	inv, err := OpenDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	nmHome := filepath.Join(dir, "nm-e2e-retry", "nmhome")
	if err := inv.Register("retry", nmHome, "", 0, os.Getpid()); err != nil {
		t.Fatal(err)
	}
	inv.findDaemons = func(string) ([]int, error) {
		return nil, os.ErrPermission
	}

	result := inv.ReapAll()
	if result.Removed != 0 || len(result.Errors) == 0 {
		t.Fatalf("reap result = %+v", result)
	}
	entries, err := inv.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].ID != "retry" {
		t.Fatalf("entry was not retained: %+v", entries)
	}
}

func TestIsAllowedTempRoot(t *testing.T) {
	cases := []struct {
		path string
		want bool
	}{
		{"/private/tmp/nm-e2e-abc/nmhome", true},
		{"/tmp/nmh-xyz/nm-home", true},
		{"/var/folders/xx/nm-e2e-1/nmhome", true},
		{"/Users/someone/.no-mistakes", false},
		{"/", false},
		{"", false},
		{".", false},
	}
	if home, err := os.UserHomeDir(); err == nil {
		cases = append(cases, struct {
			path string
			want bool
		}{filepath.Join(home, ".no-mistakes"), false})
	}
	for _, tc := range cases {
		if got := isAllowedTempRoot(tc.path); got != tc.want {
			t.Errorf("isAllowedTempRoot(%q) = %v, want %v", tc.path, got, tc.want)
		}
	}
}

func TestConcurrencySlotCap(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(EnvInventory, dir)
	t.Setenv(EnvMaxConcurrent, "1")
	inv, err := OpenDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	s1, err := inv.AcquireSlot(time.Second)
	if err != nil {
		t.Fatalf("first slot: %v", err)
	}
	defer s1.Release()

	done := make(chan error, 1)
	go func() {
		s2, err := inv.AcquireSlot(150 * time.Millisecond)
		if s2 != nil {
			s2.Release()
		}
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected second slot acquire to time out under cap=1")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("slot acquire hung")
	}

	s1.Release()
	s3, err := inv.AcquireSlot(time.Second)
	if err != nil {
		t.Fatalf("slot after release: %v", err)
	}
	s3.Release()
}

func TestReapAllRefusesSharedRoot(t *testing.T) {
	dir := t.TempDir()
	inv, err := OpenDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip(err)
	}
	shared := filepath.Join(home, ".no-mistakes")
	if err := inv.Register("bad", shared, "", 1, os.Getpid()); err != nil {
		t.Fatal(err)
	}
	result := inv.ReapAll()
	if result.Skipped != 1 {
		t.Fatalf("skipped = %d, want 1; result=%+v", result.Skipped, result)
	}
	// Entry removed so it cannot be retried.
	list, err := inv.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 0 {
		t.Fatalf("shared root entry should be dropped, got %+v", list)
	}
}

package management

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestWriteConfigRestrictsPermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX file modes are not enforced on Windows")
	}

	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("debug: false\n"), 0o644); err != nil {
		t.Fatalf("seed config: %v", err)
	}

	if err := WriteConfig(path, []byte("debug: true\n")); err != nil {
		t.Fatalf("WriteConfig: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat config: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("config mode = %o, want 600", got)
	}
}

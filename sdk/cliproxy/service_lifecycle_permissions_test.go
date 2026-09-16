package cliproxy

import (
	"os"
	"runtime"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
)

func TestEnsureAuthDirRestrictsPermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX directory modes are not enforced on Windows")
	}

	authDir := t.TempDir()
	if err := os.Chmod(authDir, 0o755); err != nil {
		t.Fatalf("seed auth directory mode: %v", err)
	}

	service := &Service{cfg: &config.Config{AuthDir: authDir}}
	if err := service.ensureAuthDir(); err != nil {
		t.Fatalf("ensureAuthDir: %v", err)
	}

	info, err := os.Stat(authDir)
	if err != nil {
		t.Fatalf("stat auth directory: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o700 {
		t.Fatalf("auth directory mode = %o, want 700", got)
	}
}

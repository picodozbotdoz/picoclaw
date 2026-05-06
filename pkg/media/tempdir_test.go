package media

import (
	"os"
	"path/filepath"
	"testing"
)

func TestTempDir_DefaultPath(t *testing.T) {
	// Reset override
	SetTempDir("")

	got := TempDir()
	want := filepath.Join(os.TempDir(), TempDirName)
	if got != want {
		t.Errorf("TempDir() = %q, want %q", got, want)
	}
}

func TestTempDir_OverridePath(t *testing.T) {
	custom := "/home/user/.picoclaw/tmp"
	SetTempDir(custom)

	got := TempDir()
	if got != custom {
		t.Errorf("TempDir() = %q, want %q", got, custom)
	}

	// Reset
	SetTempDir("")
}

func TestTempDir_OverrideEmptyRestoresDefault(t *testing.T) {
	// Set override
	SetTempDir("/some/custom/path")
	if TempDir() != "/some/custom/path" {
		t.Fatalf("override not applied")
	}

	// Reset to default
	SetTempDir("")
	want := filepath.Join(os.TempDir(), TempDirName)
	if got := TempDir(); got != want {
		t.Errorf("after clearing override, TempDir() = %q, want %q", got, want)
	}
}

func TestTempDir_ConcurrentAccess(t *testing.T) {
	done := make(chan string, 2)

	go func() {
		SetTempDir("/concurrent/a")
		done <- TempDir()
	}()

	go func() {
		SetTempDir("/concurrent/b")
		done <- TempDir()
	}()

	// Just verify no panics or data races
	<-done
	<-done

	// Reset
	SetTempDir("")
}

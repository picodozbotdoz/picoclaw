package media

import (
	"os"
	"path/filepath"
	"sync"
)

const TempDirName = "picoclaw_media"

var (
	overrideDir string
	overrideMu  sync.RWMutex
)

// SetTempDir sets the override directory for temporary media file storage.
// When set, TempDir() returns this path instead of os.TempDir()/picoclaw_media.
// Pass an empty string to revert to the default location.
func SetTempDir(dir string) {
	overrideMu.Lock()
	defer overrideMu.Unlock()
	overrideDir = dir
}

// TempDir returns the shared temporary directory used for downloaded media.
// If an override has been set via SetTempDir, that path is returned.
// Otherwise it defaults to os.TempDir()/picoclaw_media.
func TempDir() string {
	overrideMu.RLock()
	dir := overrideDir
	overrideMu.RUnlock()

	if dir != "" {
		return dir
	}
	return filepath.Join(os.TempDir(), TempDirName)
}

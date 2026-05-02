package agent

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDetectProject_GoProject(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module example.com/test\n"), 0o644); err != nil {
		t.Fatalf("write go.mod: %v", err)
	}

	info := DetectProject(dir)
	if info.Type != ProjectTypeGo {
		t.Errorf("Type = %q, want %q", info.Type, ProjectTypeGo)
	}
	if info.BuildCmd != "go build ./..." {
		t.Errorf("BuildCmd = %q, want %q", info.BuildCmd, "go build ./...")
	}
	if info.TestCmd != "go test ./..." {
		t.Errorf("TestCmd = %q, want %q", info.TestCmd, "go test ./...")
	}
	if info.RootDir != dir {
		t.Errorf("RootDir = %q, want %q", info.RootDir, dir)
	}
}

func TestDetectProject_NodeProject(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "package.json"), []byte(`{"name":"test"}`), 0o644); err != nil {
		t.Fatalf("write package.json: %v", err)
	}

	info := DetectProject(dir)
	if info.Type != ProjectTypeNode {
		t.Errorf("Type = %q, want %q", info.Type, ProjectTypeNode)
	}
	if info.BuildCmd != "npm run build" {
		t.Errorf("BuildCmd = %q, want %q", info.BuildCmd, "npm run build")
	}
	if info.TestCmd != "npm test" {
		t.Errorf("TestCmd = %q, want %q", info.TestCmd, "npm test")
	}
}

func TestDetectProject_RustProject(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "Cargo.toml"), []byte("[package]\nname = \"test\"\n"), 0o644); err != nil {
		t.Fatalf("write Cargo.toml: %v", err)
	}

	info := DetectProject(dir)
	if info.Type != ProjectTypeRust {
		t.Errorf("Type = %q, want %q", info.Type, ProjectTypeRust)
	}
	if info.BuildCmd != "cargo build" {
		t.Errorf("BuildCmd = %q, want %q", info.BuildCmd, "cargo build")
	}
}

func TestDetectProject_PythonProject_PyProjectToml(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "pyproject.toml"), []byte("[project]\nname = 'test'\n"), 0o644); err != nil {
		t.Fatalf("write pyproject.toml: %v", err)
	}

	info := DetectProject(dir)
	if info.Type != ProjectTypePython {
		t.Errorf("Type = %q, want %q", info.Type, ProjectTypePython)
	}
	if info.TestCmd != "pytest" {
		t.Errorf("TestCmd = %q, want %q", info.TestCmd, "pytest")
	}
}

func TestDetectProject_PythonProject_SetupPy(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "setup.py"), []byte("from setuptools import setup\n"), 0o644); err != nil {
		t.Fatalf("write setup.py: %v", err)
	}

	info := DetectProject(dir)
	if info.Type != ProjectTypePython {
		t.Errorf("Type = %q, want %q", info.Type, ProjectTypePython)
	}
}

func TestDetectProject_MakeProject(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "Makefile"), []byte("all:\n\techo hello\n"), 0o644); err != nil {
		t.Fatalf("write Makefile: %v", err)
	}

	info := DetectProject(dir)
	if info.Type != ProjectTypeMake {
		t.Errorf("Type = %q, want %q", info.Type, ProjectTypeMake)
	}
	if info.BuildCmd != "make" {
		t.Errorf("BuildCmd = %q, want %q", info.BuildCmd, "make")
	}
	if info.TestCmd != "make test" {
		t.Errorf("TestCmd = %q, want %q", info.TestCmd, "make test")
	}
}

func TestDetectProject_Makefile_VariantNames(t *testing.T) {
	for _, name := range []string{"makefile", "GNUmakefile"} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, name), []byte("all:\n\techo hello\n"), 0o644); err != nil {
				t.Fatalf("write %s: %v", name, err)
			}

			info := DetectProject(dir)
			if info.Type != ProjectTypeMake {
				t.Errorf("Type = %q, want %q for %s", info.Type, ProjectTypeMake, name)
			}
		})
	}
}

func TestDetectProject_UnknownProject(t *testing.T) {
	dir := t.TempDir()

	info := DetectProject(dir)
	if info.Type != ProjectTypeUnknown {
		t.Errorf("Type = %q, want %q", info.Type, ProjectTypeUnknown)
	}
	if info.BuildCmd != "" {
		t.Errorf("BuildCmd = %q, want empty for unknown project", info.BuildCmd)
	}
	if info.TestCmd != "" {
		t.Errorf("TestCmd = %q, want empty for unknown project", info.TestCmd)
	}
	if info.RootDir != dir {
		t.Errorf("RootDir = %q, want %q", info.RootDir, dir)
	}
}

func TestDetectProject_PriorityOrder(t *testing.T) {
	// Go is detected before Node when both markers exist
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module test\n"), 0o644); err != nil {
		t.Fatalf("write go.mod: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "package.json"), []byte(`{"name":"test"}`), 0o644); err != nil {
		t.Fatalf("write package.json: %v", err)
	}

	info := DetectProject(dir)
	if info.Type != ProjectTypeGo {
		t.Errorf("Type = %q, want %q (Go should take priority over Node)", info.Type, ProjectTypeGo)
	}
}

func TestDetectProject_NonExistentDirectory(t *testing.T) {
	info := DetectProject("/nonexistent/path/that/does/not/exist")
	if info.Type != ProjectTypeUnknown {
		t.Errorf("Type = %q, want %q for non-existent directory", info.Type, ProjectTypeUnknown)
	}
}

func TestDetectProject_PyProjectTakesPriorityOverSetupPy(t *testing.T) {
	// When both pyproject.toml and setup.py exist, pyproject.toml wins
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "pyproject.toml"), []byte("[project]\n"), 0o644); err != nil {
		t.Fatalf("write pyproject.toml: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "setup.py"), []byte("from setuptools import setup\n"), 0o644); err != nil {
		t.Fatalf("write setup.py: %v", err)
	}

	info := DetectProject(dir)
	if info.Type != ProjectTypePython {
		t.Errorf("Type = %q, want %q", info.Type, ProjectTypePython)
	}
}

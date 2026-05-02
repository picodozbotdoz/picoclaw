package agent

import (
	"os"
	"path/filepath"
)

// ProjectType identifies the kind of project in the workspace.
type ProjectType string

const (
	ProjectTypeGo      ProjectType = "go"
	ProjectTypeNode    ProjectType = "node"
	ProjectTypePython  ProjectType = "python"
	ProjectTypeRust    ProjectType = "rust"
	ProjectTypeMake    ProjectType = "make"
	ProjectTypeUnknown ProjectType = "unknown"
)

// ProjectInfo holds detected project metadata used for build and test commands.
type ProjectInfo struct {
	Type     ProjectType
	BuildCmd string
	TestCmd  string
	RootDir  string
}

// DetectProject inspects the workspace directory to determine the project type
// and appropriate build/test commands. It checks for well-known project markers
// (go.mod, package.json, Cargo.toml, etc.) in priority order.
func DetectProject(workspace string) ProjectInfo {
	checks := []struct {
		detect   func(string) bool
		pType    ProjectType
		buildCmd string
		testCmd  string
	}{
		{hasGoMod, ProjectTypeGo, "go build ./...", "go test ./..."},
		{hasPackageJSON, ProjectTypeNode, "npm run build", "npm test"},
		{hasCargoToml, ProjectTypeRust, "cargo build", "cargo test"},
		{hasPyProjectToml, ProjectTypePython, "pip install -e .", "pytest"},
		{hasSetupPy, ProjectTypePython, "pip install -e .", "pytest"},
		{hasMakefile, ProjectTypeMake, "make", "make test"},
	}

	for _, c := range checks {
		if c.detect(workspace) {
			return ProjectInfo{
				Type:     c.pType,
				BuildCmd: c.buildCmd,
				TestCmd:  c.testCmd,
				RootDir:  workspace,
			}
		}
	}

	return ProjectInfo{Type: ProjectTypeUnknown, RootDir: workspace}
}

func hasGoMod(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, "go.mod"))
	return err == nil
}

func hasPackageJSON(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, "package.json"))
	return err == nil
}

func hasCargoToml(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, "Cargo.toml"))
	return err == nil
}

func hasPyProjectToml(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, "pyproject.toml"))
	return err == nil
}

func hasSetupPy(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, "setup.py"))
	return err == nil
}

func hasMakefile(dir string) bool {
	for _, name := range []string{"Makefile", "makefile", "GNUmakefile"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
			return true
		}
	}
	return false
}

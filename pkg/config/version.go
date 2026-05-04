package config

import (
        "fmt"
        "runtime"
)

// Build-time variables injected via ldflags during build process.
// These are set by the Makefile or .goreleaser.yaml using the -X flag:
//
//      -X github.com/sipeed/picoclaw/pkg/config.Version=<version>
//      -X github.com/sipeed/picoclaw/pkg/config.GitCommit=<commit>
//      -X github.com/sipeed/picoclaw/pkg/config.GitBranch=<branch>
//      -X github.com/sipeed/picoclaw/pkg/config.BuildTime=<timestamp>
//      -X github.com/sipeed/picoclaw/pkg/config.GoVersion=<go-version>
var (
        Version   = "dev" // Default value when not built with ldflags
        GitCommit string  // Git commit SHA (short)
        GitBranch string  // Git branch name
        BuildTime string  // Build timestamp in RFC3339 format
        GoVersion string  // Go version used for building
)

// FormatVersion returns the version string with optional git commit and branch.
// Output examples:
//
//      "1.2.3"                                — version only
//      "1.2.3 (git: abc123)"                  — version + commit
//      "1.2.3 (git: abc123, branch: main)"    — version + commit + branch
//      "1.2.3 (branch: main)"                 — version + branch (no commit)
func FormatVersion() string {
        v := Version
        var parts []string
        if GitCommit != "" {
                parts = append(parts, fmt.Sprintf("git: %s", GitCommit))
        }
        if GitBranch != "" {
                parts = append(parts, fmt.Sprintf("branch: %s", GitBranch))
        }
        if len(parts) > 0 {
                v += " (" + joinParts(parts) + ")"
        }
        return v
}

// joinParts joins version metadata parts with ", " separator.
func joinParts(parts []string) string {
        result := parts[0]
        for i := 1; i < len(parts); i++ {
                result += ", " + parts[i]
        }
        return result
}

// FormatBuildInfo returns build time and go version info
func FormatBuildInfo() (string, string) {
        build := BuildTime
        goVer := GoVersion
        if goVer == "" {
                goVer = runtime.Version()
        }
        return build, goVer
}

// GetVersion returns the version string
func GetVersion() string {
        return Version
}

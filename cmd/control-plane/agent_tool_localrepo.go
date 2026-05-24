package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
)

// Local git repository tools — gated on REPLAY_ALLOWED_REPO_PATHS. When that
// env var is empty the tools aren't even registered, so the model isn't tempted
// to call something that will only ever return an "access disabled" error.

// allowedRepoPaths is set from REPLAY_ALLOWED_REPO_PATHS in main before any
// requests are served. Empty means local-file access is disabled entirely.
var allowedRepoPaths []string

// isRepoPathAllowed reports whether path is under one of the configured
// allowed prefixes.
func isRepoPathAllowed(path string) bool {
	if len(allowedRepoPaths) == 0 {
		return false
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return false
	}
	for _, allowed := range allowedRepoPaths {
		// Require a separator after the prefix so /allowed-extra doesn't match /allowed.
		if strings.HasPrefix(abs+"/", allowed+"/") {
			return true
		}
	}
	return false
}

// localRepoToolDefs returns the read_local_file and list_local_directory tools.
// Caller appends these only when allowedRepoPaths is non-empty.
func localRepoToolDefs() []toolDef {
	return []toolDef{
		{
			Name:        "read_local_file",
			Description: "Read a file from a local git repository. Useful for investigating code when using a local test setup. Specify the repo path and file path within the repo.",
			Schema: map[string]any{
				"type":     "object",
				"required": []string{"repo_path", "file_path"},
				"properties": map[string]any{
					"repo_path": map[string]any{"type": "string", "description": "Filesystem path to the git repository root, e.g. /tmp/test-repo"},
					"file_path": map[string]any{"type": "string", "description": "Repo-relative path to file, e.g. src/component.ts"},
					"ref":       map[string]any{"type": "string", "description": "Git ref (branch/commit). Optional; defaults to HEAD."},
				},
			},
			Run: handleReadLocalFile,
		},
		{
			Name:        "list_local_directory",
			Description: "List the contents of a directory in a local git repository.",
			Schema: map[string]any{
				"type":     "object",
				"required": []string{"repo_path"},
				"properties": map[string]any{
					"repo_path": map[string]any{"type": "string", "description": "Filesystem path to the git repository root"},
					"dir_path":  map[string]any{"type": "string", "description": "Directory path within repo. Use empty string for root."},
					"ref":       map[string]any{"type": "string", "description": "Git ref. Optional; defaults to HEAD."},
				},
			},
			Run: handleListLocalDirectory,
		},
	}
}

func handleReadLocalFile(ctx context.Context, d *toolDeps, input json.RawMessage) (any, error) {
	var args struct {
		RepoPath string `json:"repo_path"`
		FilePath string `json:"file_path"`
		Ref      string `json:"ref"`
	}
	if err := json.Unmarshal(input, &args); err != nil {
		return nil, fmt.Errorf("bad input: %w", err)
	}
	if args.RepoPath == "" {
		return nil, fmt.Errorf("repo_path is required")
	}
	if args.FilePath == "" {
		return nil, fmt.Errorf("file_path is required")
	}
	if !isRepoPathAllowed(args.RepoPath) {
		if len(allowedRepoPaths) == 0 {
			return nil, fmt.Errorf("local file access is disabled: set REPLAY_ALLOWED_REPO_PATHS to enable")
		}
		return nil, fmt.Errorf("repo_path %q is not in the allowed list", args.RepoPath)
	}
	args.FilePath = strings.TrimPrefix(strings.TrimSpace(args.FilePath), "/")

	ref := args.Ref
	if ref == "" {
		ref = "HEAD"
	}

	// safe.directory handles Docker UID/GID mismatches when the repo is mounted in.
	out, err := execInRepo(ctx, args.RepoPath, "git", "-c", "safe.directory="+args.RepoPath, "show", fmt.Sprintf("%s:%s", ref, args.FilePath))
	if err != nil {
		return nil, fmt.Errorf("failed to read %s at %s: %w", args.FilePath, ref, err)
	}

	if len(out) > 200_000 {
		return nil, fmt.Errorf("file is too large (%d bytes); cap is 200KB", len(out))
	}

	return map[string]any{
		"repo_path": args.RepoPath,
		"file_path": args.FilePath,
		"ref":       ref,
		"content":   string(out),
		"size":      len(out),
	}, nil
}

func handleListLocalDirectory(ctx context.Context, d *toolDeps, input json.RawMessage) (any, error) {
	var args struct {
		RepoPath string `json:"repo_path"`
		DirPath  string `json:"dir_path"`
		Ref      string `json:"ref"`
	}
	if err := json.Unmarshal(input, &args); err != nil {
		return nil, fmt.Errorf("bad input: %w", err)
	}
	if args.RepoPath == "" {
		return nil, fmt.Errorf("repo_path is required")
	}
	if !isRepoPathAllowed(args.RepoPath) {
		if len(allowedRepoPaths) == 0 {
			return nil, fmt.Errorf("local file access is disabled: set REPLAY_ALLOWED_REPO_PATHS to enable")
		}
		return nil, fmt.Errorf("repo_path %q is not in the allowed list", args.RepoPath)
	}

	dirPath := strings.TrimPrefix(strings.TrimSpace(args.DirPath), "/")
	ref := args.Ref
	if ref == "" {
		ref = "HEAD"
	}

	treeArg := ref
	if dirPath != "" {
		treeArg = ref + ":" + dirPath
	}

	out, err := execInRepo(ctx, args.RepoPath, "git", "-c", "safe.directory="+args.RepoPath, "ls-tree", "-l", treeArg)
	if err != nil {
		return nil, fmt.Errorf("failed to list %s at %s: %w", dirPath, ref, err)
	}

	// Parse ls-tree -l output: mode type hash size path
	entries := []map[string]any{}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line == "" {
			continue
		}
		parts := strings.Fields(line)
		if len(parts) < 5 {
			continue
		}
		mode, kind, sha, size, name := parts[0], parts[1], parts[2], parts[3], parts[4]

		typeStr := "file"
		if kind == "tree" {
			typeStr = "dir"
		}

		sizeNum := 0
		if size != "-" {
			fmt.Sscanf(size, "%d", &sizeNum)
		}

		entries = append(entries, map[string]any{
			"name": name,
			"type": typeStr,
			"mode": mode,
			"size": sizeNum,
			"sha":  sha,
		})
	}

	return map[string]any{
		"repo_path": args.RepoPath,
		"dir_path":  dirPath,
		"ref":       ref,
		"entries":   entries,
	}, nil
}

// execInRepo runs a command with cwd set to the repo path.
func execInRepo(ctx context.Context, repoPath string, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = repoPath
	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("%s: %w", strings.TrimSpace(string(out)), err)
	}
	return out, nil
}

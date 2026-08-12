package benchmark

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadLocalConfigResolvesPathsFromItsDirectory(t *testing.T) {
	root := t.TempDir()
	configurationPath := filepath.Join(root, "v0", "local.yaml")
	if err := os.MkdirAll(filepath.Dir(configurationPath), 0o700); err != nil {
		t.Fatal(err)
	}
	contents := `version: 1
paths:
  manifest: ./tasks.yaml
  agent_config: ./private/agent.yaml
  private_dir: ./private
  results_dir: ./results
  workspaces_dir: ./workspaces
  state_dir: ./state
execution:
  prepare_timeout: 10m
  prepare_proxy: http://127.0.0.1:7890
`
	if err := os.WriteFile(configurationPath, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	configuration, err := LoadLocalConfig(configurationPath)
	if err != nil {
		t.Fatal(err)
	}
	for name, actual := range map[string]string{
		"manifest":       configuration.Paths.Manifest,
		"agent_config":   configuration.Paths.AgentConfig,
		"private_dir":    configuration.Paths.PrivateDir,
		"results_dir":    configuration.Paths.ResultsDir,
		"workspaces_dir": configuration.Paths.WorkspacesDir,
		"state_dir":      configuration.Paths.StateDir,
	} {
		if !strings.HasPrefix(actual, filepath.Dir(configurationPath)+string(filepath.Separator)) {
			t.Fatalf("%s = %q is not relative to local config", name, actual)
		}
	}
	if configuration.Execution.PrepareProxy != "http://127.0.0.1:7890" {
		t.Fatalf("proxy=%q", configuration.Execution.PrepareProxy)
	}
}

func TestLoadLocalConfigRejectsUnknownAndIncompleteFields(t *testing.T) {
	for name, contents := range map[string]string{
		"unknown": `version: 1
paths: {}
execution:
  prepare_timeout: 10m
unexpected: value
`,
		"missing": `version: 1
paths:
  manifest: ./tasks.yaml
execution:
  prepare_timeout: 10m
`,
		"invalid duration": `version: 1
paths:
  manifest: ./tasks.yaml
  agent_config: ./agent.yaml
  private_dir: ./private
  results_dir: ./results
  workspaces_dir: ./workspaces
  state_dir: ./state
execution:
  prepare_timeout: tomorrow
`,
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "local.yaml")
			if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := LoadLocalConfig(path); err == nil {
				t.Fatal("expected local config validation error")
			}
		})
	}
}

func TestEnsureLocalAssetsIgnoredRejectsTrackedAndUnignoredAssets(t *testing.T) {
	root := t.TempDir()
	runGit(t, root, "init")
	if err := os.WriteFile(filepath.Join(root, ".gitignore"), []byte("/local/\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ignoredAsset := filepath.Join(root, "local", "tasks.yaml")
	if err := os.MkdirAll(filepath.Dir(ignoredAsset), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(ignoredAsset, []byte("local"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := EnsureLocalAssetsIgnored(root, ignoredAsset); err != nil {
		t.Fatalf("ignored asset rejected: %v", err)
	}
	trackedAsset := filepath.Join(root, "tracked.yaml")
	if err := os.WriteFile(trackedAsset, []byte("tracked"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(t, root, "add", "tracked.yaml")
	if err := EnsureLocalAssetsIgnored(root, trackedAsset); err == nil || !strings.Contains(err.Error(), "must not be Git-tracked") {
		t.Fatalf("tracked asset error=%v", err)
	}
	unignoredAsset := filepath.Join(root, "unignored.yaml")
	if err := os.WriteFile(unignoredAsset, []byte("unignored"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := EnsureLocalAssetsIgnored(root, unignoredAsset); err == nil || !strings.Contains(err.Error(), "must be covered by .gitignore") {
		t.Fatalf("unignored asset error=%v", err)
	}
}

func TestEnsureLocalAssetsIgnoredAllowsIgnoredDirectoriesNotCreatedYet(t *testing.T) {
	root := t.TempDir()
	runGit(t, root, "init")
	if err := os.WriteFile(filepath.Join(root, ".gitignore"), []byte("/local/\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := EnsureLocalAssetsIgnored(root, filepath.Join(root, "local", "results"), filepath.Join(root, "local", "workspaces")); err != nil {
		t.Fatalf("future ignored output roots rejected: %v", err)
	}
}

func TestEnsureLocalAssetsIgnoredResolvesSymlinksAndProtectsRepositoryRoot(t *testing.T) {
	root := t.TempDir()
	runGit(t, root, "init")
	if err := os.WriteFile(filepath.Join(root, ".gitignore"), []byte("/local/\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := EnsureLocalAssetsIgnored(root, root); err == nil || !strings.Contains(err.Error(), "must not be the Cairn repository root") {
		t.Fatalf("repository root error=%v", err)
	}
	trackedDirectory := filepath.Join(root, "tracked")
	if err := os.MkdirAll(trackedDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "local"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(trackedDirectory, filepath.Join(root, "local", "escape")); err != nil {
		t.Fatal(err)
	}
	if err := EnsureLocalAssetsIgnored(root, filepath.Join(root, "local", "escape", "results")); err == nil || !strings.Contains(err.Error(), "must be covered by .gitignore") {
		t.Fatalf("symlink escape error=%v", err)
	}
}

func TestEnsureLocalAssetsIgnoredUsesGitWorktreeTopLevelForSubdirectoriesAndSymlinks(t *testing.T) {
	root := t.TempDir()
	runGit(t, root, "init")
	if err := os.WriteFile(filepath.Join(root, ".gitignore"), []byte("/local/\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "local", "nested"), 0o700); err != nil {
		t.Fatal(err)
	}
	unignoredAsset := filepath.Join(root, "unignored.yaml")
	if err := os.WriteFile(unignoredAsset, []byte("unignored"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := EnsureLocalAssetsIgnored(filepath.Join(root, "local", "nested"), unignoredAsset); err == nil || !strings.Contains(err.Error(), "must be covered by .gitignore") {
		t.Fatalf("subdirectory root error=%v", err)
	}
	link := filepath.Join(t.TempDir(), "cairn-link")
	if err := os.Symlink(root, link); err != nil {
		t.Fatal(err)
	}
	if err := EnsureLocalAssetsIgnored(link, filepath.Join(link, "unignored.yaml")); err == nil || !strings.Contains(err.Error(), "must be covered by .gitignore") {
		t.Fatalf("symlink root error=%v", err)
	}
}

func TestIgnoredByRepositoryGitignoreRejectsNonRepositorySources(t *testing.T) {
	root := t.TempDir()
	for name, output := range map[string]string{
		"missing NUL fields":        ".gitignore\x001\x00/local/",
		"git info exclude":          filepath.Join(root, ".git", "info", "exclude") + "\x001\x00/local/\x00local/tasks.yaml\x00",
		"global ignore":             "/tmp/global-ignore\x001\x00/local/\x00local/tasks.yaml\x00",
		"non gitignore filename":    ".git/exclude\x001\x00/local/\x00local/tasks.yaml\x00",
		"unexpected matching asset": ".gitignore\x001\x00/local/\x00other/tasks.yaml\x00",
	} {
		t.Run(name, func(t *testing.T) {
			if ignoredByRepositoryGitignore(root, "local/tasks.yaml", output) {
				t.Fatalf("unexpected accepted source %q", output)
			}
		})
	}
	if !ignoredByRepositoryGitignore(root, "local/tasks.yaml", ".gitignore\x001\x00/local/\x00local/tasks.yaml\x00") {
		t.Fatal("expected root .gitignore source to be accepted")
	}
}

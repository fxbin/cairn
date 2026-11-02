package tools

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

func TestObservationDigestStableAndIncludesGitIgnoredWrites(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "generated"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".gitignore"), []byte("generated/\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "generated", "a.txt"), []byte("one"), 0o600); err != nil {
		t.Fatal(err)
	}
	workspace, err := NewWorkspace(root, nil, 0, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	first, err := workspace.Observation()
	if err != nil {
		t.Fatal(err)
	}
	second, err := workspace.Observation()
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatalf("observation not stable: %s != %s", first, second)
	}
	// The previous sequential digest is SHA256(sorted(type, mode, path, file hash)).
	var entries []string
	err = filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relative, err := filepath.Rel(root, path)
		if err != nil || relative == "." {
			return err
		}
		if workspace.Ignore(relative) {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		mode := info.Mode().Perm()
		relative = filepath.ToSlash(relative)
		if entry.IsDir() {
			entries = append(entries, fmt.Sprintf("d\t%04o\t%s", mode, relative))
			return nil
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		digest := sha256.Sum256(content)
		entries = append(entries, fmt.Sprintf("f\t%04o\t%s\t%s", mode, relative, hex.EncodeToString(digest[:])))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(entries)
	baseline := sha256.Sum256([]byte(strings.Join(entries, "\n")))
	if first != "sha256:"+hex.EncodeToString(baseline[:]) {
		t.Fatalf("parallel digest changed the existing observation contract")
	}
	if err := os.WriteFile(filepath.Join(root, "generated", "a.txt"), []byte("two"), 0o600); err != nil {
		t.Fatal(err)
	}
	changed, err := workspace.Observation()
	if err != nil {
		t.Fatal(err)
	}
	if changed == first {
		t.Fatal(".gitignore must not hide observed side effects")
	}
}

func TestObservationParallelManyFiles(t *testing.T) {
	root := t.TempDir()
	for i := 0; i < 120; i++ {
		if err := os.WriteFile(filepath.Join(root, fmt.Sprintf("%04d.txt", i)), []byte("value"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	ws, err := NewWorkspace(root, nil, 0, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	one, err := ws.Observation()
	if err != nil {
		t.Fatal(err)
	}
	two, err := ws.Observation()
	if err != nil {
		t.Fatal(err)
	}
	if one != two {
		t.Fatal("parallel ordering must not change snapshot fingerprint")
	}
}

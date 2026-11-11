package desktop

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// TestListFileTreeBreadthFirst ensures that a generated directory cannot
// exhaust the node budget before root-level siblings are shown.
func TestListFileTreeBreadthFirst(t *testing.T) {
	root := t.TempDir()
	dense := filepath.Join(root, "a-generated")
	if err := os.Mkdir(dense, 0o755); err != nil {
		t.Fatal(err)
	}
	for index := 0; index < maxTreeEntries+8; index++ {
		path := filepath.Join(dense, fmt.Sprintf("f-%04d.txt", index))
		if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "z-readme.md"), []byte("README"), 0o600); err != nil {
		t.Fatal(err)
	}
	tree, err := ListFileTree(root)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, child := range tree.Children {
		if child.Name == "z-readme.md" {
			found = true
		}
	}
	if !found {
		t.Fatalf("root-level files were lost after a dense directory: %+v", tree.Children)
	}
	if len(tree.Children) < 2 {
		t.Fatalf("missing root siblings: %+v", tree.Children)
	}
}

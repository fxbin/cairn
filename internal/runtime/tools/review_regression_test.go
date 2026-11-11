package tools

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestReviewPackageCannotPublishOutsideStore(t *testing.T) {
	parent := t.TempDir()
	source := writePackageSource(t, "../outside", "1", map[string]string{"note.md": "content"})
	store, err := NewPackageStore(filepath.Join(parent, "store"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.InstallPackage(source); err == nil {
		t.Fatal("escaping package name was installed")
	}
	if _, err := os.Stat(filepath.Join(parent, "outside")); !os.IsNotExist(err) {
		t.Fatal("package escaped store")
	}
}

func TestReviewPackageRejectsUnknownExecutableDeclarations(t *testing.T) {
	source := writePackageSource(t, "safe", "1", map[string]string{"note.md": "content"})
	path := filepath.Join(source, PackageManifestFile)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(raw, []byte("hooks:\n  install: ignored-command\n")...), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := InspectPackage(source); err == nil {
		t.Fatal("unknown hooks declaration was silently accepted")
	}
}

func TestReviewPackageEnforcesAggregateLimit(t *testing.T) {
	files := map[string]string{}
	for i := 0; i < PackageMaxTotalBytes/PackageMaxFileBytes+1; i++ {
		files[fmt.Sprintf("file-%d.md", i)] = strings.Repeat("x", PackageMaxFileBytes)
	}
	source := writePackageSource(t, "large", "1", files)
	if _, err := InspectPackage(source); err == nil {
		t.Fatal("oversize aggregate package was accepted")
	}
}

func TestReviewPackageDisableDoesNotFollowMarkerSymlink(t *testing.T) {
	source := writePackageSource(t, "safe", "1", map[string]string{"note.md": "content"})
	store, err := NewPackageStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.InstallPackage(source); err != nil {
		t.Fatal(err)
	}
	sentinel := filepath.Join(t.TempDir(), "sentinel")
	if err := os.WriteFile(sentinel, []byte("unchanged"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(sentinel, filepath.Join(store.Root, "safe", packageDisabledFile)); err != nil {
		t.Fatal(err)
	}
	if err := store.SetPackageEnabled("safe", false); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(sentinel)
	if err != nil || string(raw) != "unchanged" {
		t.Fatalf("marker followed symlink: %q %v", raw, err)
	}
}

func TestReviewTemplateReadRejectsEscapingSymlink(t *testing.T) {
	store, err := NewTemplateStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "outside.md")
	if err := os.WriteFile(outside, []byte("outside text"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(store.Root, "alias.md")); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Load("alias"); err == nil {
		t.Fatal("template followed outside symlink")
	}
	catalog, err := store.List()
	if err != nil || len(catalog.Entries) != 0 || len(catalog.Diagnostics) != 1 {
		t.Fatalf("unsafe template listed: %+v %v", catalog, err)
	}
}

func TestReviewTemplateConcurrentSaveNeverReplacesWinner(t *testing.T) {
	store, err := NewTemplateStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	results := make(chan string, 16)
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			body := fmt.Sprintf("writer-%d", i)
			if store.Save(TemplateDocument{Name: "shared", Body: body}) == nil {
				results <- body
			}
		}(i)
	}
	close(start)
	wg.Wait()
	close(results)
	var winners []string
	for body := range results {
		winners = append(winners, body)
	}
	if len(winners) != 1 {
		t.Fatalf("expected one publisher, got %d", len(winners))
	}
	loaded, err := store.Load("shared")
	if err != nil || loaded.Body != winners[0] {
		t.Fatalf("winner replaced: %+v %v", loaded, err)
	}
}

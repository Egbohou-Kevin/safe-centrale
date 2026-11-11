package qualitytest

import (
	"io/fs"
	"maps"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// The brand is SeQ (Kevin, 31/10/2026 03:23: its icon in the reports, the architecture,
// everywhere): every document opens with it. See docs/marque/README.md.
var brandRule = rule{name: "brand", fix: `start the document with <img src="<path to docs/marque/seq-icone.svg>" width="56" alt="SeQ">`}

const brandIcon = "docs/marque/seq-icone.svg"

// markdownOf reads the module's documents: path -> text. CLAUDE.md is Claude's, not a document.
func markdownOf(t *testing.T, root string) map[string]string {
	t.Helper()
	docs := map[string]string{}
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if p != root && skipDir(d.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(p, ".md") || d.Name() == "CLAUDE.md" {
			return nil
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		src, err := os.ReadFile(p)
		docs[filepath.ToSlash(rel)] = string(src)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return docs
}

// unbranded returns the documents whose first line is not SeQ's icon, by its path from them.
func unbranded(docs map[string]string) []finding {
	var found []finding
	for _, rel := range slices.Sorted(maps.Keys(docs)) {
		icon, err := filepath.Rel(path.Dir(rel), brandIcon)
		if err != nil {
			icon = brandIcon
		}
		icon = filepath.ToSlash(icon)
		first, _, _ := strings.Cut(docs[rel], "\n")
		if !strings.HasPrefix(first, `<img src="`+icon+`"`) {
			found = append(found, finding{key: rel, msg: rel + " does not open with SeQ's icon (" + icon + ")"})
		}
	}
	return found
}

func TestEveryDocumentOpensWithSeQsIcon(t *testing.T) {
	root := moduleRoot(t)
	if _, err := os.Stat(filepath.Join(root, brandIcon)); err != nil {
		t.Fatal(err)
	}
	brandRule.enforce(t, unbranded(markdownOf(t, root)))
}

func TestBrandRuleCatchesViolations(t *testing.T) {
	docs := map[string]string{
		"README.md":           `<img src="docs/marque/seq-icone.svg" width="56" alt="SeQ">` + "\n\n# x\n",
		"docs/a.md":           `<img src="marque/seq-icone.svg" width="56" alt="SeQ">` + "\n",
		"docs/marque/m.md":    `<img src="seq-icone.svg" width="56" alt="SeQ">` + "\n",
		"docs/deep/b.md":      `<img src="../marque/seq-icone.svg" width="56" alt="SeQ">` + "\n",
		"kit.md":              "# no icon\n",
		"docs/wrong.md":       `<img src="docs/marque/seq-icone.svg" width="56" alt="SeQ">` + "\n",
		"docs/deep/second.md": "# x\n" + `<img src="../marque/seq-icone.svg">` + "\n",
	}
	got := keys(unbranded(docs))
	if !slices.Equal(got, []string{"docs/deep/second.md", "docs/wrong.md", "kit.md"}) {
		t.Errorf("brand rule: got %v, want [docs/deep/second.md docs/wrong.md kit.md]", got)
	}
}

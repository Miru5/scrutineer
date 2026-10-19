package skills

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func TestBundledFileParserAuditMetadata(t *testing.T) {
	dir := filepath.Join("..", "..", "skills", "audit-file-parser")
	skill, err := ParseFile(filepath.Join(dir, "SKILL.md"))
	if err != nil {
		t.Fatal(err)
	}
	if skill.Name != "audit-file-parser" || skill.OutputKind != "findings" || skill.Model != "high" || skill.MaxTurns != 48 || skill.MinConfidence != "high" || skill.SchemaJSON == "" {
		t.Fatalf("invalid audit metadata: %+v", skill)
	}
	// Parsers keep real source under directories named build, out, dist or
	// target, such as Go's src/cmd/dist and Rust crates' src/target modules,
	// so only root-level dist and target output is excluded.
	for _, path := range []string{"tlvdoc/reader.py", "src/decode.c", "lib/xml/parser.go", "internal/build/x.go", "cmd/out/main.go", "src/cmd/dist/x.go", "app/src/target/y.rs"} {
		if !PathIncluded(path, skill.Paths, skill.IgnorePaths) {
			t.Errorf("excluded %s", path)
		}
	}
	for _, path := range []string{"node_modules/lib/index.js", "vendor/dep/dep.go", "third_party/lib/a.c", "dist/parser.js", "target/debug/parser", ".cache/entry", "generated/schema.go", "parser.min.js"} {
		if PathIncluded(path, skill.Paths, skill.IgnorePaths) {
			t.Errorf("included %s", path)
		}
	}
	ref, err := os.ReadFile(filepath.Join(dir, "references", "threat-model.md"))
	if err != nil {
		t.Fatal(err)
	}
	// Findings persist these links as references, so each must name a
	// versioned CWE definition page and nothing else on the CWE site.
	definition := regexp.MustCompile(`^https://cwe\.mitre\.org/data/definitions/[0-9]+\.html$`)
	links := regexp.MustCompile(`https?://cwe\.mitre\.org[^\s)\]]*`).FindAllString(string(ref), -1)
	if len(links) == 0 {
		t.Error("reference must link CWE definitions")
	}
	for _, link := range links {
		if !definition.MatchString(link) {
			t.Errorf("CWE link %q is not a definition page link", link)
		}
	}
	for _, section := range []string{"Reference and limits", "Actors and assets", "Resource exhaustion and amplification", "Length, offset and integer handling", "Archive and container extraction", "Format confusion and differential parsing", "External references and embedded content", "Malformed input and failure handling", "Evidence discipline"} {
		if !strings.Contains(string(ref), "## "+section) {
			t.Errorf("reference missing %q", section)
		}
	}
}

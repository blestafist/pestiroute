package core

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

type packageImports struct {
	ImportPath   string
	Dir          string
	GoFiles      []string
	CgoFiles     []string
	Imports      []string
	TestImports  []string
	XTestImports []string
}

const coreImportPath = "github.com/blestafist/pestiroute/internal/core"

func runGoList(t *testing.T, args ...string) []byte {
	t.Helper()
	cmd := exec.Command("go", append([]string{"list"}, args...)...)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("go list %v: %v", args, err)
	}
	return out
}

func standardPackages(t *testing.T) map[string]bool {
	t.Helper()
	lines := strings.Fields(string(runGoList(t, "-f", "{{.ImportPath}}", "std")))
	standard := make(map[string]bool, len(lines))
	for _, line := range lines {
		standard[line] = true
	}
	return standard
}

func checkProductionImports(imports []string, standard map[string]bool) []string {
	var illegal []string
	for _, path := range imports {
		if !standard[path] {
			illegal = append(illegal, path)
		}
	}
	return illegal
}

func readPackageList(data []byte) ([]packageImports, error) {
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	var packages []packageImports
	for {
		var pkg packageImports
		if err := decoder.Decode(&pkg); err != nil {
			if err == io.EOF {
				return packages, nil
			}
			return nil, err
		}
		packages = append(packages, pkg)
	}
}

func corePackageImportViolations(packages []packageImports, standard map[string]bool) []string {
	var violations []string
	for _, pkg := range packages {
		if pkg.ImportPath != coreImportPath && !strings.HasPrefix(pkg.ImportPath, coreImportPath+"/") {
			continue
		}
		for _, path := range checkProductionImports(pkg.Imports, standard) {
			violations = append(violations, pkg.ImportPath+" imports non-standard package "+path)
		}
	}
	return violations
}

func coreGoPackages(t *testing.T) []packageImports {
	t.Helper()
	packages, err := readPackageList(runGoList(t, "-json", "./..."))
	if err != nil {
		t.Fatalf("decode go list packages: %v", err)
	}
	if len(packages) == 0 {
		t.Fatal("go list ./... returned no packages")
	}
	for _, pkg := range packages {
		if pkg.ImportPath != coreImportPath && !strings.HasPrefix(pkg.ImportPath, coreImportPath+"/") {
			t.Fatalf("go list ./... escaped internal/core scope: %s", pkg.ImportPath)
		}
	}
	return packages
}

func TestCoreProductionDependencies(t *testing.T) {
	packages := coreGoPackages(t)
	violations := corePackageImportViolations(packages, standardPackages(t))
	if len(violations) != 0 {
		t.Errorf("production Core dependencies: %s", strings.Join(violations, "; "))
	}
}

func TestCoreDependencyGuardRejectsForbiddenFixture(t *testing.T) {
	fixture := []string{
		"fmt",
		"github.com/blestafist/pestiroute/internal/adapter/responses",
		"github.com/blestafist/pestiroute/internal/connector/responses",
		"github.com/blestafist/pestiroute/internal/testutil/scripted",
		"example.com/protocol/parser",
	}
	got := corePackageImportViolations([]packageImports{{
		ImportPath: coreImportPath + "/nested/fixture",
		Imports:    fixture,
	}}, map[string]bool{"fmt": true})
	if len(got) != len(fixture)-1 {
		t.Fatalf("nested illegal imports not explicitly rejected: got %v", got)
	}
	for _, path := range fixture[1:] {
		if !strings.Contains(strings.Join(got, "\n"), path) {
			t.Errorf("nested illegal import %s not reported: %v", path, got)
		}
	}
}

func TestCoreHasNoProviderSwitchesOrUniversalPayloadTypes(t *testing.T) {
	for _, pkg := range coreGoPackages(t) {
		for _, name := range append(pkg.GoFiles, pkg.CgoFiles...) {
			path := filepath.Join(pkg.Dir, name)
			source, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read %s: %v", path, err)
			}
			violations, err := sourceGuardViolations(path, source)
			if err != nil {
				t.Fatalf("parse %s: %v", path, err)
			}
			for _, violation := range violations {
				t.Error(violation)
			}
		}
	}
}

func sourceGuardViolations(filename string, source []byte) ([]string, error) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, filename, source, 0)
	if err != nil {
		return nil, err
	}
	var violations []string
	add := func(node ast.Node, message string) {
		violations = append(violations, fset.Position(node.Pos()).String()+": "+message)
	}
	ast.Inspect(file, func(node ast.Node) bool {
		switch n := node.(type) {
		case *ast.IfStmt:
			if containsProviderName(n.Cond) {
				add(n, "provider-specific conditional")
			}
		case *ast.CaseClause:
			for _, expr := range n.List {
				if containsProviderName(expr) {
					add(n, "provider-specific switch case")
					break
				}
			}
		case *ast.SwitchStmt:
			if isProviderTag(n.Tag) {
				add(n, "provider-name switch tag")
			}
		case *ast.TypeSwitchStmt:
			if isProviderTag(n.Assign) {
				add(n, "provider-name type-switch tag")
			}
		case *ast.TypeSpec:
			if n.Name.Name == "Message" || n.Name.Name == "Tool" || n.Name.Name == "Reasoning" {
				add(n, "universal LLM payload type "+n.Name.Name)
			}
		}
		return true
	})
	return violations, nil
}

func isProviderTag(node ast.Node) bool {
	found := false
	ast.Inspect(node, func(node ast.Node) bool {
		if ident, ok := node.(*ast.Ident); ok && strings.EqualFold(ident.Name, "provider") {
			found = true
		}
		return !found
	})
	return found
}

func containsProviderName(node ast.Node) bool {
	providers := map[string]bool{"openai": true, "anthropic": true, "gemini": true, "ollama": true}
	found := false
	ast.Inspect(node, func(node ast.Node) bool {
		switch n := node.(type) {
		case *ast.BasicLit:
			if n.Kind == token.STRING {
				value, err := strconv.Unquote(n.Value)
				if err == nil && providerName(value, providers) {
					found = true
				}
			}
		case *ast.Ident:
			if providers[strings.ToLower(n.Name)] {
				found = true
			}
		}
		return !found
	})
	return found
}

func providerName(value string, providers map[string]bool) bool {
	value = strings.ToLower(value)
	if providers[value] {
		return true
	}
	for provider := range providers {
		if strings.HasPrefix(value, provider+".") {
			return true
		}
	}
	return false
}

func TestCoreSourceGuardFixtures(t *testing.T) {
	for name, fixture := range map[string]struct{ source, expected string }{
		"provider if":          {`package core; func f(provider string) { if provider == "openai" {} }`, "provider-specific conditional"},
		"provider switch case": {`package core; func f(provider string) { switch provider { case "anthropic": } }`, "provider-specific switch case"},
		"provider switch tag":  {`package core; func f(provider string) { switch provider {} }`, "provider-name switch tag"},
		"message type":         {`package core; type Message struct{}`, "universal LLM payload type Message"},
		"tool type":            {`package core; type Tool struct{}`, "universal LLM payload type Tool"},
		"reasoning type":       {`package core; type Reasoning struct{}`, "universal LLM payload type Reasoning"},
	} {
		t.Run(name, func(t *testing.T) {
			violations, err := sourceGuardViolations(name+".go", []byte(fixture.source))
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(strings.Join(violations, "\n"), fixture.expected) {
				t.Fatalf("guard did not report %q: %v", fixture.expected, violations)
			}
		})
	}
	for name, source := range map[string]string{
		"word substrings":         `package core; const text = "anthropology openai-compatible"; var openaiClient int`,
		"opaque protocol literal": `package core; const protocol = "openai.responses.v1"`,
	} {
		t.Run(name, func(t *testing.T) {
			violations, err := sourceGuardViolations(name+".go", []byte(source))
			if err != nil {
				t.Fatal(err)
			}
			if len(violations) != 0 {
				t.Fatalf("guard rejected valid source: %v", violations)
			}
		})
	}
}

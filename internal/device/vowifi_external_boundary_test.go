package device

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestVoWiFiHostImportsExternalRuntimehostOnly(t *testing.T) {
	repoRoot := filepath.Clean(filepath.Join("..", ".."))
	scanRoots := []string{"cmd", "internal"}
	allowedVoWiFiImports := map[string]bool{
		"github.com/1239t/vowifi-go/runtimehost":             true,
		"github.com/1239t/vowifi-go/runtimehost/carrier":     true,
		"github.com/1239t/vowifi-go/runtimehost/e911":        true,
		"github.com/1239t/vowifi-go/runtimehost/eventhost":   true,
		"github.com/1239t/vowifi-go/runtimehost/identity":    true,
		"github.com/1239t/vowifi-go/runtimehost/messaging":   true,
		"github.com/1239t/vowifi-go/runtimehost/simauth":     true,
		"github.com/1239t/vowifi-go/runtimehost/voiceclient": true,
		"github.com/1239t/vowifi-go/runtimehost/voicehost":   true,
	}
	var offenders []string
	fset := token.NewFileSet()

	for _, root := range scanRoots {
		base := filepath.Join(repoRoot, root)
		err := filepath.WalkDir(base, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				return nil
			}
			if filepath.Ext(path) != ".go" || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			file, parseErr := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
			if parseErr != nil {
				return parseErr
			}
			rel, _ := filepath.Rel(repoRoot, path)
			for _, spec := range file.Imports {
				importPath := strings.Trim(spec.Path.Value, `"`)
				if strings.HasPrefix(importPath, "github.com/iniwex5/"+"vowifi-go/engine/") {
					continue
				}
				if strings.HasPrefix(importPath, "github.com/iniwex5/"+"vowifi-go/") && !allowedVoWiFiImports[importPath] {
					offenders = append(offenders, rel+": imports non-public VoWiFi package "+importPath)
				}
				if importPath == "github.com/iniwex5/"+"vohive/internal/vowifi" ||
					strings.HasPrefix(importPath, "github.com/iniwex5/"+"vohive/internal/vowifi/") {
					offenders = append(offenders, rel+": imports old internal VoWiFi")
				}
			}
			return nil
		})
		if err != nil {
			t.Fatalf("scan %s: %v", root, err)
		}
	}

	if len(offenders) > 0 {
		t.Fatalf("VoHive must import only approved vowifi-go runtimehost public packages:\n%s", strings.Join(offenders, "\n"))
	}
}

func TestVoWiFiHostRuntimeFileStaysThin(t *testing.T) {
	body, err := os.ReadFile(filepath.Join("..", "..", "internal", "device", "pool_vowifi_runtime.go"))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Count(string(body), "\n") + 1
	if lines > 650 {
		t.Fatalf("pool_vowifi_runtime.go has %d lines, want <= 650 after host split", lines)
	}
}

func TestVoWiFiStartOrchestrationIsExtracted(t *testing.T) {
	repoRoot := filepath.Clean(filepath.Join("..", ".."))
	body, err := os.ReadFile(filepath.Join(repoRoot, "internal", "device", "vowifi_start_orchestrator.go"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "prepareVoWiFiStartContext") {
		t.Fatal("vowifi_start_orchestrator.go must own prepareVoWiFiStartContext")
	}

	runtimeBody, err := os.ReadFile(filepath.Join(repoRoot, "internal", "device", "pool_vowifi_runtime.go"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(runtimeBody), "RefreshIdentityLive") || strings.Contains(string(runtimeBody), "PrepareStart(runtimehost.PrepareStartInput") {
		t.Fatal("pool_vowifi_runtime.go still owns pre-start orchestration")
	}
}

package main

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"salcara/bridge/internal/desktopcompanion"
)

func runtimeFixtureFile(t *testing.T, file, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(file), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte(content), 0700); err != nil {
		t.Fatal(err)
	}
}

func runtimeFixtureBundle(t *testing.T, directory string) {
	t.Helper()
	for name, contents := range map[string]string{
		"package.json":            `{"version":"` + desktopcompanion.Version + `"}`,
		"LICENSE":                 "Synthetic test fixture",
		"src/index.mjs":           "// Never executed by this test",
		"src/permission-hook.mjs": "// Never executed by this test",
	} {
		runtimeFixtureFile(t, filepath.Join(directory, filepath.FromSlash(name)), contents)
	}
}

func runtimeFixtureNodeName() string {
	if runtime.GOOS == "windows" {
		return "SalcaraProbeNode.exe"
	}
	return "SalcaraProbeNode"
}

func TestPackagedRuntimePathsUseCoreResourcesAfterRelocation(t *testing.T) {
	for _, layout := range []struct{ name, core, desktop string }{
		{"windows-portable", "resources/SalcaraBridge.exe", "Salcara Bridge.exe"},
		{"linux-portable", "resources/SalcaraBridge", "Salcara Bridge"},
		{"mac-app", "Salcara Bridge.app/Contents/Resources/SalcaraBridge", "Salcara Bridge.app/Contents/MacOS/Salcara Bridge"},
	} {
		t.Run(layout.name, func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "moved app with spaces")
			core := filepath.Join(root, filepath.FromSlash(layout.core))
			desktop := filepath.Join(root, filepath.FromSlash(layout.desktop))
			node := filepath.Join(filepath.Dir(core), runtimeFixtureNodeName())
			bundle := filepath.Join(filepath.Dir(core), "desktop-companion")
			for _, executable := range []string{core, desktop, node} {
				runtimeFixtureFile(t, executable, "Synthetic executable: never launched")
			}
			runtimeFixtureBundle(t, bundle)
			p := resolveRuntimePaths(core, desktop, "", true)
			if p.coreExe != core || p.startupExe != desktop || p.companionDir != bundle || p.nodeExe != node {
				t.Fatalf("packaged resource and launcher paths were mixed: %+v", p)
			}
			data, codex := filepath.Join(root, "isolated-data"), filepath.Join(root, "isolated-codex")
			service := desktopcompanion.New(desktopcompanion.Options{
				BundleDir: p.companionDir, NodePath: p.nodeExe, DataDir: data, CodexHome: codex,
			})
			status, err := service.Preview(context.Background())
			if err != nil || !status.Available || status.Installed {
				t.Fatalf("complete packaged resources must be available without external installation: %+v, %v", status, err)
			}
			for _, directory := range []string{data, codex} {
				if _, err := os.Stat(directory); !os.IsNotExist(err) {
					t.Fatalf("readiness must not create user or installation state: %s, %v", directory, err)
				}
			}
		})
	}
}

func TestPackagedRuntimeMissingNodeDoesNotUseSystemNode(t *testing.T) {
	root := t.TempDir()
	core := filepath.Join(root, "resources", "SalcaraBridge.exe")
	desktop := filepath.Join(root, "Salcara Bridge.exe")
	systemDir := filepath.Join(root, "system-path")
	systemNode := filepath.Join(systemDir, "node")
	if runtime.GOOS == "windows" {
		systemNode += ".exe"
	}
	for _, executable := range []string{core, desktop, systemNode} {
		runtimeFixtureFile(t, executable, "Synthetic executable: never launched")
	}
	t.Setenv("PATH", systemDir)
	bundle := filepath.Join(filepath.Dir(core), "desktop-companion")
	runtimeFixtureBundle(t, bundle)
	p := resolveRuntimePaths(core, desktop, "", true)
	want := filepath.Join(filepath.Dir(core), runtimeFixtureNodeName())
	if p.nodeExe != want || p.nodeExe == systemNode {
		t.Fatalf("missing packaged runtime was hidden by system Node: %+v", p)
	}
	service := desktopcompanion.New(desktopcompanion.Options{
		BundleDir: p.companionDir, NodePath: p.nodeExe, DataDir: filepath.Join(root, "isolated-data"), CodexHome: filepath.Join(root, "isolated-codex"),
	})
	status, err := service.Preview(context.Background())
	if err == nil || status.Available || strings.Contains(status.Message, "请先安装 Node.js") {
		t.Fatalf("damaged package must fail readiness without asking for external Node: %+v, %v", status, err)
	}
}

func TestDevelopmentRuntimeKeepsExplicitBundleAndSystemNode(t *testing.T) {
	root := t.TempDir()
	core := filepath.Join(root, "desktop", "bin", "SalcaraBridge.exe")
	bundle := filepath.Join(root, "source", "desktop-companion")
	nodeDir := filepath.Join(root, "developer-node")
	node := filepath.Join(nodeDir, "node")
	if runtime.GOOS == "windows" {
		node += ".exe"
	}
	runtimeFixtureFile(t, node, "Synthetic developer runtime: never launched")
	t.Setenv("PATH", nodeDir)
	p := resolveRuntimePaths(core, "", bundle, true)
	if p.coreExe != core || p.startupExe != core || p.companionDir != bundle || p.nodeExe != node {
		t.Fatalf("explicit developer resource path or Node fallback was lost: %+v", p)
	}
}

func TestDesktopLauncherRequiresEmbeddedRegularExecutable(t *testing.T) {
	root := t.TempDir()
	core := filepath.Join(root, "resources", "SalcaraBridge.exe")
	desktop := filepath.Join(root, "Salcara Bridge.exe")
	runtimeFixtureFile(t, desktop, "Synthetic launcher: never launched")
	for _, example := range []struct {
		name, desktop string
		embedded      bool
	}{
		{"standalone", desktop, false},
		{"missing", filepath.Join(root, "absent.exe"), true},
		{"directory", root, true},
		{"relative", "relative.exe", true},
	} {
		t.Run(example.name, func(t *testing.T) {
			p := resolveRuntimePaths(core, example.desktop, "", example.embedded)
			if p.startupExe != core || p.coreExe != core {
				t.Fatalf("invalid/non-embedded desktop path replaced the core: %+v", p)
			}
		})
	}
}

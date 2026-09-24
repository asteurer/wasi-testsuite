// Command wago-runner executes a single wasi-testsuite test module through the
// wago runtime's Go embedding API, translating the suite's invocation parameters
// (environment, a preopened directory, and guest arguments) into a
// wago-org/wasi configuration.
//
// The wago CLI (`wago run`) has no per-invocation flags for environment
// variables or preopened directories, so the wasi-testsuite adapter cannot build
// a plain wago command line. This helper fills that gap.
//
// Two host modes are supported, selected with --wasi:
//
//	--wasi p1   Preview 1 core modules via github.com/wago-org/wasi/p1
//	            (mirrors that package's p1/wasitest_exec_test.go).
//	--wasi p2   Preview 2 components via github.com/wago-org/wasi/p2 +
//	            github.com/wago-org/component-model (mirrors p2/p2_test.go).
//	            Used to run the wasm32-wasip3 suite for now.
//
// Usage:
//
//	wago-runner [--wasi p1|p2] [--env KEY=VALUE]... [--dir HOST]... <module.wasm> [args...]
//
// The guest's stdout/stderr are written through to this process's, and it exits
// with the guest's exit code.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"

	component "github.com/wago-org/component-model"
	wago "github.com/wago-org/wago"
	wagoplugin "github.com/wago-org/wago/plugin"
	"github.com/wago-org/wasi/p1"
	"github.com/wago-org/wasi/p2"
)

type stringList []string

func (s *stringList) String() string     { return fmt.Sprint([]string(*s)) }
func (s *stringList) Set(v string) error { *s = append(*s, v); return nil }

func main() {
	wasi := flag.String("wasi", "p1", "host bundle: p1 (Preview 1 core module) or p2 (Preview 2 component)")
	var env, dirs stringList
	flag.Var(&env, "env", "guest environment variable KEY=VALUE (repeatable)")
	flag.Var(&dirs, "dir", "host directory to preopen as guest / (repeatable)")
	flag.Parse()

	rest := flag.Args()
	if len(rest) < 1 {
		fmt.Fprintln(os.Stderr, "usage: wago-runner [--wasi p1|p2] [--env K=V]... [--dir HOST]... <module.wasm> [args...]")
		os.Exit(2)
	}
	wasmPath, guestArgs := rest[0], rest[1:]

	switch *wasi {
	case "p1":
		os.Exit(runP1(wasmPath, guestArgs, env, dirs))
	case "p2":
		os.Exit(runP2(wasmPath, guestArgs, env, dirs))
	default:
		fmt.Fprintf(os.Stderr, "unknown --wasi %q (want p1 or p2)\n", *wasi)
		os.Exit(2)
	}
}

// runP1 executes a Preview 1 core module.
func runP1(wasmPath string, guestArgs, env, dirs []string) int {
	src, err := os.ReadFile(wasmPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "read:", err)
		return 1
	}
	compiled, err := wago.Compile(nil, src)
	if err != nil {
		fmt.Fprintln(os.Stderr, "compile:", err)
		return 1
	}

	// Guest argv is [program name, args...], matching the reference adapters.
	cfg := p1.Config{
		Args:   append([]string{filepath.Base(wasmPath)}, guestArgs...),
		Env:    env,
		Stdin:  os.Stdin,
		Stdout: os.Stdout,
		Stderr: os.Stderr,
	}
	for _, host := range dirs {
		tmp, cleanup, err := stagePreopen(host)
		if err != nil {
			fmt.Fprintln(os.Stderr, "preopen:", err)
			return 1
		}
		defer cleanup()
		cfg.Mounts = append(cfg.Mounts, p1.Preopen{
			GuestPath: "/", HostPath: tmp, Read: true, Write: true, MutateDirectory: true,
		})
	}

	in, err := wago.Instantiate(compiled, wago.InstantiateOptions{Imports: p1.Imports(cfg)})
	if err != nil {
		fmt.Fprintln(os.Stderr, "instantiate:", err)
		return 1
	}
	defer in.Close()

	if _, err := in.Invoke("_start"); err != nil {
		var ex *wago.ExitError
		if errors.As(err, &ex) {
			return int(ex.Code)
		}
		fmt.Fprintln(os.Stderr, "trap:", err)
		return 1
	}
	return 0
}

// runP2 executes a Preview 2 component through the component-model plugin, wired
// through the wago plugin runtime exactly as wago-org/wasi's p2/p2_test.go does.
func runP2(wasmPath string, guestArgs, env, dirs []string) int {
	src, err := os.ReadFile(wasmPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "read:", err)
		return 1
	}

	var ref *wagoplugin.Ref[component.Service]
	providers := []wago.PluginProvider{component.Provider(), componentConsumer(&ref)}
	rt := wago.NewRuntime()
	defer rt.Close()
	set, err := pluginSet(providers)
	if err != nil {
		fmt.Fprintln(os.Stderr, "plugin set:", err)
		return 1
	}
	if err := rt.LoadPlugins(context.Background(), set); err != nil {
		fmt.Fprintln(os.Stderr, "load plugins:", err)
		return 1
	}

	cfg := p2.Config{
		Args:   append([]string{filepath.Base(wasmPath)}, guestArgs...),
		Env:    env,
		Stdin:  p2.NewInputStream(os.Stdin),
		Stdout: p2.NewOutputStream(os.Stdout),
		Stderr: p2.NewOutputStream(os.Stderr),
	}
	for _, host := range dirs {
		tmp, cleanup, err := stagePreopen(host)
		if err != nil {
			fmt.Fprintln(os.Stderr, "preopen:", err)
			return 1
		}
		defer cleanup()
		cfg.Mounts = append(cfg.Mounts, p2.Preopen{
			GuestPath: "/", HostPath: tmp, Read: true, Write: true, MutateDirectory: true,
		})
	}

	runErr := ref.With(func(service component.Service) error {
		return p2.Run(context.Background(), service, src, cfg)
	})
	if runErr != nil {
		var ex *p2.ExitError
		if errors.As(runErr, &ex) {
			return int(ex.Code)
		}
		fmt.Fprintln(os.Stderr, "run:", runErr)
		return 1
	}
	return 0
}

// --- component-model plugin wiring (adapted from wago-org/wasi p2/p2_test.go) ---

type pluginFunc func(*wago.Registrar) error

func (f pluginFunc) Register(r *wago.Registrar) error { return f(r) }

// componentConsumer is a minimal plugin that requires the component-model
// contract and captures a handle to the resolved component.Service.
func componentConsumer(ref **wagoplugin.Ref[component.Service]) wago.PluginProvider {
	d := wago.PluginDefinition{
		ID:         "example.com/wago-runner/component-consumer",
		Version:    "1.0.0",
		Provenance: wago.PluginProvenance{Repository: "https://example.com/wago-runner", License: "MIT"},
		Requires:   []wago.PluginRequirement{{ID: component.PluginID, Version: "^0.1.0"}},
		Consumes:   []wago.ContractRequirement{{ID: component.Contract.ID(), Major: component.Contract.Major(), Mode: wago.ContractRequired}},
	}
	return wago.PluginProvider{Definition: d, New: func() wago.Plugin {
		return pluginFunc(func(r *wago.Registrar) error {
			var err error
			*ref, err = wagoplugin.Require(r, component.Contract)
			return err
		})
	}}
}

// pluginSet resolves a provider list into a wago.PluginSet with the contract
// bindings the runtime needs to satisfy each consumer.
func pluginSet(providers []wago.PluginProvider) (wago.PluginSet, error) {
	set := wago.PluginSet{Providers: providers}
	for _, provider := range providers {
		digest, err := wago.DefinitionDigest(provider.Definition)
		if err != nil {
			return wago.PluginSet{}, err
		}
		s := wago.PluginSelection{
			ID:               provider.Definition.ID,
			DefinitionDigest: digest,
			Direct:           true,
			Dependencies:     map[string]string{},
		}
		for _, req := range provider.Definition.Requires {
			s.Dependencies[req.ID] = req.Version
		}
		for _, req := range provider.Definition.Authorities {
			s.Grants = append(s.Grants, wago.AuthorityGrant{Name: req.Name, Scope: req.Scope})
		}
		for _, req := range provider.Definition.Consumes {
			var owners []string
			for _, candidate := range providers {
				for _, spec := range candidate.Definition.Provides {
					if spec.ID == req.ID && spec.Major == req.Major {
						owners = append(owners, candidate.Definition.ID)
					}
				}
			}
			sort.Strings(owners)
			s.Contracts = append(s.Contracts, wago.ContractBinding{ID: req.ID, Major: req.Major, Providers: owners})
		}
		set.Selections = append(set.Selections, s)
	}
	return set, nil
}

// --- shared helpers ---

// stagePreopen copies host into a fresh temp dir so tests that mutate the
// preopen do not dirty the source fixtures (as wago-org/wasi's harness does),
// and returns the absolute temp path plus a cleanup func.
func stagePreopen(host string) (string, func(), error) {
	tmp, err := os.MkdirTemp("", "wago-wasi-")
	if err != nil {
		return "", func() {}, err
	}
	if err := copyTree(host, tmp); err != nil {
		os.RemoveAll(tmp)
		return "", func() {}, err
	}
	return tmp, func() { os.RemoveAll(tmp) }, nil
}

// copyTree recursively copies src into dst, preserving directories, regular
// files, and symlinks. Adapted from github.com/wago-org/wasi p1/wasitest_exec_test.go.
func copyTree(src, dst string) error {
	return filepath.WalkDir(src, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return os.MkdirAll(target, info.Mode().Perm())
		}
		if entry.Type()&os.ModeSymlink != 0 {
			link, err := os.Readlink(path)
			if err != nil {
				return err
			}
			return os.Symlink(link, target)
		}
		in, err := os.Open(path)
		if err != nil {
			return err
		}
		defer in.Close()
		out, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, info.Mode().Perm())
		if err != nil {
			return err
		}
		_, copyErr := io.Copy(out, in)
		closeErr := out.Close()
		if copyErr != nil {
			return copyErr
		}
		return closeErr
	})
}

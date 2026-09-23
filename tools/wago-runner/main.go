// Command wago-runner executes a single WASI Preview 1 core module through the
// wago runtime's Go embedding API, translating wasi-testsuite invocation
// parameters (environment, a preopened directory, and guest arguments) into a
// wago-org/wasi/p1 configuration.
//
// The wago CLI (`wago run`) has no per-invocation flags for environment
// variables or preopened directories, so the wasi-testsuite adapter cannot
// build a plain wago command line. This helper fills that gap; it mirrors the
// reference harness in github.com/wago-org/wasi (p1/wasitest_exec_test.go).
//
// Usage:
//
//	wago-runner [--env KEY=VALUE]... [--dir HOST]... <module.wasm> [guest args...]
//
// It writes the guest's stdout/stderr through to its own, and exits with the
// guest's exit code.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	wago "github.com/wago-org/wago"
	"github.com/wago-org/wasi/p1"
)

type stringList []string

func (s *stringList) String() string     { return fmt.Sprint([]string(*s)) }
func (s *stringList) Set(v string) error { *s = append(*s, v); return nil }

func main() {
	var env, dirs stringList
	flag.Var(&env, "env", "guest environment variable KEY=VALUE (repeatable)")
	flag.Var(&dirs, "dir", "host directory to preopen as guest / (repeatable)")
	flag.Parse()

	rest := flag.Args()
	if len(rest) < 1 {
		fmt.Fprintln(os.Stderr, "usage: wago-runner [--env K=V]... [--dir HOST]... <module.wasm> [args...]")
		os.Exit(2)
	}
	os.Exit(run(rest[0], rest[1:], env, dirs))
}

func run(wasmPath string, guestArgs, env, dirs []string) int {
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
		// Copy the preopen to a temp dir so tests that mutate it do not dirty
		// the source fixtures (as wago-org/wasi's own harness does).
		tmp, err := os.MkdirTemp("", "wago-wasi-p1-")
		if err != nil {
			fmt.Fprintln(os.Stderr, "temp root:", err)
			return 1
		}
		defer os.RemoveAll(tmp)
		if err := copyTree(host, tmp); err != nil {
			fmt.Fprintln(os.Stderr, "copy root:", err)
			return 1
		}
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

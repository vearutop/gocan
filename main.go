package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/vearutop/gocan/internal/diff"
	"github.com/vearutop/gocan/internal/format"
)

func main() {
	var (
		writeFlag    bool
		listFlag     bool
		diffFlag     bool
		checkFlag    bool
		diffFile     string
		parentCommit string
		configPath   string
	)
	flag.BoolVar(&writeFlag, "w", false, "write result to (source) file instead of stdout")
	flag.BoolVar(&listFlag, "l", false, "list files whose formatting differs from gocan's")
	flag.BoolVar(&diffFlag, "d", false, "display diffs instead of rewriting files")
	flag.BoolVar(&checkFlag, "check", false, "exit non-zero if any file is not formatted")
	flag.StringVar(&diffFile, "diff", "", "git diff file for changes (optional, used with -denoise)")
	flag.StringVar(&parentCommit, "parent-commit", "", "git parent commit for diff base (optional, used with -denoise)")
	flag.StringVar(&configPath, "config", "", "path to JSON config file")
	flag.Parse()

	paths := flag.Args()
	if len(paths) == 0 {
		paths = []string{"-"}
	}

	files, err := collectFiles(paths)
	if err != nil {
		fatal(err)
	}
	if len(files) == 0 {
		return
	}

	var (
		cfg       format.Config
		baseDir   string
		cfgLoaded bool
	)
	if configPath != "" {
		var err error
		cfg, err = format.LoadConfig(configPath)
		if err != nil {
			fatal(err)
		}
		if err := format.ValidateConfig(cfg); err != nil {
			fatal(err)
		}
		baseDir = filepath.Dir(configPath)
		cfgLoaded = true
	}

	resolver := newConfigResolver()
	out := io.Writer(os.Stdout)
	changedAny := false
	for _, path := range files {
		if path == "-" {
			stdinCfg := cfg
			if !cfgLoaded {
				stdinCfg = format.DefaultConfig()
				if err := format.ValidateConfig(stdinCfg); err != nil {
					fatal(err)
				}
			}
			src, err := io.ReadAll(os.Stdin)
			if err != nil {
				fatal(err)
			}
			formatted, err := format.FormatFile("stdin", src, stdinCfg)
			if err != nil {
				fatal(err)
			}
			changed := !bytesEqual(src, formatted)
			if changed {
				changedAny = true
			}
			if diffFlag && changed {
				diff, err := unifiedDiff("stdin", src, formatted)
				if err != nil {
					fatal(err)
				}
				if _, err := out.Write(diff); err != nil {
					fatal(err)
				}
			}
			if listFlag && changed {
				if _, err := fmt.Fprintln(out, "stdin"); err != nil {
					fatal(err)
				}
			}
			if !writeFlag && !listFlag && !diffFlag {
				if _, err := out.Write(formatted); err != nil {
					fatal(err)
				}
				if len(formatted) == 0 || formatted[len(formatted)-1] != '\n' {
					if _, err := out.Write([]byte("\n")); err != nil {
						fatal(err)
					}
				}
			}
			continue
		}

		if !cfgLoaded {
			var err error
			cfg, baseDir, err = resolver.Resolve(path)
			if err != nil {
				fatal(err)
			}
		}
		if format.IsExcluded(path, baseDir, cfg) {
			continue
		}

		src, err := os.ReadFile(path)
		if err != nil {
			fatal(err)
		}
		formatted, err := format.FormatFile(path, src, cfg)
		if err != nil {
			fatal(err)
		}
		changed := !bytesEqual(src, formatted)
		if changed {
			changedAny = true
		}

		if listFlag && changed {
			if _, err := fmt.Fprintln(out, path); err != nil {
				fatal(err)
			}
		}

		if diffFlag && changed {
			diff, err := unifiedDiff(path, src, formatted)
			if err != nil {
				fatal(err)
			}
			if _, err := out.Write(diff); err != nil {
				fatal(err)
			}
		}

		if writeFlag && changed {
			if err := os.WriteFile(path, formatted, 0o644); err != nil {
				fatal(err)
			}
		}

		if !writeFlag && !listFlag && !diffFlag {
			if _, err := out.Write(formatted); err != nil {
				fatal(err)
			}
			if len(formatted) == 0 || formatted[len(formatted)-1] != '\n' {
				if _, err := out.Write([]byte("\n")); err != nil {
					fatal(err)
				}
			}
		}
	}

	if checkFlag && changedAny {
		os.Exit(1)
	}
}

func bytesEqual(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func collectFiles(paths []string) ([]string, error) {
	var files []string
	for _, p := range paths {
		if p == "-" {
			files = append(files, p)
			continue
		}
		p = format.CanonicalizePath(p)
		info, err := os.Stat(p)
		if err != nil {
			return nil, err
		}
		if info.IsDir() {
			root := p
			err := filepath.WalkDir(p, func(path string, d os.DirEntry, err error) error {
				if err != nil {
					return err
				}
				if d.IsDir() {
					if path == root {
						return nil
					}
					name := d.Name()
					if name == "vendor" || strings.HasPrefix(name, ".") {
						return filepath.SkipDir
					}
					return nil
				}
				if strings.HasSuffix(path, ".go") {
					files = append(files, path)
				}
				return nil
			})
			if err != nil {
				return nil, err
			}
			continue
		}
		if strings.HasSuffix(p, ".go") {
			files = append(files, p)
		}
	}
	return files, nil
}

func fatal(err error) {
	var pathErr *os.PathError
	if errors.As(err, &pathErr) {
		fmt.Fprintf(os.Stderr, "gocan: %v\n", pathErr)
		os.Exit(1)
	}
	fmt.Fprintf(os.Stderr, "gocan: %v\n", err)
	os.Exit(1)
}

func unifiedDiff(path string, a, b []byte) ([]byte, error) {
	out := diff.Diff(path, a, path, b)
	return out, nil
}

# gocan

[![Build Status](https://github.com/bool64/go-template/workflows/test-unit/badge.svg)](https://github.com/bool64/go-template/actions?query=branch%3Amaster+workflow%3Atest-unit)
[![Coverage Status](https://codecov.io/gh/bool64/go-template/branch/master/graph/badge.svg)](https://codecov.io/gh/bool64/go-template)
[![GoDevDoc](https://img.shields.io/badge/dev-doc-00ADD8?logo=go)](https://pkg.go.dev/github.com/bool64/go-template)
[![Time Tracker](https://wakatime.com/badge/github/bool64/go-template.svg)](https://wakatime.com/badge/github/bool64/go-template)
![Code lines](https://sloc.xyz/github/bool64/go-template/?category=code)
![Comments](https://sloc.xyz/github/bool64/go-template/?category=comments)

`gocan` formats Go source files into a user-defined canonical declaration order to make diffs and merges cleaner. 
It is intentionally gofmt-like: you can rewrite files, list files that would change, or show diffs.

## Features

- Canonical declaration ordering based on a simple JSON config.
- gofmt-style controls: rewrite (`-w`), list (`-l`), or diff (`-d`).
- Stable ordering inside each configured block (alphabetical by name).
- Import declarations stay at the top in their original order.
- Safe handling of `const` blocks: no splitting or spec reordering, preserving `iota` semantics.
- Works on individual files or recursively on directories (skips `vendor` and dot-directories).
- Optional helper attachment: unexported functions called by exactly one top-level parent are placed directly after that parent.

## Quick Start

```bash
go install github.com/vearutop/gocan/cmd/gocan@latest
gocan -w .
```

Or download binary from [releases](https://github.com/vearutop/gocan/releases).

### Linux AMD64

```
wget https://github.com/vearutop/gocan/releases/latest/download/linux_amd64.tar.gz && tar xf linux_amd64.tar.gz && rm linux_amd64.tar.gz
./gocan -version
```


## Config

Config is a JSON file defining declaration order. Each rule is matched by `kind` and `exported`. For `receiver` rules, `exportedMethod` controls method grouping within each receiver type. `packageMainFunc` ignores `exported`.

Supported `kind` values:

- `const`
- `var`
- `type`
- `func`
- `receiver` (methods grouped by receiver type)
- `packageMainFunc` (`func main()` in `package main`)
- `constructor` (functions named `New*`)

Example:

```json
{
  "order": [
    {"kind": "packageMainFunc"},
    {"kind": "const", "exported": true},
    {"kind": "var", "exported": true},
    {"kind": "func", "exported": true},
    {"kind": "constructor", "exported": true},
    {"kind": "type", "exported": true},
    {"kind": "receiver", "exported": true, "exportedMethod": true},
    {"kind": "receiver", "exported": true, "exportedMethod": false},
    {"kind": "const", "exported": false},
    {"kind": "var", "exported": false},
    {"kind": "func", "exported": false},
    {"kind": "constructor", "exported": false},
    {"kind": "type", "exported": false},
    {"kind": "receiver", "exported": false, "exportedMethod": true},
    {"kind": "receiver", "exported": false, "exportedMethod": false}
  ],
  "helperAttachment": true,
  "exclude": [
    "**/*_generated.go",
    "internal/legacy/**"
  ]
}
```

`helperAttachment` behavior:

- Only unexported top-level functions with exactly one top-level caller in the same file are attached.
- Attachment is transitive for single-parent chains (`top -> helperA -> helperB` results in `top, helperA, helperB`).
- Helpers called by multiple parents remain in normal order.

Run with:

```bash
./gocan -w -config gocan.json .
```

Config discovery:

- If `-config` is provided, it is used for all files.
- Otherwise, `gocan` searches for `.gocan.json` in the file's directory and its parents.
- If no config is found, the built-in defaults apply.

## CLI

- `-w` rewrite files in place
- `-l` list files whose formatting differs
- `-d` display unified diffs
- `-check` exit non-zero if any file is not formatted
- `-config` path to JSON config file
- `-gh-annotate` emit GitHub Actions annotations for layout-only changes
- `-gh-head` git head ref/sha for `-gh-annotate` (default `HEAD`)
- `-gh-message` annotation message for `-gh-annotate`
- `-gh-base` git base ref/sha for `-gh-annotate` (defaults to `GITHUB_BASE_SHA`, falls back to `origin/main` if empty)

If no paths are provided (or `-` is given), `gocan` reads from stdin and writes to stdout.

## GitHub Actions Annotations

`gocan` can emit GitHub Actions annotations that mark layout-only changes (i.e., changes that disappear after canonicalization). This helps reviewers focus on semantic changes.

Example workflow step:

```yaml
- name: Layout-only annotations
  env:
    BASE_SHA: ${{ github.event.pull_request.base.sha }}
  run: |
    gocan -gh-annotate -gh-base "$BASE_SHA"
```

Notes:
- `-gh-annotate` ignores new files that don't exist in the base ref.
- If `-gh-base` is not provided, `gocan` uses `GITHUB_BASE_SHA` if set, otherwise tries `origin/main`, `origin/master`, `main`, `master` (in that order).
- Set `GOCAN_DEBUG=1` to print debug logs and a final summary to stderr.
- In mixed files (layout-only moves plus semantic edits), annotations target only unchanged declarations whose text matches base after canonicalization.

## Notes and Limitations

- Const/var/type blocks are reordered as whole declarations. If a block mixes exported and unexported specs, it is classified by the first name.
- Functions named `New*` are treated as constructors.
- If two declarations tie on rule and name (e.g. multiple `const` blocks starting with `_`), the formatted declaration text is used as a deterministic tie-breaker.
- Exclude patterns use `/` separators and support `*`, `?`, and `**` (any number of path segments), matched relative to the config file directory.
- Helper attachment applies only to unexported top-level functions with exactly one top-level caller in the same file. Helpers called by multiple parents remain in normal order.

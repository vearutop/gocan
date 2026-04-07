# gocan

[![Build Status](https://github.com/vearutop/gocan/workflows/test-unit/badge.svg)](https://github.com/vearutop/gocan/actions?query=branch%3Amaster+workflow%3Atest-unit)
[![Coverage Status](https://codecov.io/gh/vearutop/gocan/branch/master/graph/badge.svg)](https://codecov.io/gh/vearutop/gocan)
[![GoDevDoc](https://img.shields.io/badge/dev-doc-00ADD8?logo=go)](https://pkg.go.dev/github.com/vearutop/gocan)
[![Time Tracker](https://wakatime.com/badge/github/vearutop/gocan.svg)](https://wakatime.com/badge/github/vearutop/gocan)
![Code lines](https://sloc.xyz/github/vearutop/gocan/?category=code)
![Comments](https://sloc.xyz/github/vearutop/gocan/?category=comments)

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
- `-gh-head` git head ref/sha for `-gh-annotate` (defaults to `GITHUB_HEAD_SHA`/`GITHUB_HEAD_REF`, otherwise `HEAD`)
- `-gh-base` git base ref/sha for `-gh-annotate` (defaults to `GITHUB_BASE_SHA`/`GITHUB_BASE_REF` if set)
- `-gh-max-notices` max GitHub Actions notices to emit (default `10`)
- `-gh-skip-summary` skip writing full notice list to `GITHUB_STEP_SUMMARY` (default `false`)

If no paths are provided (or `-` is given), `gocan` reads from stdin and writes to stdout.

## GitHub Actions Annotations

`gocan` can emit GitHub Actions annotations that mark layout-only changes (i.e., changes that disappear after canonicalization). This helps reviewers focus on semantic changes.

Example workflow:

```yaml
name: gocan
on:
  pull_request:

# Cancel the workflow in progress if a newer build is about to start.
concurrency:
  group: ${{ github.workflow }}-${{ github.head_ref || github.run_id }}
  cancel-in-progress: true

jobs:
  gocan:
    runs-on: ubuntu-latest
    steps:
      - name: Checkout code
        uses: actions/checkout@v6
      - name: Annotate layout-only changes
        run: |
          curl -sLO https://github.com/vearutop/gocan/releases/download/v0.0.3/linux_amd64.tar.gz && tar xf linux_amd64.tar.gz
          gocan_hash=$(git hash-object ./gocan)
          [ "$gocan_hash" == "cfcb9c334ed3e887a802f1fde35fbf7f5af2d8e8" ] || (echo "::error::unexpected hash for gocan, possible tampering: $gocan_hash" && exit 1)
          git fetch --no-tags --depth=1 origin ${{ github.event.pull_request.base.sha }}
          git fetch --no-tags --depth=1 origin ${{ github.event.pull_request.head.sha }}
          ./gocan -gh-annotate -gh-base ${{ github.event.pull_request.base.sha }} -gh-head ${{ github.event.pull_request.head.sha }}

```

Notes:
- New files are ignored unless they contain declarations that existed in base (i.e., moved between files within the same package directory), in which case those moved declarations can be annotated.
- If `-gh-base` is not provided, `gocan` uses `GITHUB_BASE_SHA` or `GITHUB_BASE_REF` (as `origin/<ref>`). If still empty, it tries `origin/main`, `origin/master`, `main`, `master` (in that order).
- If `-gh-head` is not provided, `gocan` uses `GITHUB_HEAD_SHA` or `GITHUB_HEAD_REF` (as `origin/<ref>`), otherwise `HEAD`.
- Set `GOCAN_DEBUG=1` to print debug logs and a final summary to stderr.
- In mixed files (layout-only moves plus semantic edits), annotations target only unchanged declarations whose text matches base after canonicalization.
- Layout-only detection is package-scoped within a directory: declarations moved between files in the same directory/package can be annotated.
- `-gh-annotate` also tries to detect likely moves between packages in the PR by matching identical normalized declarations that disappear from one package and appear in another. Ambiguous matches are skipped.

## Notes and Limitations

- Const/var/type blocks are reordered as whole declarations. If a block mixes exported and unexported specs, it is classified by the first name.
- Functions named `New*` are treated as constructors.
- If two declarations tie on rule and name (e.g. multiple `const` blocks starting with `_`), the formatted declaration text is used as a deterministic tie-breaker.
- Exclude patterns use `/` separators and support `*`, `?`, and `**` (any number of path segments), matched relative to the config file directory.
- Helper attachment applies only to unexported top-level functions with exactly one top-level caller in the same file. Helpers called by multiple parents remain in normal order.

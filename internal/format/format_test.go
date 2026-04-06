package format_test

import (
	"strings"
	"testing"

	"github.com/vearutop/gocan/internal/format"
)

func TestBasicOrdering(t *testing.T) {
	src := `package p

// Exported constants
const (
	B = 2
)

// Another const
const (
	A = 1
)

type Z struct{}

type AType struct{}

func NewZ() *Z { return &Z{} }

func (Z) M() {}

// ExportedFunc does a thing.
func ExportedFunc() {}

const (
	c = 3
)

type z struct{}

func (z) m() {}

func unexported() {}

func newLocal() {}
`

	cfg := format.DefaultConfig()
	out, err := format.FormatFile("test.go", []byte(src), cfg)
	if err != nil {
		t.Fatalf("format: %v", err)
	}

	outStr := string(out)
	if !strings.Contains(outStr, "// ExportedFunc does a thing.") {
		t.Fatalf("expected comment to survive rearranging")
	}
	if !strings.Contains(outStr, "// Exported constants") {
		t.Fatalf("expected comment to survive rearranging")
	}

	assertOrder(t, outStr,
		"// Another const",
		"const (\n\tA = 1",
		"const (\n\tB = 2",
		"// ExportedFunc does a thing.",
		"func ExportedFunc()",
		"func NewZ() *Z",
		"type AType struct{}",
		"type Z struct{}",
		"func (Z) M()",
		"const (\n\tc = 3",
		"func newLocal()",
		"func unexported()",
		"type z struct{}",
		"func (z) m()",
	)
}

func TestBuildTagsPreserved(t *testing.T) {
	src := `//go:build go1.18
// +build go1.18

package p

const B = 2
const A = 1
`

	cfg := format.DefaultConfig()
	out, err := format.FormatFile("test.go", []byte(src), cfg)
	if err != nil {
		t.Fatalf("format: %v", err)
	}

	outStr := string(out)
	assertOrder(t, outStr,
		"//go:build go1.18",
		"// +build go1.18",
		"package p",
		"const A = 1",
		"const B = 2",
	)
}

func TestConstTieBreakerByText(t *testing.T) {
	src := `package p

const (
	_ = "b"
)

const (
	_ = "a"
)
`

	cfg := format.DefaultConfig()
	out, err := format.FormatFile("test.go", []byte(src), cfg)
	if err != nil {
		t.Fatalf("format: %v", err)
	}

	outStr := string(out)
	assertOrder(t, outStr,
		"const (\n\t_ = \"a\"",
		"const (\n\t_ = \"b\"",
	)
}

func TestHelperAttachment(t *testing.T) {
	src := `package p

func doWork() {
	helperB()
	helperA()
}

// helperA comment
func helperA() {}

func helperB() {}

func other() {
	helperB()
}
`

	cfg := format.DefaultConfig()
	out, err := format.FormatFile("test.go", []byte(src), cfg)
	if err != nil {
		t.Fatalf("format: %v", err)
	}

	outStr := string(out)
	if !strings.Contains(outStr, "// helperA comment") {
		t.Fatalf("expected helper comment to survive rearranging")
	}

	assertOrder(t, outStr,
		"func doWork()",
		"// helperA comment",
		"func helperA()",
		"func helperB()",
		"func other()",
	)
}

func TestHelperAttachmentDisabled(t *testing.T) {
	src := `package p

func doWork() {
	helperA()
}

func helperA() {}
`

	cfg := format.DefaultConfig()
	cfg.HelperAttachment = false
	out, err := format.FormatFile("test.go", []byte(src), cfg)
	if err != nil {
		t.Fatalf("format: %v", err)
	}

	outStr := string(out)
	assertOrder(t, outStr,
		"func doWork()",
		"func helperA()",
	)
}

func TestHelperAttachmentNested(t *testing.T) {
	src := `package p

func top() {
	helperA()
}

func helperA() {
	helperB()
}

func helperB() {}

func other() {}
`

	cfg := format.DefaultConfig()
	out, err := format.FormatFile("test.go", []byte(src), cfg)
	if err != nil {
		t.Fatalf("format: %v", err)
	}

	outStr := string(out)
	assertOrder(t, outStr,
		"func top()",
		"func helperA()",
		"func helperB()",
		"func other()",
	)
}

func TestLeadingCommentsStayWithDecl(t *testing.T) {
	src := `package p

// ToContext adds variable to context.

func ToContext() {}

// FromContext returns variables from context.

func FromContext() {}
`

	cfg := format.DefaultConfig()
	out, err := format.FormatFile("test.go", []byte(src), cfg)
	if err != nil {
		t.Fatalf("format: %v", err)
	}

	outStr := string(out)
	assertOrder(t, outStr,
		"// FromContext returns variables from context.",
		"func FromContext()",
		"// ToContext adds variable to context.",
		"func ToContext()",
	)
}

func TestMethodOrderByExportednessWithinReceiver(t *testing.T) {
	src := `package p

type Thing struct{}

func (Thing) zeta() {}

func (Thing) Alpha() {}
`

	cfg := format.DefaultConfig()
	out, err := format.FormatFile("test.go", []byte(src), cfg)
	if err != nil {
		t.Fatalf("format: %v", err)
	}

	outStr := string(out)
	assertOrder(t, outStr,
		"type Thing struct{}",
		"func (Thing) Alpha()",
		"func (Thing) zeta()",
	)
}

func TestMethodsFollowReceiverType(t *testing.T) {
	src := `package p

func (anotherErr) Error() string { return "boom" }

type anotherErr struct{}
`

	cfg := format.DefaultConfig()
	out, err := format.FormatFile("test.go", []byte(src), cfg)
	if err != nil {
		t.Fatalf("format: %v", err)
	}

	outStr := string(out)
	assertOrder(t, outStr,
		"type anotherErr struct{}",
		"func (anotherErr) Error() string",
	)
}

func TestPackageMainFuncKind(t *testing.T) {
	src := `package main

func helper() {}

func main() {}
`

	cfg := format.DefaultConfig()
	cfg.Order = append([]format.Rule{
		{Kind: "packageMainFunc"},
	}, cfg.Order...)
	out, err := format.FormatFile("test.go", []byte(src), cfg)
	if err != nil {
		t.Fatalf("format: %v", err)
	}

	outStr := string(out)
	assertOrder(t, outStr,
		"func main()",
		"func helper()",
	)
}

func assertOrder(t *testing.T, s string, markers ...string) {
	t.Helper()
	pos := 0
	for _, m := range markers {
		idx := strings.Index(s[pos:], m)
		if idx == -1 {
			t.Fatalf("expected marker not found: %q", m)
		}
		pos += idx + len(m)
	}
}

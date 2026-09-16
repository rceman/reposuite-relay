package gofmtstruct

import (
	"strings"
	"testing"
)

func formatted(t *testing.T, source string) string {
	t.Helper()
	result, err := FormatSource("fixture.go", []byte(source))
	if err != nil {
		t.Fatal(err)
	}
	return string(result)
}

func TestFormatSourceUsesVerticalKeyedStructLiterals(t *testing.T) {
	got := formatted(t, `package fixture

type Named struct { A int; B int }

var _ = struct { A int; B int }{A: 1, B: 2}
var _ = Named{A: 1, B: 2}
`)
	if strings.Count(got, "A: 1,\n") != 2 || strings.Count(got, "B: 2,\n") != 2 {
		t.Fatalf("keyed literals were not vertical:\n%s", got)
	}
}

func TestFormatSourceHandlesNestedAndCommentedLiterals(t *testing.T) {
	got := formatted(t, `package fixture

type Outer struct { Inner struct { A int; B int }; C int }

var _ = Outer{Inner: struct { A int; B int }{A: 1, B: 2}, /* keep */ C: 3}
`)
	for _, want := range []string{"Inner:", "A: 1,", "B: 2,", "/* keep */", "C: 3,"} {
		if !strings.Contains(got, want) {
			t.Fatalf("formatted output lost %q:\n%s", want, got)
		}
	}
}

func TestFormatSourceHandlesModelIdentifierAndPolicyFixtures(t *testing.T) {
	got := formatted(t, `package fixture

type ProjectIdentifiers struct { SchemaVersion int; ProjectID string; ProjectCode string; NextTaskNumber uint64; NextADRNumber uint64 }
type WorkflowPolicyAgent struct { WaitForCI bool }
type WorkflowPolicyCI struct { Task string; TaskMerge string; Release string }
type ProjectWorkflowPolicy struct { SchemaVersion int; ProjectID string; Revision int; WorkflowStage string; IntegrationBranch string; Agent WorkflowPolicyAgent; CI WorkflowPolicyCI }

var _ = ProjectIdentifiers{SchemaVersion: 1, ProjectID: "gateway", ProjectCode: "GTW", NextTaskNumber: 2, NextADRNumber: 3}
var _ = ProjectWorkflowPolicy{SchemaVersion: 1, ProjectID: "gateway", Revision: 2, WorkflowStage: "develop_active", IntegrationBranch: "develop", Agent: WorkflowPolicyAgent{WaitForCI: true}, CI: WorkflowPolicyCI{Task: "require", TaskMerge: "observe", Release: "disabled"}}
`)
	for _, want := range []string{
		"ProjectIdentifiers{\n",
		"ProjectID",
		"ProjectWorkflowPolicy{\n",
		"Agent: WorkflowPolicyAgent{\n",
		"WaitForCI: true,\n",
		"CI: WorkflowPolicyCI{\n",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("model fixture was not structurally formatted at %q:\n%s", want, got)
		}
	}
	if !strings.Contains(got, "ProjectID") || !strings.Contains(got, "\"gateway\",") {
		t.Fatalf("identifier fields were not retained in the formatted fixture:\n%s", got)
	}
	if !strings.Contains(got, "Release") || !strings.Contains(got, "\"disabled\",") {
		t.Fatalf("policy CI fields were not retained in the formatted fixture:\n%s", got)
	}
}

func TestFormatSourceLeavesSingleFieldUnkeyedAndMapsCompact(t *testing.T) {
	got := formatted(t, `package fixture

type Named struct { A int; B int }
type Alias map[string]int

var _ = Named{A: 1}
var _ = Named{1, 2}
var _ = map[string]int{"a": 1, "b": 2}
var _ = Alias{"a": 1, "b": 2}
`)
	if strings.Contains(got, "Named{\n") {
		t.Fatalf("single-field or unkeyed literal was expanded:\n%s", got)
	}
	if strings.Contains(got, "Alias{\n") || strings.Contains(got, "map[string]int{\n") {
		t.Fatalf("map literal was expanded:\n%s", got)
	}
}

func TestFormatSourceIsIdempotent(t *testing.T) {
	source := `package fixture

type Named struct { A int; B int }

var _ = Named{
	A: 1,
	B: 2,
}
`
	first := formatted(t, source)
	second := formatted(t, first)
	if first != second {
		t.Fatalf("formatter is not idempotent:\nfirst:\n%s\nsecond:\n%s", first, second)
	}
}

func TestFormatPackageLeavesNamedMapDeclaredInAnotherFileUnchanged(t *testing.T) {
	sources := map[string][]byte{
		"types.go": []byte("package fixture\n\ntype Alias map[string]int\n"),
		"use.go":   []byte("package fixture\n\nvar _ = Alias{\"a\": 1, \"b\": 2}\n"),
	}
	formatted, err := FormatPackage(sources)
	if err != nil {
		t.Fatal(err)
	}
	if string(formatted["use.go"]) != string(sources["use.go"]) {
		t.Fatalf("cross-file named map changed:\n%s", formatted["use.go"])
	}
}

func TestFormatSourceLeavesImportedNamedMapUnchanged(t *testing.T) {
	source := []byte("package fixture\n\nimport \"net/http\"\n\nvar _ = http.Header{\"a\": []string{\"b\"}, \"c\": []string{\"d\"}}\n")
	formatted, err := FormatSource("use.go", source)
	if err != nil {
		t.Fatal(err)
	}
	if string(formatted) != string(source) {
		t.Fatalf("imported named map changed:\n%s", formatted)
	}
}

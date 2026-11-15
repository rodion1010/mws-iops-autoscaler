//go:build e2e

package e2e

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// classifierStep returns the shell of e2e-pr.yml's changes job, so the test exercises the
// workflow's own code rather than a copy.
func classifierStep(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", ".github", "workflows", "e2e-pr.yml"))
	if err != nil {
		t.Fatalf("read workflow: %v", err)
	}
	var wf struct {
		Jobs map[string]struct {
			Steps []struct {
				Run string `yaml:"run"`
			} `yaml:"steps"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(raw, &wf); err != nil {
		t.Fatalf("parse workflow: %v", err)
	}
	job, ok := wf.Jobs["changes"]
	if !ok || len(job.Steps) != 1 {
		t.Fatalf("workflow has no single-step changes job")
	}
	return job.Steps[0].Run
}

// runClassifier runs the step as the runner does (bash -eo pipefail) with a stub gh that prints
// rows, or fails when rows is "FAIL". grepFails makes `grep -vE` exit 2.
func runClassifier(t *testing.T, step string, changed int, rows string, grepFails bool) string {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "bin")
	if err := os.Mkdir(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	gh := "#!/bin/sh\ncat \"$STUB_ROWS\"\n"
	if rows == "FAIL" {
		gh = "#!/bin/sh\nexit 1\n"
	}
	writeExec(t, filepath.Join(bin, "gh"), gh)
	if grepFails {
		realGrep, err := exec.LookPath("grep")
		if err != nil {
			t.Fatal(err)
		}
		writeExec(t, filepath.Join(bin, "grep"), fmt.Sprintf("#!/bin/sh\nfor a; do [ \"$a\" = -vE ] && exit 2; done\nexec %s \"$@\"\n", realGrep))
	}
	rowsFile := filepath.Join(dir, "rows.tsv")
	if err := os.WriteFile(rowsFile, []byte(rows), 0o644); err != nil {
		t.Fatal(err)
	}
	stepFile := filepath.Join(dir, "step.sh")
	if err := os.WriteFile(stepFile, []byte(step), 0o644); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "output")
	cmd := exec.Command("bash", "--noprofile", "--norc", "-eo", "pipefail", stepFile)
	cmd.Env = append(os.Environ(),
		"PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"),
		"GITHUB_OUTPUT="+out, "RUNNER_TEMP="+dir, "STUB_ROWS="+rowsFile,
		fmt.Sprintf("CHANGED=%d", changed), "REPO=o/r", "PR=1")
	_ = cmd.Run() // A failing step leaves no output, which the caller reads as "run smoke".
	got, _ := os.ReadFile(out)
	return strings.TrimSpace(string(got))
}

func writeExec(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
}

func row(name string) string { return name + "\t" + name + "\n" }

func TestRegression_Workflow_DocsOnlyClassifierFailsClosed(t *testing.T) {
	step := classifierStep(t)

	// A code file first in a long list: an early-exiting grep in a pipe would SIGPIPE the writer
	// and, under pipefail, read as "docs only".
	large := row("internal/agent.go")
	for i := 0; i < 999; i++ {
		large += row(fmt.Sprintf("docs/%0100d.md", i))
	}

	cases := []struct {
		name      string
		changed   int
		rows      string
		grepFails bool
		want      string
	}{
		{"docs only", 3, row("README.md") + row("docs/a.md") + row("LICENSE"), false, "docs_only=true"},
		{"code change", 2, row("README.md") + row("internal/x.go"), false, "docs_only=false"},
		{"code renamed into docs", 1, "docs/x.md\tinternal/x.go\n", false, "docs_only=false"},
		{"list shorter than changed_files", 5, row("README.md"), false, "docs_only=false"},
		{"API failure", 1, "FAIL", false, "docs_only=false"},
		{"no files", 0, "", false, "docs_only=false"},
		{"workflow file", 1, row(".github/workflows/e2e-pr.yml"), false, "docs_only=false"},
		{"large PR with code first", 1000, large, false, "docs_only=false"},
		{"classification error", 1, row("README.md"), true, "docs_only=false"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := runClassifier(t, step, tc.changed, tc.rows, tc.grepFails)
			// Empty output means the step failed, and smoke runs on a missing docs_only too.
			if tc.want == "docs_only=false" && got == "" {
				return
			}
			if got != tc.want {
				t.Errorf("output = %q, want %q", got, tc.want)
			}
		})
	}
}

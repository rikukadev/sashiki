package terraform

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func TestRenderedUserDataDoesNotContainGitHubToken(t *testing.T) {
	template := readFile(t, "user-data.sh.tftpl")
	main := readFile(t, "main.tf")
	variables := readFile(t, "variables.tf")

	// This replacement reproduces the old unsafe template input. Keeping the
	// sentinel in the map makes the test fail if a literal token placeholder is
	// ever reintroduced, while the safe input renders only a parameter ARN.
	const token = "github_pat_DO_NOT_RENDER_THIS_SECRET"
	const parameterARN = "arn:aws:ssm:ap-northeast-1:123456789012:parameter/prod/sashiki/github-token"
	rendered := strings.NewReplacer(
		"${github_token}", token,
		"${github_token_ssm_parameter_arn}", parameterARN,
	).Replace(template)
	if strings.Contains(rendered, token) {
		t.Fatal("rendered user-data contains the GitHub token value")
	}
	if !strings.Contains(rendered, parameterARN) {
		t.Fatal("rendered user-data does not contain the SSM parameter ARN")
	}

	oldReference := regexp.MustCompile(`var\.github_token(?:[^A-Za-z0-9_]|$)`)
	if oldReference.MatchString(main) || strings.Contains(variables, `variable "github_token"`) {
		t.Fatal("Terraform module still accepts or passes a literal github_token")
	}
	for _, want := range []string{
		`github_token_ssm_parameter_arn = var.github_token_ssm_parameter_arn`,
		`actions   = ["ssm:GetParameter"]`,
		`resources = [statement.value]`,
	} {
		if !strings.Contains(main, want) {
			t.Errorf("main.tf missing %q", want)
		}
	}
}

func TestPrivateInstallKeepsXtraceDisabledUntilTokenIsDiscarded(t *testing.T) {
	template := readFile(t, "user-data.sh.tftpl")
	start := strings.Index(template, `%{ if github_token_ssm_parameter_arn != "" ~}`)
	if start < 0 {
		t.Fatal("private repository install branch is missing")
	}
	end := strings.Index(template[start:], `%{ else ~}`)
	if end < 0 {
		t.Fatal("private repository install branch has no public fallback")
	}
	private := template[start : start+end]

	previous := -1
	for _, want := range []string{
		"set +x",
		`GITHUB_TOKEN="$(aws ssm get-parameter --with-decryption`,
		"curl --config -",
		"GITHUB_TOKEN=\"$GITHUB_TOKEN\" bash",
		"unset GITHUB_TOKEN",
		"set -x",
	} {
		at := strings.Index(private, want)
		if at < 0 {
			t.Fatalf("private install branch missing %q", want)
		}
		if at <= previous {
			t.Fatalf("private install step %q is out of order", want)
		}
		previous = at
	}
}

func readFile(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(".", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

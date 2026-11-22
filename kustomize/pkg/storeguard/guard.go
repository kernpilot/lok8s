// Package storeguard is the one path that writes a plaintext file into a
// secrets store. A store keeps each plaintext secret file next to its SOPS
// ciphertext (<file>.enc), and a block of rules in the .gitignore of the
// store keeps every plaintext out of git. lo secrets set, lo secrets
// decrypt and the cache of the Secret generator write a plaintext through
// Guard.WritePlaintext only. Before the write, it checks that:
//
//   - the file is an entry of the store directory itself (Destination);
//   - its name does not end in .enc in any letter case (ReservedName): the
//     rule !Secret.*.enc of the block re-includes such a name, and git
//     would commit the plaintext;
//   - the path is a regular file or absent (DestinationOK): a symlink, a
//     directory, a FIFO, a socket or a device is refused;
//   - the .gitignore protects the store, or the guard can make it do so
//     (Guard.EnsureGitignore writes the block, or refuses the file).
//
// Then it writes a new private temp file in the store and renames it onto
// the path (WriteReplacing). A symlink or a hard link at the path is
// replaced as a name, and the file that it names keeps its old content.
// Guard.CheckPlaintext runs the same checks and writes nothing.
//
// The package uses the standard library only: the lo binary and the
// standalone Secret plugin both import it. A refusal or a failed write is a
// *Stop: the lines to print, which name files and fields, never a value, a
// rule of the .gitignore or the error text of the file system.
package storeguard

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"strings"
)

// StepPlaintext is the step that a refusal of WritePlaintext stops before,
// when the caller has no other name for it.
const StepPlaintext = "the plaintext write"

// Stop is the error of a store write that did not happen: a refusal before
// the write, or a write that failed and left the old file as it was. Lines
// are the lines to print, in order, each as one error line.
type Stop struct {
	Lines []string
}

func (s *Stop) Error() string { return strings.Join(s.Lines, "\n") }

func stop(lines ...string) *Stop { return &Stop{Lines: lines} }

// Guard holds what the messages and the .gitignore write of a store need.
// The zero Guard is ready to use.
type Guard struct {
	// Rel is a path for a message (lo: relative to the project). Nil uses
	// the path as it is. A message prints the result with printable bytes
	// only (Printable).
	Rel func(path string) string
	// FillGitignore fills the temp file of a .gitignore write with want.
	// Nil writes want (a seam: a test of lo makes the write stop part way).
	FillGitignore func(w io.Writer, want []byte) error
}

func (g Guard) rel(path string) string {
	if g.Rel == nil {
		return path
	}
	return g.Rel(path)
}

// Plaintext names one plaintext write of WritePlaintext.
type Plaintext struct {
	Dir  string // the store directory
	Base string // the name of the file in Dir
	Step string // the step that a refusal stops before ("" is StepPlaintext)
	Who  string // the command that the line of a failed write names; "" names none
}

// CheckName returns the name of the plaintext file of a key of a store,
// Secret.<name>.<namespace>.<key>, in the store dir, or a *Stop with the
// refusal of lo secrets set. It checks, in this order: each part
// (PartProblem), that the file is an entry of dir itself (Destination), and
// that the name does not end in .enc (ReservedName). The part check refuses
// each part that makes the name a path, so the Destination check refuses
// only an empty dir (the file would be at the root of the file system):
// New of the cache refuses an empty dir before, and set has a store path.
// set checks a key with it before it reads the value. A refusal names the
// field and its text, never the value.
func CheckName(dir, name, namespace, key string) (string, error) {
	for _, part := range []struct{ field, text string }{{"name", name}, {"namespace", namespace}, {"key", key}} {
		if why := PartProblem(part.text); why != "" {
			return "", stop(fmt.Sprintf("the %s '%s' is refused: %s. Nothing was written: use a %s without it", part.field, Printable(part.text), why, part.field))
		}
	}
	base := "Secret." + name + "." + namespace + "." + key
	if _, ok := Destination(dir, base); !ok {
		return "", stop(notInStore(base))
	}
	if ReservedName(base) {
		return "", stop(reservedRefusal("the key '"+Printable(key)+"'", base))
	}
	return base, nil
}

// reservedRefusal is the one text of the refusal of a plaintext file base
// whose name ends in .enc (ReservedName). subject is what is refused: the
// key of lo secrets set (CheckName, the text of the bash twin), or the
// plaintext write of a caller that has no key (WritePlaintext).
func reservedRefusal(subject, base string) string {
	return subject + " is refused: the plaintext file " + Printable(base) + " would end in .enc (in any letter case), and the .gitignore rule !Secret.*.enc lets git commit it. Nothing was written: use a key name that does not end in .enc"
}

// notInStore is the refusal of a plaintext file that is not an entry of the
// store directory itself (Destination).
func notInStore(base string) string {
	return "the plaintext file " + Printable(base) + " is refused: it is not a file of the store directory itself. Nothing was written: use a name, a namespace and a key without a path part"
}

// notRegular is the refusal of a path that is not a regular file and not
// absent (DestinationOK), before step.
func notRegular(rel, step string) string {
	return Printable(rel) + " is not a regular file (a symlink, a directory, a FIFO, a socket or a device), or its lookup failed: lo stopped before " + step + " — remove it, then run this command again"
}

// WritePlaintext writes data into the plaintext file p.Base of the store
// p.Dir, mode 0600. It writes nothing and returns a *Stop when:
//
//   - the file is not an entry of p.Dir itself (Destination);
//   - its name ends in .enc in any letter case (ReservedName);
//   - the path is not a regular file and not absent (DestinationOK);
//   - the .gitignore of p.Dir cannot protect the store, or its write fails
//     (EnsureGitignore).
//
// The callers check the name before (set and the cache with CheckName,
// decrypt with its own check of each entry), so a name refusal here comes
// from a direct caller, or from an entry that decrypt lists after its check.
// Then it writes a new private temp file in p.Dir and renames it onto the
// path (WriteReplacing). A write that fails leaves the old file as it was
// and returns a *Stop that names the path, and the temp file if its remove
// failed too. The caller makes p.Dir.
func (g Guard) WritePlaintext(p Plaintext, data []byte) error {
	path, step, err := g.destination(p)
	if err != nil {
		return err
	}
	if _, err := g.EnsureGitignore(p.Dir, step); err != nil {
		return err
	}
	if err := WriteReplacing(path, 0o600, func(w io.Writer) error {
		_, err := w.Write(data)
		return err
	}); err != nil {
		final := "cannot write " + Printable(g.rel(path)) + ": it was not changed"
		if p.Who != "" {
			final = p.Who + " " + final
		}
		return g.FailedWrite(err, final)
	}
	return nil
}

// CheckPlaintext runs the checks of WritePlaintext for the plaintext file p
// and writes nothing: the name and the path (Destination, ReservedName,
// DestinationOK), then the .gitignore of p.Dir (GitignorePlan, which does
// not write the block). It returns the *Stop that WritePlaintext returns
// for a refusal. A caller runs it before it computes a value that it will
// write (the cache, before a generator), or in place of the write (a dry
// run). A store directory that does not exist passes: the write makes the
// .gitignore and the file.
func (g Guard) CheckPlaintext(p Plaintext) error {
	_, step, err := g.destination(p)
	if err == nil {
		_, _, err = g.GitignorePlan(p.Dir, step)
	}
	return err
}

// destination checks the name and the path of the plaintext file p, as
// WritePlaintext does before the .gitignore, and returns the path and the
// step of a refusal.
func (g Guard) destination(p Plaintext) (path, step string, err error) {
	step = p.Step
	if step == "" {
		step = StepPlaintext
	}
	path, ok := Destination(p.Dir, p.Base)
	if !ok {
		return "", "", stop(notInStore(p.Base))
	}
	if ReservedName(p.Base) {
		return "", "", stop(reservedRefusal("the plaintext write", p.Base))
	}
	if !DestinationOK(path) {
		return "", "", stop(notRegular(g.rel(path), step))
	}
	return path, step, nil
}

// GitignorePlan reads the .gitignore of dir once and returns the bytes
// that it must have (PlanGitignore), and whether they differ from the file
// (write). It returns a *Stop in these cases:
//   - The .gitignore is not a regular file and not absent (DestinationOK):
//     a symlink, a directory, a FIFO, a socket or a device, or its lookup
//     fails. The write would go through a link to the file that it names,
//     and git does not read a .gitignore that is a symlink.
//   - The file cannot be read: a rewrite would drop the lines of the user.
//   - The file cannot protect the store (PlanGitignore).
//
// It reads at most GitignoreMaxBytes and one byte more, and the plan
// refuses a file larger than that. Each refusal names step, the step of the
// command that the check stops before, and the file with printable bytes
// only.
func (g Guard) GitignorePlan(dir, step string) (want []byte, write bool, err error) {
	gitignore := dir + "/.gitignore"
	rel := Printable(g.rel(gitignore))
	if !DestinationOK(gitignore) {
		return nil, false, stop(notRegular(rel, step))
	}
	f, err := os.Open(gitignore)
	if errors.Is(err, fs.ErrNotExist) {
		return []byte(gitignoreContent), true, nil
	}
	var raw []byte
	if err == nil {
		raw, err = io.ReadAll(io.LimitReader(f, GitignoreMaxBytes+1))
		if cerr := f.Close(); err == nil {
			err = cerr
		}
	}
	var problems []string
	if err != nil {
		problems = []string{"cannot read " + rel}
	} else {
		want, problems = PlanGitignore(raw, rel)
	}
	if problems != nil {
		return nil, false, stop(append(problems, GitignoreFinal(rel, step))...)
	}
	return want, !bytes.Equal(raw, want), nil
}

// EnsureGitignore makes sure that the .gitignore of the store dir keeps
// every plaintext out of git. It writes the file only when its bytes differ
// from the plan (GitignorePlan): no file, no block, the legacy lines, or a
// changed block, and reports whether it wrote it. A refused file is not
// changed. The write goes to a temp file in the store and a rename over the
// old name (WriteReplacing, mode 0644): it replaces a link or a hard link
// instead of writing through it. A write that fails leaves the old file as
// it was and returns a *Stop that names the file (FailedWrite). step names
// the step of the command that a refusal stops before.
//
// The file gets mode 0644 on each rewrite: the .gitignore is committed, and
// git reads the same rules for each user of the repository. It holds no
// secret.
func (g Guard) EnsureGitignore(dir, step string) (wrote bool, err error) {
	want, write, err := g.GitignorePlan(dir, step)
	if err != nil || !write {
		return false, err
	}
	fill := g.FillGitignore
	if fill == nil {
		fill = func(w io.Writer, want []byte) error {
			_, err := w.Write(want)
			return err
		}
	}
	gitignore := dir + "/.gitignore"
	if err := WriteReplacing(gitignore, 0o644, func(w io.Writer) error { return fill(w, want) }); err != nil {
		return false, g.FailedWrite(err, "cannot write "+Printable(g.rel(gitignore))+": it was not changed")
	}
	return true, nil
}

// FailedWrite is the *Stop of a write through WriteReplacing that failed,
// on a file that the guard checks before it writes it: the temp file that
// stays, if its remove failed too, then final. It has no error text of the
// file system, because that text has the raw path.
func (g Guard) FailedWrite(err error, final string) *Stop {
	if left, ok := errors.AsType[*TempLeftError](err); ok {
		return stop("cannot remove the temp file "+Printable(g.rel(left.Name))+": remove it by hand", final)
	}
	return stop(final)
}

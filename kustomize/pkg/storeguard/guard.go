// Package storeguard is the one path that writes a plaintext file into a
// secrets store. A store keeps each plaintext secret file next to its SOPS
// ciphertext (<file>.enc), and a block of rules in the .gitignore of the
// store keeps every plaintext out of git. lo secrets set, lo secrets
// decrypt and the cache of the Secret generator write a plaintext through
// Guard.WritePlaintext only. Before the write, it checks that:
//
//   - the file is an entry of the store directory itself (Destination);
//   - its name is not .gitignore in any letter case: the plaintext would
//     replace the rules that keep it out of git;
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
// The order holds after a power loss too: the guard syncs the store
// directory after the rename of the .gitignore, before the first byte of
// the plaintext, and again after the rename of the plaintext. MakeDir syncs
// the parent of each directory that it makes. The rules keep an untracked
// plaintext out of git only: a file that git tracks already stays tracked.
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
	"path/filepath"
	"strings"
	"syscall"
)

// StepPlaintext is the step that a refusal of WritePlaintext stops before,
// when the caller has no other name for it.
const StepPlaintext = "the plaintext write"

// Stop is the error of a store write: a refusal before the write, a write
// that failed and left the old file as it was, or a write whose file is in
// place but whose sync failed (its text says that the file was written).
// Lines are the lines to print, in order, each as one error line.
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
	// EnsureGitignore reads the file again after the rename and refuses a
	// file whose bytes are not want (bytes.Equal), so a fill that writes
	// other bytes and returns nil fails closed.
	FillGitignore func(w io.Writer, want []byte) error
	// syncDir syncs a directory. Nil is SyncDir (a seam: a test of this
	// package makes a sync fail).
	syncDir func(dir string) error
}

func (g Guard) sync(dir string) error {
	if g.syncDir == nil {
		return SyncDir(dir)
	}
	return g.syncDir(dir)
}

// syncFile syncs an open directory: (*os.File).Sync (a seam: a test of
// this package gives SyncDir the error of a file system).
var syncFile = (*os.File).Sync

// SyncDir syncs the directory dir (fsync), so that a rename or a new entry
// in it survives a power loss. A file system that cannot sync a directory
// (EINVAL, ENOTSUP) counts as synced: it gives no other way to do it.
func SyncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	err = syncFile(d)
	if cerr := d.Close(); err == nil {
		err = cerr
	}
	if errors.Is(err, syscall.EINVAL) || errors.Is(err, syscall.ENOTSUP) {
		return nil
	}
	return err
}

// MakeDir makes the store directory dir with its missing parents, mode
// perm (os.MkdirAll), and syncs the parent of each directory that it made,
// from the top: a new store then survives a power loss with the .gitignore
// that the first write puts in it. It syncs nothing when dir exists. An
// error of os.MkdirAll comes back as it is; a sync that fails is a *Stop.
//
// When a sync fails, MakeDir removes the directories that it made again,
// from the deepest (os.Remove: a directory that is not empty stays). A run
// of the command again then makes them again and syncs their parents: a
// directory that stays would make the next run sync nothing. A directory
// that it cannot remove is named in the *Stop.
func (g Guard) MakeDir(dir string, perm fs.FileMode) error {
	var made []string
	for d := filepath.Clean(dir); ; d = filepath.Dir(d) {
		if _, err := os.Lstat(d); !errors.Is(err, fs.ErrNotExist) {
			break
		}
		made = append(made, d)
		if filepath.Dir(d) == d {
			break
		}
	}
	if err := os.MkdirAll(dir, perm); err != nil {
		return err
	}
	for i := len(made) - 1; i >= 0; i-- {
		if err := g.sync(filepath.Dir(made[i])); err != nil {
			return g.unmake(made[i], made)
		}
	}
	return nil
}

// unmake removes the directories made, from the deepest, after the sync
// of the parent of failed failed, and returns the *Stop of MakeDir.
func (g Guard) unmake(failed string, made []string) *Stop {
	for _, d := range made {
		if err := os.Remove(d); err != nil {
			return stop("the sync of the parent directory of " + Printable(g.rel(failed)) + " failed, and lo cannot remove " + Printable(g.rel(d)) + ", which it made: a power loss can lose it. lo wrote no file in it — remove it, then run this command again")
		}
	}
	return stop("the sync of the parent directory of " + Printable(g.rel(failed)) + " failed, so lo removed the directories that it made: lo wrote no file — run this command again")
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
// The name starts with Secret., so it is never .gitignore in any letter
// case (WritePlaintext refuses that name for a direct caller). set checks
// a key with it before it reads the value. A refusal names the field and
// its text, never the value.
func CheckName(dir, name, namespace, key string) (string, error) {
	for _, part := range []struct{ field, text string }{{"name", name}, {"namespace", namespace}, {"key", key}} {
		if why := PartProblem(part.text); why != "" {
			return "", stop(fmt.Sprintf("the %s '%s' is refused: %s. Nothing was written: use a %s without it", part.field, Printable(part.text), why, part.field))
		}
	}
	base := EntryName(name, namespace, key)
	if _, ok := Destination(dir, base); !ok {
		return "", stop(notInStore(base))
	}
	if ReservedName(base) {
		return "", stop(reservedRefusal("the key '"+Printable(key)+"'", base))
	}
	return base, nil
}

// EntryName is the name of the plaintext file of the key key of the Secret
// name in namespace, Secret.<name>.<namespace>.<key>. It is the one builder
// of the name: CheckName (the writers) and FormatName of the cache (the
// readers) both build it here. It checks nothing (CheckName does).
func EntryName(name, namespace, key string) string {
	return "Secret." + name + "." + namespace + "." + key
}

// reservedRefusal is the one text of the refusal of a plaintext file base
// whose name ends in .enc (ReservedName). subject is what is refused: the
// key of lo secrets set (CheckName, the text of the bash twin), or the
// plaintext write of a caller that has no key (WritePlaintext).
func reservedRefusal(subject, base string) string {
	return subject + " is refused: the plaintext file " + Printable(base) + " would end in .enc (in any letter case), and the .gitignore rule !Secret.*.enc lets git commit it. Nothing was written: use a key name that does not end in .enc"
}

// gitignoreName reports whether base is .gitignore in any ASCII letter
// case. A plaintext of that name would replace the rules of the store, and
// a file system that ignores the case of a name has one file for them all.
func gitignoreName(base string) bool { return asciiLower(base) == ".gitignore" }

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
//   - its name is .gitignore in any letter case;
//   - its name ends in .enc in any letter case (ReservedName);
//   - the path is not a regular file and not absent (DestinationOK);
//   - the .gitignore of p.Dir cannot protect the store, or its write fails
//     (EnsureGitignore).
//
// The callers check the name before (set and the cache with CheckName,
// decrypt with its own check of each entry), so a name refusal here comes
// from a direct caller, or from an entry that decrypt lists after its check.
// Then it writes a new private temp file in p.Dir and renames it onto the
// path (WriteReplacing), and syncs p.Dir. A write that fails leaves the old
// file as it was and returns a *Stop that names the path, and the temp file
// if its remove failed too. The .gitignore that EnsureGitignore wrote stays.
// A sync of p.Dir that fails after the rename returns a *Stop that says
// that the file was written. The caller makes p.Dir (MakeDir).
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
	if err := g.sync(p.Dir); err != nil {
		return stop(Printable(g.rel(path)) + " was written, but the sync of its directory failed, so a power loss can lose the new file")
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
// .gitignore and the file. It reads the files as they are: it cannot tell
// that the write will fail (a full disk, a change after the check).
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
	if gitignoreName(p.Base) {
		return "", "", stop("the plaintext write is refused: the plaintext file " + Printable(p.Base) + " would replace the .gitignore of the store (in any letter case), which keeps every plaintext out of git. Nothing was written: use the name of a store entry")
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
// After the rename it reads the file again: a file that is not a regular
// file, or whose bytes are not the bytes of the plan (bytes.Equal), is
// refused, so a FillGitignore that writes other bytes fails closed (also a
// file that the plan would keep as it is). Then it
// syncs dir, so that the .gitignore survives a power loss before the caller
// writes a plaintext. After the rename, wrote is true, also with an error:
// the error then says that the file was written.
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
	rel := Printable(g.rel(gitignore))
	if err := WriteReplacing(gitignore, 0o644, func(w io.Writer) error { return fill(w, want) }); err != nil {
		return false, g.FailedWrite(err, "cannot write "+rel+": it was not changed")
	}
	if !holds(gitignore, want) {
		return true, stop(rel + " was written, but it does not have the bytes that lo planned for it: lo stopped before " + step + " — fix the file, then run this command again")
	}
	if err := g.sync(dir); err != nil {
		return true, stop(rel + " was written, but the sync of its directory failed, so a power loss can lose it: lo stopped before " + step + " — run this command again")
	}
	return true, nil
}

// holds reports whether path is a regular file (DestinationOK) whose bytes
// are want. It reads at most one byte more than want.
func holds(path string, want []byte) bool {
	if !DestinationOK(path) {
		return false
	}
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	raw, err := io.ReadAll(io.LimitReader(f, int64(len(want))+1))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err == nil && bytes.Equal(raw, want)
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

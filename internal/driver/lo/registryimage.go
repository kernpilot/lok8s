package lo

// registryimage.go — the registry image is pulled explicitly before the
// driver creates a registry-set container (bash: lo::registry_image_ensure).
//
// `docker container create` and `docker run` pull a missing image
// implicitly. On that path docker prints "Unable to find image … locally"
// first and the pull error after it, and one transient network error fails
// the whole command. An explicit `docker pull` with a bounded retry rides
// out a short outage, and dockerErrSummary keeps the lines that name the
// cause.

import (
	"context"
	"fmt"
	"io"
	"regexp"
	"strings"

	"github.com/kernpilot/lok8s/internal/ui"
)

// registryPullBackoff is the wait in seconds before each retry of the
// registry image pull. The pull runs once, plus one retry for each entry.
var registryPullBackoff = []int{2, 5, 10}

// dockerErrLines is the number of stderr lines dockerErrSummary keeps.
const dockerErrLines = 3

// ensureRegistryImage makes sure RegistryImage is in the local image store
// (bash: lo::registry_image_ensure). When `docker image inspect` finds it,
// nothing is pulled. Otherwise `docker pull` runs, and a failed pull is
// retried after each registryPullBackoff wait. The result is kept for the
// life of the driver: a later call returns it and runs no docker command.
// A failure prints one error line with the last pull's cause; later calls
// print nothing more.
func (d *Driver) ensureRegistryImage(ctx context.Context, errOut io.Writer) error {
	if d.registryImage.done {
		return d.registryImage.err
	}
	err := d.pullRegistryImage(ctx, errOut)
	// A cancelled context is not a result: the next call tries again.
	if ctx.Err() == nil {
		d.registryImage.done, d.registryImage.err = true, err
	}
	return err
}

func (d *Driver) pullRegistryImage(ctx context.Context, errOut io.Writer) error {
	if d.runQuiet(ctx, "docker", "image", "inspect", "-f", "{{.Id}}", RegistryImage) == nil {
		return nil
	}
	attempts := len(registryPullBackoff) + 1
	ui.DebugTo(errOut, "registry image %s: not found locally, pulling", RegistryImage)
	var errText string
	for attempt := 1; attempt <= attempts; attempt++ {
		var err error
		if errText, err = d.errOutput(ctx, "docker", "pull", RegistryImage); err == nil {
			return nil
		}
		if attempt == attempts || ctx.Err() != nil {
			break
		}
		wait := registryPullBackoff[attempt-1]
		ui.WarnTo(errOut, "docker pull %s failed (attempt %d of %d): %s. Retrying in %ds.",
			RegistryImage, attempt, attempts, dockerErrSummary(errText), wait)
		if err := d.sleepSeconds(ctx, wait); err != nil {
			return err
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	fmt.Fprintf(errOut, "error: docker pull %s failed after %d attempts: %s\n",
		RegistryImage, attempts, dockerErrSummary(errText))
	return ui.Handled(fmt.Errorf("docker pull %s failed after %d attempts", RegistryImage, attempts))
}

// urlUserinfo matches the user:password@ part of a URL, up to the last @
// before the host: a password can hold a raw @.
var urlUserinfo = regexp.MustCompile(`([A-Za-z][A-Za-z0-9+.-]*://)[^/[:space:]]+@`)

// dockerErrSummary reduces a docker stderr to the lines that name the
// cause (bash: lo::docker_err_summary). Each line loses its trailing white
// space. It drops the empty lines, docker's implicit-pull notice ("Unable
// to find image … locally") and the "See/Run 'docker … --help'" hint, then
// keeps the last dockerErrLines lines, joined with " | ". The cause is at
// the end: on the implicit-pull path the notice comes first. The
// user:password@ part of a URL, up to the last @ before a / or a white
// space, is masked as ***@.
func dockerErrSummary(s string) string {
	var kept []string
	for line := range strings.SplitSeq(s, "\n") {
		line = strings.TrimRight(line, " \t\r\v\f")
		switch {
		case line == "",
			strings.HasPrefix(line, "Unable to find image "),
			(strings.HasPrefix(line, "See 'docker ") || strings.HasPrefix(line, "Run 'docker ")) && strings.Contains(line, "--help'"):
			continue
		}
		kept = append(kept, line)
	}
	if len(kept) > dockerErrLines {
		kept = kept[len(kept)-dockerErrLines:]
	}
	return urlUserinfo.ReplaceAllString(strings.Join(kept, " | "), "${1}***@")
}

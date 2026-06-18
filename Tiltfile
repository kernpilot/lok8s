# vim: filetype=python
#
# lok8s Tilt bootstrap
# ====================
# This is the project-root Tiltfile that `tilt up` discovers. It loads
# the lok8s Tilt extension from .lok8s/tilt/ — that's where all the
# real logic lives (services config, live-reload rules, hot-reload by
# runtime, etc.). Keep this file thin: a load + a single function call.
#
# To extend or override behavior, prefer:
#   - editing services.yaml (committed) for service definitions, or
#   - editing services.local.yaml (gitignored) for per-developer overrides
# rather than modifying this file or the extension directly.
#
# The extension itself (.lok8s/tilt/Tiltfile) is structured for a future
# upstream PR to github.com/tilt-dev/tilt-extensions, where it would live
# at `tilt-extensions/lok8s/Tiltfile` and consumers would `load('ext://lok8s', 'lok8s')`.
load('./.lok8s/tilt/Tiltfile', 'lok8s')

lok8s()

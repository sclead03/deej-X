## Developer scripts

This document lists the various scripts in the project and their purposes.

> Note: All scripts are meant to be run from the root of the repository, i.e. from the _root_ `deej` directory: `.\pkg\deej\scripts\...\whatever.bat`. They're not guaranteed to work correctly if run from another directory.

### Windows

- [`make-icon.bat`](./windows/make-icon.bat): Converts a .ico file to an icon byte array in a Go file. Used by our systray library. You shouldn't need to run this unless you change the deej logo
- [`make-rsrc.bat`](./windows/make-rsrc.bat): Generates a `rsrc.syso` resource file inside `cmd` alongside `main.go` - This indicates to the Go linker to use the deej application manifest and icon when building.

> The upstream `build-dev.bat`/`build-release.bat`/`build-all.bat`/`prepare-release.bat` (and their Linux `.sh` equivalents) have been removed — this fork builds with the manual command in the root `CLAUDE.md` ("Building deej-x.exe"). See that file's Remaining Work section for the plan to reintroduce a release/tagging pipeline.

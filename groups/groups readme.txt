**Usage:** Use the `groups/` directory next to `config.yaml`. Each file is a group:

groups/steam.yaml:
- steam.exe
- steamwebhelper.exe
- gameoverlayui.exe

groups/bananasplit.yaml:
- someapp.exe
- otherapp.exe

Then in config.yaml:
slider_mapping:
  3: deej.steam
  4: deej.bananasplit

**Icon:** Put `steam.png` / `bananasplit.png` in your `icon_dir` — the `deej.` prefix is stripped automatically, same as `deej.unmapped` → `unmapped.png`.

**Explicit-wins rule:** If `firefox.exe` is listed on an explicit slider AND appears in `groups/browsers.yaml`, it is excluded from the group's volume control on `deej.browsers`. It also counts as "mapped" for `deej.unmapped` exclusion purposes if it's in any group.

**Config reload:** Editing a group file alone does NOT trigger the config watcher (it only watches `config.yaml`). Touching `config.yaml` will reload both the config and all group files.
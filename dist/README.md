# Daily refresh with systemd --user

These example units run `imdb update` once a day as your own user. No system
service or root-owned database is required.

## Install

From the repository root, build the binary and install it wherever you prefer.
For example:

```sh
./build.sh
install -Dm755 imdb "$HOME/.local/bin/imdb"
```

Copy both units into your user configuration directory:

```sh
unit_dir="${XDG_CONFIG_HOME:-$HOME/.config}/systemd/user"
mkdir -p "$unit_dir"
cp dist/imdb-update.service dist/imdb-update.timer "$unit_dir/"
```

**Before enabling the timer**, edit the copied `imdb-update.service`:

- `ExecStart=` defaults to `%h/.local/bin/imdb`. Replace it with your binary's
  absolute path if installed elsewhere. `%h` expands to your home directory;
  systemd does not run this command through a shell, so do not use `~`.
- `-db` defaults to `%S/imdb/imdb.sqlite`. `%S` is the user state directory
  (`$XDG_STATE_HOME`, normally `~/.local/state`). `StateDirectory=imdb`
  creates the parent directory automatically. To refresh an existing catalog,
  set `-db` to its absolute path instead and ensure its parent directory exists
  and is writable, including for SQLite's WAL/SHM files. Remove
  `StateDirectory=imdb` if you no longer use that directory.
- The command allows two full-update attempts within the CLI's default
  **two-hour overall deadline**. Add, for example, `-timeout 6h` if needed.
  This is a full download/import, not an incremental refresh.

Enable the timer (not the service):

```sh
systemctl --user daemon-reload
systemctl --user enable --now imdb-update.timer
```

The timer runs after midnight in local time, with up to one hour of random
delay to spread download load. `Persistent=true` catches up once after missed
scheduled runs when the user manager starts again, subject to the same delay.
It does not retry failed runs immediately; transient failures use the CLI's
attempts, otherwise the next daily run tries again.

To create or refresh the catalog immediately, without waiting for the timer:

```sh
systemctl --user start --no-block imdb-update.service
```

The same service cannot overlap itself; avoid other writers to this database.
Point consuming applications at the configured database path.

## Check and manage

```sh
systemctl --user list-timers --all imdb-update.timer
systemctl --user status imdb-update.service
journalctl --user -u imdb-update.service -n 50 --no-pager
```

A successful oneshot service normally shows as inactive after it finishes.
JSON results and errors go to the journal. For ongoing progress there too,
add `-progress always` to `ExecStart=` (this produces more log output).
After editing installed units, run `systemctl --user daemon-reload`; after
changing the schedule, also run `systemctl --user restart imdb-update.timer`.

User services normally depend on your login session. To run after logout and
start the user manager at boot, optionally enable lingering:

```sh
loginctl enable-linger "$USER"
```

This may require administrator authorization. The machine must be running and
have network access; lingering does not wake a powered-off or suspended machine.

To stop future scheduled refreshes (leaves the database intact):

```sh
systemctl --user disable --now imdb-update.timer
```

This does not cancel an update already running. To cancel one, use
`systemctl --user stop imdb-update.service`.

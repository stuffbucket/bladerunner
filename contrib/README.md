# contrib

Material that supports the project without being part of the binary.

## assets/

Eight annotated screenshots of a real `br` session, in narrative order:

| # | File | Shows |
|---|---|---|
| 1 | `01-what-it-is.png` | What bladerunner is: an Incus VM on macOS on Virtualization.framework |
| 2 | `02-booting.png` | Staged, observable boot with a live guest-console tail |
| 3 | `03-vm-ready.png` | SSH and the Incus API forwarded over vsock; a holder owns the VM |
| 4 | `04-status.png` | What is running, which guest image, where its state lives |
| 5 | `05-shell.png` | A shell in the guest |
| 6 | `06-instances.png` | An Incus container running inside the VM, with its address |
| 7 | `07-exec.png` | Running commands in that container from the macOS shell |
| 8 | `08-stop.png` | Clean ACPI shutdown, disk synced before detach |

Use them in the README, the site, or a release. They are 30–92 KB each.

## screenshots/

The tooling that produces `assets/`. Update the images when the CLI output
changes. Without it they stay frozen at what the terminal showed on the day
they were taken.

```
./make-shots.sh        # caps/ -> build/ -> optimised PNGs in ../assets
python3 make-video.py  # build/ -> build/bladerunner-walkthrough.mp4
```

Needs `python3`, `rsvg-convert`, ImageMagick, and `ffmpeg` for the video.
Neither script touches a VM: both read only from `caps/`, so captions and crops
can be re-cut offline.

- `caps/` — PTY recordings from `script(1)`. These carry the real 256-colour
  output, which is why the screenshots show the CLI's own styling rather than a
  reconstruction.
- `render.py` — replays a recording and renders what the terminal was left
  showing, as an annotated SVG and then a PNG. The replay includes the
  cursor-up repaints that a live progress display performs.
- `build/` — intermediates. Untracked.

The walkthrough video is deliberately **not** committed. It is 4.3 MB, more than
twenty times the largest file in the repository, and git history is permanent.
Regenerate it with `make-video.py` and attach it to a release or the site.

## How honest the screenshots are

They come from a real session against a real VM. `br up` booted the pre-baked
guest image. An Alpine container was created through the forwarded Incus API.
It stayed after a full VM restart and returned with its address. `br exec` and
`br shell` ran commands in the guest and in the container.

Where a frame is cropped, `BR_ROWS` narrows the window onto rows the terminal
showed. Nothing is re-typed, re-staged, or hand-edited. One line is added: the
`$ br ...` prompt. It restates the command that produced the output, so a
reader does not infer it from the window title.

These frames were captured after #257. Before that correction, `br shell` gave
`Permission denied (publickey)` on a VM with a disk older than the dedicated
SSH user.

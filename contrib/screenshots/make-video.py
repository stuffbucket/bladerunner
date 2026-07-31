#!/usr/bin/env python3
"""Assemble the screenshots into a slow walkthrough video.

Pacing is the point. Each frame is held long enough to read the caption and the
output under it — denser frames get longer — and the crossfades are deliberately
unhurried, because the alternative is a montage nobody can follow.
"""

import pathlib
import re
import subprocess
import sys

CANVAS_W, CANVAS_H = 2240, 1400
BACKGROUND = "#0b0d12"
FADE = 1.0

# Seconds to hold each frame, by how much there is to take in.
FRAMES = [
    ("01-what-it-is.png", 8.0),
    ("02-booting.png", 10.0),
    ("03-vm-ready.png", 9.0),
    ("04-status.png", 10.0),
    ("05-shell.png", 7.0),
    ("06-instances.png", 7.0),
    ("07-exec.png", 8.0),
    ("08-stop.png", 6.0),
]

HERE = pathlib.Path(__file__).parent
SHOTS = HERE / "build"
STAGE = HERE / "build" / "video-stage"
OUT = HERE / "build" / "bladerunner-walkthrough.mp4"


def normalise():
    """Fit every screenshot onto one canvas at native resolution.

    xfade requires identical dimensions, and the cards differ in both. Rather
    than upscale the PNGs — which would soften exactly the text the video is
    meant to be read from — each card is re-rendered from its SVG at the zoom
    that fills the frame, so the glyphs are drawn at final size.
    """
    STAGE.mkdir(parents=True, exist_ok=True)
    staged = []
    for name, _ in FRAMES:
        svg = SHOTS / name.replace(".png", ".svg")
        if not svg.exists():
            sys.exit(f"missing SVG for {name}; run ./make-shots.sh first")

        head = svg.read_text()[:400]
        w = float(re.search(r'width="(\d+)"', head).group(1))
        h = float(re.search(r'height="(\d+)"', head).group(1))
        zoom = min(CANVAS_W * 0.92 / w, CANVAS_H * 0.88 / h)

        native = STAGE / f"native-{name}"
        subprocess.run(
            ["rsvg-convert", "-z", f"{zoom:.4f}", "-o", str(native), str(svg)],
            check=True,
        )

        dst = STAGE / name
        subprocess.run([
            "magick", str(native),
            "-background", BACKGROUND,
            "-gravity", "center",
            "-extent", f"{CANVAS_W}x{CANVAS_H}",
            str(dst),
        ], check=True)
        staged.append(dst)
    return staged


def build(staged):
    """Chain the stills with crossfades and encode."""
    cmd = ["ffmpeg", "-y"]
    for path, (_, hold) in zip(staged, FRAMES):
        # Each still is held for its own duration plus the fade it overlaps
        # into, so the readable time is the hold, not hold-minus-fade.
        cmd += ["-loop", "1", "-t", f"{hold + FADE:.2f}", "-i", str(path)]

    steps, prev, offset = [], "0:v", 0.0
    for i in range(1, len(staged)):
        offset += FRAMES[i - 1][1]
        label = f"v{i}"
        steps.append(
            f"[{prev}][{i}:v]xfade=transition=fade:duration={FADE}:offset={offset:.2f}[{label}]"
        )
        prev = label

    # format=yuv420p keeps the file playable in browsers and QuickTime, which
    # is where a walkthrough actually gets watched.
    filter_chain = ";".join(steps) + f";[{prev}]format=yuv420p[out]" if steps else "[0:v]format=yuv420p[out]"

    cmd += [
        "-filter_complex", filter_chain,
        "-map", "[out]",
        "-r", "30",
        "-c:v", "libx264",
        "-preset", "slow",
        "-crf", "20",
        "-movflags", "+faststart",
        str(OUT),
    ]
    subprocess.run(cmd, check=True, capture_output=True)


def main():
    OUT.parent.mkdir(parents=True, exist_ok=True)
    staged = normalise()
    build(staged)

    total = sum(hold for _, hold in FRAMES) + FADE
    size = OUT.stat().st_size / 1_000_000
    print(f"{OUT}  ~{total:.0f}s  {size:.1f} MB  {CANVAS_W}x{CANVAS_H}")
    return 0


if __name__ == "__main__":
    sys.exit(main())

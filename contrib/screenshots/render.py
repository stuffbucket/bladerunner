#!/usr/bin/env python3
"""Render a captured terminal session to an annotated PNG screenshot.

The capture comes from script(1), so it is a real PTY recording: it carries the
lipgloss 256-colour output exactly as a user sees it, plus the control noise a
terminal would have consumed. That noise is stripped here rather than at capture
time, so the raw file stays a faithful recording.

ANSI is parsed into styled spans and emitted as SVG, which rsvg-convert turns
into a PNG. Going through SVG rather than drawing text directly keeps the glyphs
vector-crisp at any output scale.
"""

import html
import os
import re
import subprocess
import sys

# Terminal geometry. The cell size is what makes the SVG line up as a grid.
CHAR_W = 8.4
LINE_H = 19.0
FONT_SIZE = 14
FONT = "Menlo, DejaVu Sans Mono, Liberation Mono, monospace"

# Card chrome.
PAD_X = 22
BODY_TOP_PAD = 20
TITLE_H = 38
CAPTION_H = 54
RADIUS = 12

BG = "#12141a"
TITLEBAR = "#1c1f27"
CAPTION_BG = "#181b23"
CAPTION_FG = "#aab2c5"
TITLE_FG = "#dfe4ee"
DEFAULT_FG = "#e6e9ef"

# Control sequences a real terminal answers or acts on, none of which are text:
# OSC colour queries, cursor-position reports, bracketed paste, cursor moves.
NOISE = re.compile(
    r"\x1b\][0-9]*;[^\x07\x1b]*(?:\x07|\x1b\\)"  # OSC ... BEL/ST
    r"|\x1b\[\??[0-9;]*[a-zA-Z]"                  # CSI, filtered below for SGR
    r"|\x1b[=>]"
    r"|\r"
    r"|\x04"
)
SGR = re.compile(r"\x1b\[([0-9;]*)m")
# Everything below space except newline, tab, and ESC, plus DEL. ESC is excluded
# because the SGR sequences kept above still need their introducer; stripping it
# would turn every colour code into literal text.
CONTROL = re.compile(r"[\x00-\x08\x0b-\x1a\x1c-\x1f\x7f]")


def xterm256():
    """The xterm 256-colour palette, which is what lipgloss emits."""
    base = [
        "#000000", "#cd3131", "#0dbc79", "#e5e510", "#2472c8", "#bc3fbc", "#11a8cd", "#e5e5e5",
        "#666666", "#f14c4c", "#23d18b", "#f5f543", "#3b8eea", "#d670d6", "#29b8db", "#f5f5f5",
    ]
    levels = [0, 95, 135, 175, 215, 255]
    for r in levels:
        for g in levels:
            for b in levels:
                base.append(f"#{r:02x}{g:02x}{b:02x}")
    for i in range(24):
        v = 8 + i * 10
        base.append(f"#{v:02x}{v:02x}{v:02x}")
    return base


PALETTE = xterm256()


class Style:
    """The active SGR state."""

    def __init__(self):
        self.fg = None
        self.bg = None
        self.bold = False

    def copy(self):
        s = Style()
        s.fg, s.bg, s.bold = self.fg, self.bg, self.bold
        return s

    def key(self):
        return (self.fg, self.bg, self.bold)


def apply_sgr(style, params):
    """Update style for one SGR escape."""
    codes = [int(p) for p in params.split(";") if p != ""] or [0]
    i = 0
    while i < len(codes):
        c = codes[i]
        if c == 0:
            style.fg = style.bg = None
            style.bold = False
        elif c == 1:
            style.bold = True
        elif c == 22:
            style.bold = False
        elif c == 39:
            style.fg = None
        elif c == 49:
            style.bg = None
        elif 30 <= c <= 37:
            style.fg = PALETTE[c - 30]
        elif 90 <= c <= 97:
            style.fg = PALETTE[c - 90 + 8]
        elif 40 <= c <= 47:
            style.bg = PALETTE[c - 40]
        elif 100 <= c <= 107:
            style.bg = PALETTE[c - 100 + 8]
        elif c in (38, 48) and i + 2 < len(codes) and codes[i + 1] == 5:
            colour = PALETTE[codes[i + 2] % 256]
            if c == 38:
                style.fg = colour
            else:
                style.bg = colour
            i += 2
        elif c in (38, 48) and i + 4 < len(codes) and codes[i + 1] == 2:
            r, g, b = codes[i + 2], codes[i + 3], codes[i + 4]
            colour = f"#{r:02x}{g:02x}{b:02x}"
            if c == 38:
                style.fg = colour
            else:
                style.bg = colour
            i += 4
        i += 1
    return style


CSI = re.compile(r"\x1b\[(\??)([0-9;]*)([a-zA-Z])")


def render_screen(raw):
    """Replay a PTY recording and return what the terminal was left showing.

    A live progress display does not append — it moves the cursor up and
    repaints. Treating the recording as a transcript would stack every repaint
    on top of the last, so the sequences that move and erase are replayed
    against a line buffer instead. Columns are not tracked: every erase clears
    whole lines, which is what these displays actually do.
    """
    rows, cur, pos = [""], 0, 0

    def ensure(n):
        while len(rows) <= n:
            rows.append("")

    while pos < len(raw):
        m = CSI.search(raw, pos)
        chunk = raw[pos:m.start()] if m else raw[pos:]

        for j, part in enumerate(chunk.split("\n")):
            if j > 0:
                cur += 1
                ensure(cur)
                rows[cur] = ""
            if "\r" in part:
                part = part.split("\r")[-1]
                rows[cur] = ""
            rows[cur] += part

        if m is None:
            break
        pos = m.end()

        params, final = m.group(2), m.group(3)
        n = int(params.split(";")[0]) if params.split(";")[0].isdigit() else 1

        if final == "m":
            rows[cur] += m.group(0)  # Styling is text as far as the buffer cares.
        elif final == "A":
            cur = max(0, cur - n)
        elif final == "B":
            cur += n
            ensure(cur)
        elif final == "J":
            del rows[cur + 1:]
            rows[cur] = ""
        elif final == "K":
            rows[cur] = ""

    # A repaint that advances the cursor one row further than it later moves
    # back leaves one copy of its top line behind per frame. Rather than model
    # the exact off-by-one, adjacent identical rows are collapsed: a terminal
    # display that legitimately repeats a line verbatim, immediately, does not
    # occur in this output, and the stack of leftovers is pure noise.
    collapsed = []
    for row in rows:
        if collapsed and row == collapsed[-1] and row.strip():
            continue
        collapsed.append(row)

    return "\n".join(collapsed)


def parse(text):
    """Turn an ANSI string into a list of lines, each a list of (style, text)."""
    lines, current, style = [], [], Style()
    pos = 0
    while pos < len(text):
        m = SGR.search(text, pos)
        if m is None:
            segment, pos = text[pos:], len(text)
        else:
            segment, pos = text[pos:m.start()], m.end()

        for j, part in enumerate(segment.split("\n")):
            if j > 0:
                lines.append(current)
                current = []
            if part:
                current.append((style.copy(), part))

        if m is not None:
            style = apply_sgr(style, m.group(1))

    lines.append(current)
    return lines


def strip_noise(text):
    """Remove control sequences that are not SGR, then any stray control bytes.

    A PTY recording keeps bytes a terminal would have acted on rather than
    displayed — backspaces from spinner erasure, bells, form feeds. They are not
    text, and XML rejects most of them outright, so they go after the escape
    sequences have been handled.
    """
    def keep_sgr(m):
        return m.group(0) if m.group(0).endswith("m") and m.group(0).startswith("\x1b[") else ""

    text = NOISE.sub(keep_sgr, text)
    return CONTROL.sub("", text)


def to_svg(lines, title, caption, width_cols):
    body_h = len(lines) * LINE_H + BODY_TOP_PAD * 2
    w = int(width_cols * CHAR_W + PAD_X * 2)
    h = int(TITLE_H + CAPTION_H + body_h)

    parts = [
        f'<svg xmlns="http://www.w3.org/2000/svg" width="{w}" height="{h}" viewBox="0 0 {w} {h}">',
        f'<rect x="0" y="0" width="{w}" height="{h}" rx="{RADIUS}" fill="{BG}"/>',
        f'<path d="M0,{RADIUS} a{RADIUS},{RADIUS} 0 0 1 {RADIUS},-{RADIUS} h{w - 2 * RADIUS} '
        f'a{RADIUS},{RADIUS} 0 0 1 {RADIUS},{RADIUS} v{TITLE_H - RADIUS} h-{w} z" fill="{TITLEBAR}"/>',
    ]
    for i, colour in enumerate(("#ff5f57", "#febc2e", "#28c840")):
        parts.append(f'<circle cx="{20 + i * 19}" cy="{TITLE_H / 2}" r="6" fill="{colour}"/>')

    parts.append(
        f'<text x="{w / 2}" y="{TITLE_H / 2 + 5}" font-family="{FONT}" font-size="13" '
        f'fill="{TITLE_FG}" text-anchor="middle">{html.escape(title)}</text>'
    )

    parts.append(
        f'<rect x="0" y="{TITLE_H}" width="{w}" height="{CAPTION_H}" fill="{CAPTION_BG}"/>'
    )
    for i, line in enumerate(caption):
        parts.append(
            f'<text x="{PAD_X}" y="{TITLE_H + 22 + i * 20}" font-family="{FONT}" '
            f'font-size="13" fill="{CAPTION_FG}">{html.escape(line)}</text>'
        )

    y0 = TITLE_H + CAPTION_H + BODY_TOP_PAD
    for row, spans in enumerate(lines):
        y = y0 + row * LINE_H
        col = 0
        for style, text in spans:
            if style.bg:
                parts.append(
                    f'<rect x="{PAD_X + col * CHAR_W:.1f}" y="{y - 13:.1f}" '
                    f'width="{len(text) * CHAR_W:.1f}" height="{LINE_H:.1f}" fill="{style.bg}"/>'
                )
            col += len(text)

        col = 0
        for style, text in spans:
            fill = style.fg or DEFAULT_FG
            weight = ' font-weight="bold"' if style.bold else ""
            parts.append(
                f'<text x="{PAD_X + col * CHAR_W:.1f}" y="{y:.1f}" font-family="{FONT}" '
                f'font-size="{FONT_SIZE}" fill="{fill}"{weight} '
                f'xml:space="preserve">{html.escape(text)}</text>'
            )
            col += len(text)

    parts.append("</svg>")
    return "\n".join(parts)


def main():
    if len(sys.argv) < 5:
        print("usage: render.py <capture> <out.png> <title> <caption...>", file=sys.stderr)
        return 2

    capture, out_png, title = sys.argv[1], sys.argv[2], sys.argv[3]
    caption = sys.argv[4:]

    with open(capture, "r", encoding="utf-8", errors="replace") as f:
        raw = f.read()

    # An intermediate frame is a real moment from the same run: truncating the
    # recording shows what the terminal displayed part-way through, which is how
    # a progress display can be captured without re-running a slow command.
    limit = os.environ.get("BR_FRAME_BYTES")
    if limit:
        raw = raw[:int(limit)]

    text = strip_noise(render_screen(raw))
    # script(1) echoes the EOF it sends to close the session. It is an artefact
    # of the recorder, not something the command printed.
    text = re.sub(r"^\^D", "", text)
    lines = parse(text)

    # Trim leading and trailing blank rows so the card hugs its content.
    while lines and not any(t.strip() for _, t in lines[0]):
        lines.pop(0)
    while lines and not any(t.strip() for _, t in lines[-1]):
        lines.pop()

    # A long capture can be cropped to the part a given screenshot is about.
    # The rows are still exactly what the terminal showed; only the window
    # onto them changes.
    sl = os.environ.get("BR_ROWS")
    if sl:
        a, _, b = sl.partition(":")
        lines = lines[int(a) if a else None:int(b) if b else None]

    # The command that produced the output. A reader should not have to infer
    # it from the window title.
    cmd = os.environ.get("BR_CMD")
    if cmd:
        prompt = Style()
        prompt.fg = "#5ec27a"
        typed = Style()
        typed.fg = "#e6e9ef"
        typed.bold = True
        lines = [[(prompt, "$ "), (typed, cmd)], []] + lines

    width = max((sum(len(t) for _, t in row) for row in lines), default=40)
    width = max(width, max(len(c) for c in caption) + 2, len(title) + 4)

    svg = to_svg(lines, title, caption, width)
    svg_path = out_png.replace(".png", ".svg")
    with open(svg_path, "w", encoding="utf-8") as f:
        f.write(svg)

    subprocess.run(
        ["rsvg-convert", "-z", "2", "-o", out_png, svg_path],
        check=True,
    )
    print(f"{out_png}  ({width} cols x {len(lines)} rows)")
    return 0


if __name__ == "__main__":
    sys.exit(main())

#!/usr/bin/env python3
"""Generate the GitHub social-preview card for each repository in the Hippocampus family.

WHY THIS EXISTS, and why it is a script rather than six hand-made images.

GitHub falls back to an auto-generated card when a repository has no social preview set, and that
card is the repository NAME over the owner's avatar - so six sibling repositories with names that
all begin `hippocampus-` produced six previews that were, at the size a link preview actually
renders, indistinguishable from one another and from the service itself. The point of a family of
cards is that a reader can tell WHICH repository a link goes to without reading the slug, which is
why every card here carries a human title rather than the repository name, and why the slug is
relegated to the footer.

The mark is `docs/go-hippocampus.png` from this repository, unmodified - it is what makes the set
read as one family, and re-drawing it per repository is exactly how a family stops matching. The
palette is the demo site's (`hippocampus-demo-site/assets/styles.css`), for the same reason: it is
the project's actual public face, so the cards match the site a reader lands on.

THE ONE THING TO KNOW BEFORE CHANGING THE LAYOUT: GitHub does not display this image at 1280x640.
It is cropped to roughly 1.91:1 by the platforms that consume the Open Graph tag and shown as small
as ~400px wide in a chat client, so the title has to survive being read at a third of its rendered
size. That is the constraint behind the large type, the short titles, and the 80px margins - there
is deliberately much more empty space here than a poster would want.

Setting the image is NOT scriptable: GitHub exposes no REST or GraphQL field for a repository's
social preview, so the PNGs this writes are committed to each repository (`.github/social-preview.png`)
and uploaded by hand under Settings -> General -> Social preview. Committing them is what makes the
upload reproducible and the source of a card reviewable.

    python3 scripts/social-preview.py [--out-root ~/src] [--repo NAME ...]

Requires Pillow. Writes nothing unless it can render every card.
"""

import argparse
import os
import sys

try:
    from PIL import Image, ImageDraw, ImageFilter, ImageFont
except ImportError:  # Pillow is the one dependency, and it is not in this repository's toolchain.
    raise SystemExit(
        "this script needs Pillow, which nothing else here does - install it somewhere disposable:\n"
        "    python3 -m venv /tmp/social && /tmp/social/bin/pip install Pillow\n"
        "    /tmp/social/bin/python scripts/social-preview.py"
    )

WIDTH, HEIGHT = 1280, 640
MARGIN = 64

# The demo site's dark palette (hippocampus-demo-site/assets/styles.css).
BG = (12, 13, 18)
SURFACE = (18, 20, 28)
ACCENT = (124, 108, 240)
TEXT = (232, 234, 242)
MUTED = (162, 167, 186)
FAINT = (117, 123, 144)

# Titles are human names, not repository slugs - see the module docstring.
CARDS = {
    "hippocampus": (
        "Memory that forgets",
        "A finite memory store: significance, recall reinforcement, and a sleep cycle "
        "that sheds what stopped mattering.",
    ),
    "hippocampus-demo-site": (
        "Demo & showcase",
        "The public demo - the landing page, and the runnable showcase stacks behind it.",
    ),
    "hippocampus-gen": (
        "Test data generators",
        "Fills an instance with realistic events and memories, so a decay cycle has "
        "something to decide about.",
    ),
    "hippocampus-llamaindex": (
        "LlamaIndex memory",
        "A LlamaIndex long-term memory block backed by Hippocampus.",
    ),
    "hippocampus-obsidian": (
        "Obsidian plugin",
        "Your vault as a bounded, self-consolidating memory layer.",
    ),
    "hippocampus-otel-collector": (
        "OpenTelemetry exporter",
        "Writes each log record into Hippocampus as a memory - and the collector "
        "distribution that ships it.",
    ),
    "homebrew-tap": (
        "Homebrew tap",
        "brew install fastbean-au/tap/hippocampus - the service, the CLI and the MCP bridge.",
    ),
}

FONT_DIRS = ("/System/Library/Fonts", "/System/Library/Fonts/Supplemental", "/Library/Fonts")


def _font(names, size):
    """First of `names` that resolves, at `size`. Raises when none of them does."""
    for name in names:
        for d in FONT_DIRS:
            path = os.path.join(d, name)
            if not os.path.exists(path):
                continue

            try:
                return ImageFont.truetype(path, size)
            except OSError:
                continue

    raise SystemExit("none of these fonts resolved: %s" % ", ".join(names))


def _wrap(draw, text, font, max_width):
    """Greedy wrap of `text` to `max_width` pixels, measured in `font`."""
    words, lines, line = text.split(), [], ""

    for word in words:
        candidate = (line + " " + word).strip()
        if draw.textlength(candidate, font=font) <= max_width or not line:
            line = candidate

            continue

        lines.append(line)
        line = word

    if line:
        lines.append(line)

    return lines


def render(repo, title, subtitle, mark, out_path):
    card = Image.new("RGB", (WIDTH, HEIGHT), BG)
    draw = ImageDraw.Draw(card)

    # The mark, right-aligned and vertically centred.
    mark_h = 300
    mark_w = round(mark.width * mark_h / mark.height)
    mark_x = WIDTH - 50 - mark_w
    scaled = mark.resize((mark_w, mark_h), Image.LANCZOS)

    # A soft accent glow behind it. One ellipse under a heavy blur, NOT a stack of concentric
    # translucent ones - the stack leaves a visible arc where its outermost ring ends, which at
    # this contrast reads as a rendering fault rather than as lighting.
    glow = Image.new("RGBA", (WIDTH, HEIGHT), (0, 0, 0, 0))
    cx, cy = mark_x + mark_w // 2, HEIGHT // 2
    rx, ry = mark_w // 2, mark_h // 2
    ImageDraw.Draw(glow).ellipse((cx - rx, cy - ry, cx + rx, cy + ry), fill=ACCENT + (46,))
    glow = glow.filter(ImageFilter.GaussianBlur(120))

    card = Image.alpha_composite(card.convert("RGBA"), glow).convert("RGB")
    draw = ImageDraw.Draw(card)
    card.paste(scaled, (mark_x, (HEIGHT - mark_h) // 2), scaled)

    column = mark_x - 36 - MARGIN

    eyebrow_font = _font(("HelveticaNeue.ttc", "Arial Bold.ttf"), 24)
    title_font = _font(("HelveticaNeue.ttc", "Arial Bold.ttf"), 58)
    sub_font = _font(("HelveticaNeue.ttc", "Arial.ttf"), 27)
    foot_font = _font(("Menlo.ttc", "Courier New.ttf"), 22)

    title_lines = _wrap(draw, title, title_font, column)
    sub_lines = _wrap(draw, subtitle, sub_font, column)

    # Vertically centre the block rather than anchoring it to a fixed top. Titles run to one line
    # or two and subtitles to one, two or three, so a fixed top leaves the short cards visibly
    # bottom-heavy - which, across a set meant to be read as one family, is the thing that shows.
    block = 54 + 70 * len(title_lines) + 48 + 38 * len(sub_lines)
    y = (HEIGHT - block) // 2 - 14

    # Eyebrow: letterspaced by hand, since PIL has no tracking control.
    x = MARGIN
    for ch in "HIPPOCAMPUS":
        draw.text((x, y), ch, font=eyebrow_font, fill=ACCENT)
        x += draw.textlength(ch, font=eyebrow_font) + 4

    y += 54

    for line in title_lines:
        draw.text((MARGIN, y), line, font=title_font, fill=TEXT)
        y += 70

    y += 14
    draw.line((MARGIN, y, MARGIN + 72, y), fill=ACCENT, width=4)
    y += 30

    for line in sub_lines:
        draw.text((MARGIN, y), line, font=sub_font, fill=MUTED)
        y += 38

    draw.text(
        (MARGIN, HEIGHT - MARGIN - 26),
        "github.com/fastbean-au/%s" % repo,
        font=foot_font,
        fill=FAINT,
    )

    card.save(out_path, "PNG", optimize=True)

    return out_path


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument(
        "--out-root",
        default=os.path.join(os.path.expanduser("~"), "src"),
        help="directory holding the sibling clones (default: ~/src)",
    )
    parser.add_argument(
        "--repo",
        action="append",
        choices=sorted(CARDS),
        help="render only these repositories (default: all)",
    )
    parser.add_argument(
        "--mark",
        default=os.path.join(os.path.dirname(os.path.abspath(__file__)), "..", "docs", "go-hippocampus.png"),
        help="the family mark to composite",
    )
    args = parser.parse_args()

    mark = Image.open(args.mark).convert("RGBA")
    repos = args.repo or sorted(CARDS)

    for repo in repos:
        target = os.path.join(args.out_root, repo, ".github")
        if not os.path.isdir(os.path.dirname(target)):
            print("skipping %s: no clone at %s" % (repo, os.path.dirname(target)), file=sys.stderr)

            continue

        os.makedirs(target, exist_ok=True)
        title, subtitle = CARDS[repo]
        path = render(repo, title, subtitle, mark, os.path.join(target, "social-preview.png"))
        print("%-30s %s" % (repo, path))


if __name__ == "__main__":
    main()

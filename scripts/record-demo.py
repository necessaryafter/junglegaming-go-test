#!/usr/bin/env python3
"""Capture real demo stdout and render it as a terminal GIF (requires Pillow)."""

import argparse
import datetime
import json
from pathlib import Path
import subprocess
import sys
import time


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--output', type=Path, default=Path('docs/assets/demo.gif'))
    parser.add_argument('--font', type=Path, required=True, help='monospace TrueType font')
    args = parser.parse_args()
    from PIL import Image, ImageDraw, ImageFont

    root = Path(__file__).resolve().parent.parent
    command = [sys.executable, '-u', str(root / 'scripts/concurrency.py'), '--duplicates', '1000', '--concurrency', '16']
    captured = []
    started = time.monotonic()
    started_at = datetime.datetime.now(datetime.timezone.utc).isoformat()
    process = subprocess.Popen(command, cwd=root, stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True)
    for line in process.stdout:
        elapsed = time.monotonic() - started
        captured.append({'seconds': elapsed, 'line': line.rstrip()})
        print(line, end='', flush=True)
    code = process.wait()
    require_success = code == 0 and any(item['line'].startswith('PASS: both wallets reconcile') for item in captured)
    if not require_success:
        raise RuntimeError('demo failed; GIF was not generated')

    args.output.parent.mkdir(parents=True, exist_ok=True)
    transcript = {'started_at': started_at, 'duration_seconds': time.monotonic() - started,
                  'command': 'python3 scripts/concurrency.py --duplicates 1000 --concurrency 16',
                  'exit_code': code, 'rendering': 'real stdout; original timing; final frame held for 5 seconds',
                  'events': captured}
    args.output.with_suffix('.json').write_text(json.dumps(transcript, indent=2) + '\n')
    font = ImageFont.truetype(str(args.font), 18)
    title_font = ImageFont.truetype(str(args.font), 23)
    width, height = 1140, 670
    duration = transcript['duration_seconds']
    step = .125
    frames = []
    positions = [index * step for index in range(int(duration / step) + 2)]
    for position in positions:
        frame = Image.new('RGB', (width, height), '#0c1420')
        draw = ImageDraw.Draw(frame)
        draw.rounded_rectangle((18, 18, width - 18, height - 18), radius=14, fill='#111d2d', outline='#2c4058')
        for index, color in enumerate(['#f86d70', '#f3c969', '#65dbaa']):
            draw.ellipse((39 + index * 25, 38, 51 + index * 25, 50), fill=color)
        draw.text((135, 31), 'JUNGLE  /  REAL HTTP DEMO', font=title_font, fill='#dce7f5')
        draw.text((39, 81), '$ ' + transcript['command'], font=font, fill='#9ac7ff')
        visible = [item['line'] for item in captured if item['seconds'] <= position]
        # Test wallet UUIDs are retained in the transcript; keep the view readable.
        visible = [line for line in visible if not line.startswith('Created test wallets:')]
        for index, line in enumerate(visible[-17:]):
            color = '#65dbaa' if line.startswith(('PASS:', 'READY:', 'RECONCILIATION:')) else '#dce7f5'
            draw.text((39, 123 + index * 27), line, font=font, fill=color)
        draw.text((39, height - 58), '1,000 submissions / 16 concurrent clients / 3 processes', font=font, fill='#8499b5')
        frames.append(frame)
    delays = [125] * len(frames)
    delays[-1] = 5000
    frames[0].save(args.output, save_all=True, append_images=frames[1:], duration=delays, loop=0, optimize=True)
    print('GIF: ' + str(args.output), flush=True)


if __name__ == '__main__':
    main()

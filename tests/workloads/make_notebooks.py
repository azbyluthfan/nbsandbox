# Copyright 2026 The nbsandbox Authors
# SPDX-License-Identifier: Apache-2.0
"""Generates the workload notebooks in this directory.

The notebooks are the source of truth for tests; this script only makes them
easy to review and regenerate. Run inside the runner image (it has nbformat):

    make workloads
"""

import os

import nbformat
from nbformat.v4 import new_code_cell, new_notebook

HERE = os.path.dirname(os.path.abspath(__file__))


def params_cell(src):
    cell = new_code_cell(src)
    cell.metadata["tags"] = ["parameters"]
    return cell


NOTEBOOKS = {
    # Happy path: parameters, pandas, matplotlib, an artifact.
    "smoke.ipynb": [
        params_cell("region = 'default'\nmonths = 3"),
        new_code_cell("import os, pandas as pd\n"
                      "df = pd.DataFrame({'month': range(1, months + 1), 'value': [m * 10 for m in range(1, months + 1)]})\n"
                      "print('region:', region)\nprint(df)"),
        new_code_cell("import matplotlib\nmatplotlib.use('Agg')\nimport matplotlib.pyplot as plt\n"
                      "out = os.environ['NBSANDBOX_OUTPUT_DIR']\n"
                      "df.plot(x='month', y='value').get_figure().savefig(os.path.join(out, 'chart.png'))\n"
                      "df.to_csv(os.path.join(out, 'summary.csv'), index=False)\nprint('artifacts written')"),
    ],
    # One line per second for 10 s; lines must arrive while the cell runs.
    "stream.ipynb": [
        new_code_cell("import time\nfor i in range(10):\n    print(f'line {i}', flush=True)\n    time.sleep(1)"),
    ],
    # An exception in cell 3 (0-based, counting the user's cells).
    "fail_cell3.ipynb": [
        params_cell("x = 1"),
        new_code_cell("print('cell 1')"),
        new_code_cell("print('cell 2')"),
        new_code_cell("raise ValueError('boom in cell 3')"),
        new_code_cell("print('never runs')"),
    ],
    # Cancel and partial output: cells finish one by one, then a long cell.
    "long.ipynb": [
        new_code_cell("print('cell 0 done')"),
        new_code_cell("print('cell 1 done')"),
        new_code_cell("import time\nfor i in range(120):\n    print(f'tick {i}', flush=True)\n    time.sleep(1)"),
    ],
    # Allocate well past the memory limit, touching every page.
    "oom.ipynb": [
        new_code_cell("import numpy as np\nblocks = []\nfor i in range(64):\n"
                      "    blocks.append(np.ones(64 * 1024 * 1024, dtype=np.uint8))  # 64 MiB, touched\n"
                      "    print(f'allocated {(i + 1) * 64} MiB', flush=True)"),
    ],
    # Start processes until the pids limit refuses.
    "pids.ipynb": [
        new_code_cell("import subprocess\nprocs = []\nwhile True:\n"
                      "    procs.append(subprocess.Popen(['sleep', '300']))\n"
                      "    if len(procs) % 50 == 0:\n        print(len(procs), 'processes', flush=True)"),
    ],
    # Fill /tmp until it is full.
    "tmpfill.ipynb": [
        new_code_cell("chunk = b'x' * (8 * 1024 * 1024)\nwith open('/tmp/fill', 'wb') as f:\n"
                      "    for i in range(10_000):\n        f.write(chunk)\n        f.flush()"),
    ],
    # Parameters + an uploaded CSV in, a summary CSV artifact out.
    "f1_params_csv.ipynb": [
        params_cell("region = 'all'\nmonths = 12"),
        new_code_cell("import os, pandas as pd\n"
                      "df = pd.read_csv('data/sales.csv')\n"
                      "if region != 'all':\n    df = df[df['region'] == region]\n"
                      "df = df[df['month'] <= months]\n"
                      "summary = df.groupby('region', as_index=False)['value'].sum()\n"
                      "summary.to_csv(os.path.join(os.environ['NBSANDBOX_OUTPUT_DIR'], 'summary.csv'), index=False)\n"
                      "print(f'region={region} months={months} rows={len(df)}')\nprint(summary.to_string(index=False))"),
    ],
    # The notebook succeeds but HTML rendering fails (a folder sits where
    # output.html would be written).
    "render_fail.ipynb": [
        new_code_cell("import os\n"
                      "os.makedirs(os.path.join(os.environ['NBSANDBOX_OUTPUT_DIR'], '..', 'output.html'), exist_ok=True)\n"
                      "print('blocked output.html')"),
    ],
    # A burst of 20,000 lines of 200 bytes (4 MB) in a few seconds, to
    # fill a slow viewer's buffers.
    "burst.ipynb": [
        new_code_cell("pad = 'x' * 190\nfor i in range(1, 20001):\n    print(f'{i:06d} {pad}')"),
    ],
    # A fixed amount of single-threaded CPU work (the fairness baseline).
    "cpu_fixed.ipynb": [
        params_cell("iterations = 30_000_000"),
        new_code_cell("import time\nstart = time.process_time()\nt0 = time.time()\nacc = 0\n"
                      "for i in range(iterations):\n    acc = (acc + i * i) % 1_000_003\n"
                      "print(f'cpu_fixed wall={time.time() - t0:.3f}s cpu={time.process_time() - start:.3f}s acc={acc}')"),
    ],
    # Try to use every core for `seconds`.
    "cpu_hog.ipynb": [
        params_cell("seconds = 40"),
        new_code_cell("import multiprocessing as mp, os, time\n"
                      "def spin(deadline):\n    x = 0\n    while time.time() < deadline:\n        x += 1\n"
                      "n = (os.cpu_count() or 2) * 2\ndeadline = time.time() + seconds\n"
                      "ps = [mp.Process(target=spin, args=(deadline,)) for _ in range(n)]\n"
                      "[p.start() for p in ps]\n[p.join() for p in ps]\nprint(f'hogged with {n} processes')"),
    ],
    # A cell that never ends (after one that does, for partial output).
    "infinite.ipynb": [
        new_code_cell("print('before the loop', flush=True)"),
        new_code_cell("while True:\n    pass"),
    ],
    # Print 200-byte lines as fast as possible for `seconds`.
    "endless_print.ipynb": [
        params_cell("seconds = 25"),
        new_code_cell("import time\npad = 'x' * 190\ni = 0\nend = time.time() + seconds\n"
                      "while time.time() < end:\n    i += 1\n    print(f'{i:09d} {pad}')"),
    ],
    # A notebook function: nested parameters in, a value out through the
    # nbsandbox module, a model file read from the published bundle, and a
    # contract for both (the contract goes in the notebook's metadata: see
    # METADATA below). "fail": true makes it raise; "bad_output": true
    # returns a value the output contract rejects.
    "forecast_fn.ipynb": [
        params_cell("region = 'jakarta'\ncfg = {'months': 3, 'include': ['sales']}\nfail = False\nbad_output = False"),
        new_code_cell("import nbsandbox, pandas as pd\n"
                      "with open('models/model.txt') as f:\n    model = f.read().strip()\n"
                      "if fail:\n    raise ValueError('asked to fail')\n"
                      "months = cfg['months']\n"
                      "df = pd.DataFrame({'month': range(1, months + 1), 'value': [m * 1.5 for m in range(1, months + 1)]})\n"
                      "print('model', model, 'region', region, 'exact params', nbsandbox.parameters(), flush=True)"),
        new_code_cell("if bad_output:\n    nbsandbox.output({'oops': True})\n"
                      "else:\n    nbsandbox.output({'region': region, 'model': model, 'forecast': df['value'],"
                      " 'table': df, 'echo': nbsandbox.parameters()})"),
    ],
    # Crash the kernel in C code.
    "crash.ipynb": [
        new_code_cell("print('about to crash', flush=True)"),
        new_code_cell("import ctypes\nctypes.string_at(0)"),
    ],
}


# Extra notebook metadata (the nbsandbox contract).
METADATA = {
    "forecast_fn.ipynb": {"nbsandbox": {"contract": {
        "input": {"type": "object", "required": ["region"], "properties": {
            "region": {"type": "string", "minLength": 1},
            "cfg": {"type": "object", "properties": {"months": {"type": "integer", "minimum": 1, "maximum": 24}}},
            "fail": {"type": "boolean"}, "bad_output": {"type": "boolean"}}},
        "output": {"type": "object", "required": ["region", "forecast"], "properties": {
            "forecast": {"type": "array", "items": {"type": "number"}}}},
    }}},
}


def main():
    for name, cells in NOTEBOOKS.items():
        nb = new_notebook(cells=cells)
        nb.metadata["kernelspec"] = {"name": "python3", "display_name": "Python 3", "language": "python"}
        nb.metadata.update(METADATA.get(name, {}))
        nbformat.validate(nb)
        with open(os.path.join(HERE, name), "w") as f:
            nbformat.write(nb, f)
        print("wrote", name)


if __name__ == "__main__":
    main()

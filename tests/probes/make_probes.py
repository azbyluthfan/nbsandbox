# Copyright 2026 The nbsandbox Authors
# SPDX-License-Identifier: Apache-2.0
"""Generates the isolation probe notebooks.

    make probes

Secrets are passed in two halves and only ever joined inside the *holder*
(bob's run). The probe (alice's run) searches for "half A immediately
followed by half B" without building the joined string, so it can't find the
secret in its own memory, files or parameters by accident.

Each probe reports what it saw, not just pass/fail, into findings.json in
NBSANDBOX_OUTPUT_DIR; the integration test reads it as an artifact.
"""

import os

import nbformat
from nbformat.v4 import new_code_cell, new_notebook

HERE = os.path.dirname(os.path.abspath(__file__))


def params(src):
    c = new_code_cell(src)
    c.metadata["tags"] = ["parameters"]
    return c


HOLDER = [
    params("mem_a = ''\nmem_b = ''\nshm_a = ''\nshm_b = ''\nmax_seconds = 50"),
    new_code_cell(
        "import os, time\n"
        "secret = mem_a + mem_b  # kept in this process's memory\n"
        "with open('/dev/shm/holder_secret', 'w') as f:\n"
        "    f.write(shm_a + shm_b)\n"
        "print('holder ready', flush=True)\n"
        "start = time.time()\n"
        "while not os.path.exists('/space/release.txt') and time.time() - start < max_seconds:\n"
        "    time.sleep(0.5)\n"
        "with open('/dev/shm/holder_secret') as f:\n"
        "    intact = secret == mem_a + mem_b and f.read() == shm_a + shm_b\n"
        "print(f'holder done intact={intact} waited={time.time() - start:.1f}s', flush=True)\n"
        "assert intact, 'holder secrets changed'"
    ),
]

PROBE_SETUP = """\
import os, json, socket, stat, time, re
F = {}
OUT = os.environ['NBSANDBOX_OUTPUT_DIR']

def save():
    with open(os.path.join(OUT, 'findings.json'), 'w') as f:
        json.dump(F, f, indent=1, default=str)

def contains(data, a, b):
    \"\"\"True if bytes `a` is immediately followed by bytes `b` (never joins them).\"\"\"
    if not a or not b:
        return False
    i = data.find(a)
    while i >= 0:
        if data[i + len(a):i + len(a) + len(b)] == b:
            return True
        i = data.find(a, i + 1)
    return False

def read(path, limit=1 << 20):
    try:
        with open(path, 'rb') as f:
            return f.read(limit)
    except OSError as e:
        return e
"""

i1 = """\
# Space B by absolute host path, by ../, and by listing / and /space/..; this
# space's own run outputs and published versions, which a run must not reach
# either; and /run, which must hold only this run's folder.
own = data_dir + '/spaces/' + space_a_id if data_dir and space_a_id else ''
paths = [space_b_host_path, '/space/../' + space_b_id, '/space/../' + space_b_id + '/canary.txt',
         data_dir + '/spaces', '/spaces', '/srv/nbsandbox',
         own + '/runs' if own else '', own + '/functions' if own else '', '/space/../runs', '/space/../functions',
         '/run/../runs', '/run/../' + space_b_id]
exists = {p: os.path.exists(p) for p in paths if p}
root = sorted(os.listdir('/'))
parent = sorted(os.listdir('/space/..'))
run_entries = sorted(os.listdir('/run'))
other_runs = [e for e in run_entries if len(e) == 36 and e.count('-') == 4]
F['space-paths'] = {'paths_exist': exists, 'ls_root': root, 'ls_space_parent': parent, 'ls_run': run_entries,
           'pass': not any(exists.values()) and space_b_id not in root and space_b_id not in parent and not other_runs
                   and 'input.ipynb' in run_entries}
save(); print('space-paths', F['space-paths']['pass'])
"""

i2 = """\
# Search the container's filesystem for bob's canary.
ca, cb = canary_a.encode(), canary_b.encode()
found, scanned, start = [], 0, time.time()
def scan(top, skip_top=()):
    global scanned
    for root, dirs, files in os.walk(top, followlinks=False):
        if root == '/':
            dirs[:] = [d for d in dirs if d not in skip_top]
        for name in files:
            p = os.path.join(root, name)
            try:
                st = os.lstat(p)
                if not stat.S_ISREG(st.st_mode) or st.st_size > 5_000_000:
                    continue
                with open(p, 'rb') as f:
                    data = f.read()
            except OSError:
                continue
            scanned += 1
            if contains(data, ca, cb):
                found.append(p)
# /usr is the read-only image; /proc and /sys are kernel views (the process-list and memory-secret probes cover /proc).
scan('/', skip_top=('proc', 'sys', 'dev', 'usr'))
scan('/dev/shm')
F['canary-search'] = {'found_in': found, 'files_scanned': scanned, 'seconds': round(time.time() - start, 1),
           'skipped': ['/proc', '/sys', '/dev except /dev/shm', '/usr (read-only image)'], 'pass': not found}
save(); print('canary-search', F['canary-search']['pass'], scanned, 'files')
"""

i3 = """\
# List /proc: only this run's processes should exist.
procs = []
for pid in sorted((p for p in os.listdir('/proc') if p.isdigit()), key=int):
    cmd = read(f'/proc/{pid}/cmdline')
    procs.append({'pid': int(pid), 'cmd': cmd.replace(b'\\0', b' ').decode(errors='replace').strip() if isinstance(cmd, bytes) else str(cmd)})
foreign = [p for p in procs if 'holder' in p['cmd'] or 'nbsandboxd' in p['cmd'] or 'dockerd' in p['cmd']]
F['proc-list'] = {'processes': procs, 'count': len(procs), 'foreign': foreign, 'pass': not foreign and len(procs) < 30}
save(); print('proc-list', F['proc-list']['pass'], len(procs), 'processes')
"""

i4 = """\
# Search every readable /proc/*/environ, cmdline and mem for the holder's
# in-memory secret.
ma, mb = mem_a.encode(), mem_b.encode()
hits, readable, denied, mem_bytes = [], {'environ': 0, 'cmdline': 0, 'mem': 0}, {'environ': 0, 'cmdline': 0, 'mem': 0}, 0
start = time.time()
for pid in (p for p in os.listdir('/proc') if p.isdigit()):
    for kind in ('environ', 'cmdline'):
        data = read(f'/proc/{pid}/{kind}')
        if isinstance(data, bytes):
            readable[kind] += 1
            if contains(data, ma, mb):
                hits.append(f'{pid}/{kind}')
        else:
            denied[kind] += 1
    try:
        maps = open(f'/proc/{pid}/maps').read().splitlines()
        with open(f'/proc/{pid}/mem', 'rb', buffering=0) as mem:
            readable['mem'] += 1
            for line in maps:
                rng, perms = line.split()[:2]
                if 'r' not in perms:
                    continue
                lo, hi = (int(x, 16) for x in rng.split('-'))
                if hi - lo > 256 << 20:
                    continue
                try:
                    mem.seek(lo)
                    chunk = mem.read(hi - lo)
                except (OSError, ValueError, OverflowError):
                    continue
                mem_bytes += len(chunk)
                if contains(chunk, ma, mb):
                    hits.append(f'{pid}/mem@{rng}')
    except OSError:
        denied['mem'] += 1
F['memory-secret'] = {'hits': hits, 'readable': readable, 'denied': denied, 'mem_bytes_searched': mem_bytes,
           'seconds': round(time.time() - start, 1), 'pass': not hits}
save(); print('memory-secret', F['memory-secret']['pass'])
"""

i5 = """\
# /dev/shm must not hold the holder's shared-memory secret.
sa, sb = shm_a.encode(), shm_b.encode()
entries = os.listdir('/dev/shm')
hit = [e for e in entries if isinstance(read('/dev/shm/' + e), bytes) and contains(read('/dev/shm/' + e), sa, sb)]
F['shared-memory'] = {'dev_shm_entries': entries, 'hits': hit, 'pass': not hit and 'holder_secret' not in entries}
save(); print('shared-memory', F['shared-memory']['pass'])
"""

i6 = """\
# No Docker socket, only allowlisted environment, and no network at all.
socks = [p for p in ('/var/run/docker.sock', '/run/docker.sock', '/var/run/docker', '/run/containerd/containerd.sock') if os.path.exists(p)]
env_keys = sorted(os.environ)
forbidden = [k for k in env_keys if k.startswith(('DOCKER', 'NBSANDBOX_DATA', 'NBSANDBOX_USERS', 'NBSANDBOX_DOCKER', 'NBSANDBOX_CONFIG', 'NBSANDBOX_LISTEN'))
             or re.search('PASSWORD|TOKEN|SECRET|CREDENTIAL', k)]
attempts = {}
targets = [('socket-proxy', 2375), ('nbsandboxd', 8080), ('host.docker.internal', 8080), ('8.8.8.8', 53), ('1.1.1.1', 443),
           ('169.254.169.254', 80)] + [(t.rsplit(':', 1)[0], int(t.rsplit(':', 1)[1])) for t in extra_targets]
for host, port in targets:
    try:
        s = socket.create_connection((host, port), timeout=3)
        s.close()
        attempts[f'{host}:{port}'] = 'CONNECTED'
    except OSError as e:
        attempts[f'{host}:{port}'] = type(e).__name__ + ': ' + str(e)[:80]
# The kernel creates tunnel stubs (tunl0, gre0, sit0, ...) in every network
# namespace; they are down and unconfigured. On Docker what matters: nothing
# but loopback is up, and there are no routes. A Kubernetes Pod always has
# an interface and routes; there the deny-all NetworkPolicy must refuse
# every target, the API server, the worker and its node included.
# gVisor has no /sys/class/net: there the interfaces come from the kernel's
# list and their flags from SIOCGIFFLAGS.
def iface_flags(name):
    flags = read(f'/sys/class/net/{name}/flags')
    if isinstance(flags, bytes):
        return int(flags.strip() or b'0', 16)
    import fcntl, struct
    try:
        with socket.socket(socket.AF_INET, socket.SOCK_DGRAM) as s:
            res = fcntl.ioctl(s.fileno(), 0x8913, struct.pack('16sH14x', name.encode()[:15], 0))  # SIOCGIFFLAGS
    except OSError:
        return 0  # not an interface
    return struct.unpack('16sH14x', res)[1]
ifaces, up = [], []
if os.path.isdir('/sys/class/net'):  # interfaces are directories (bonding_masters is a file)
    names = [n for n in os.listdir('/sys/class/net') if os.path.isdir(f'/sys/class/net/{n}')]
else:
    names = [n for _, n in socket.if_nameindex()]
for name in sorted(names):
    ifaces.append(name)
    if iface_flags(name) & 1 and name != 'lo':
        up.append(name)
route = read('/proc/net/route')
routes = [l for l in route.decode().splitlines()[1:] if l.strip()] if isinstance(route, bytes) else []
F['socket-env-network'] = {'sockets_found': socks, 'env_keys': env_keys, 'forbidden_env': forbidden, 'connections': attempts,
           'interfaces': ifaces, 'interfaces_up_besides_lo': up, 'ipv4_routes': routes,
           'kubernetes': kubernetes,
           'pass': not socks and not forbidden and 'CONNECTED' not in attempts.values() and (kubernetes or (not up and not routes))}
save(); print('socket-env-network', F['socket-env-network']['pass'])
"""

i7 = """\
# Unprivileged: uid 1000, no capabilities, no-new-privileges, read-only
# image, nosuid /tmp. Kubernetes mounts emptyDir volumes without nosuid; a
# setuid bit is still ignored under no-new-privileges, which is required.
status = dict(l.split(':\\t', 1) for l in open('/proc/self/status').read().splitlines() if ':\\t' in l)
try:
    open('/usr/nbsandbox-write-test', 'w').close()
    write_usr = 'WRITTEN'
except OSError as e:
    write_usr = type(e).__name__
p = '/tmp/setuid-test'
open(p, 'w').close()
try:
    os.chmod(p, 0o4755)
    chmod = 'set' if os.stat(p).st_mode & stat.S_ISUID else 'not set'
except OSError as e:
    chmod = type(e).__name__
mounts = {l.split()[1]: l.split()[3] for l in open('/proc/mounts')}
apparmor = read('/proc/self/attr/current')
F['privileges'] = {
    'uid': os.getuid(), 'gid': os.getgid(), 'groups': os.getgroups(),
    'CapEff': status.get('CapEff', '').strip(), 'CapPrm': status.get('CapPrm', '').strip(), 'CapBnd': status.get('CapBnd', '').strip(),
    'NoNewPrivs': status.get('NoNewPrivs', '').strip(), 'Seccomp': status.get('Seccomp', '').strip(),
    'apparmor': apparmor.decode(errors='replace').strip() if isinstance(apparmor, bytes) else str(apparmor),
    'write_usr': write_usr, 'chmod_u+s_on_tmp': chmod, 'tmp_mount': mounts.get('/tmp', ''), 'root_mount': mounts.get('/', ''),
}
cap_zero = F['privileges']['CapEff'] and int(F['privileges']['CapEff'], 16) == 0 and int(F['privileges']['CapPrm'] or '0', 16) == 0
F['privileges']['pass'] = (os.getuid() == 1000 and bool(cap_zero) and F['privileges']['NoNewPrivs'] == '1' and write_usr != 'WRITTEN'
                   and (kubernetes or 'nosuid' in F['privileges']['tmp_mount']) and 'ro' in F['privileges']['root_mount'].split(','))
save(); print('privileges', F['privileges']['pass'])
"""

PROBE = [
    params("space_a_id = ''\nspace_b_id = ''\nspace_b_host_path = ''\ndata_dir = ''\ncanary_a = ''\ncanary_b = ''\n"
           "mem_a = ''\nmem_b = ''\nshm_a = ''\nshm_b = ''\nkubernetes = False\nextra_targets = []"),
    new_code_cell(PROBE_SETUP),
    new_code_cell(i1), new_code_cell(i2), new_code_cell(i3), new_code_cell(i4),
    new_code_cell(i5), new_code_cell(i6), new_code_cell(i7),
    new_code_cell("print('all probes:', {k: v['pass'] for k, v in F.items()})"),
]


def main():
    for name, cells in {"holder.ipynb": HOLDER, "isolation_probe.ipynb": PROBE}.items():
        nb = new_notebook(cells=cells)
        nb.metadata["kernelspec"] = {"name": "python3", "display_name": "Python 3", "language": "python"}
        nbformat.validate(nb)
        with open(os.path.join(HERE, name), "w") as f:
            nbformat.write(nb, f)
        print("wrote", name)


if __name__ == "__main__":
    main()

#!/usr/bin/env python3
"""One-command, LOCAL E2B Embed + Tether setup. Python standard library only.
No fake backend, no Pi installation, no cloud account, no credentials copied to guests.
The privileged E2B host setup is explicit: kernel modules, NBD, hugepages, iptables.
"""
import argparse
import copy
import json
import os
from pathlib import Path
import secrets
import shutil
import subprocess
import sys
import tempfile
import time
import urllib.error
import urllib.request

ROOT = Path(__file__).resolve().parents[1]
STATE = ROOT / '.local'
EMBED = STATE / 'embed'
CONFIG = EMBED / 'compose.json'
PROJECT = 'tether-local'
PORTS = '3000,3001,3002,3003,3010,5007,5008,5009,5432,6379,8123,9000,6061,30006'
COMMENT = 'tether-local-loopback-only'


def private_json(path, obj):
    path.parent.mkdir(parents=True, exist_ok=True)
    fd, tmp = tempfile.mkstemp(dir=path.parent, prefix='.'+path.name+'-')
    try:
        with os.fdopen(fd, 'w') as f:
            json.dump(obj, f, indent=2)
            f.write('\n')
            f.flush()
            os.fsync(f.fileno())
        os.replace(tmp, path)
    finally:
        if os.path.exists(tmp):
            os.unlink(tmp)


def run(args, timeout=60, log=None):
    try:
        result = subprocess.run(args, text=True, stdout=subprocess.PIPE,
                                stderr=subprocess.STDOUT, timeout=timeout, cwd=ROOT)
    except subprocess.TimeoutExpired as e:
        if log:
            Path(log).write_bytes(e.stdout or b'')
        raise RuntimeError(f'command timed out after {timeout}s; see {log or "local report"}') from e
    if log:
        Path(log).write_text(result.stdout)
    if result.returncode:
        raise RuntimeError(f'command exited {result.returncode}: {args[:4]}; see {log or result.stdout[-800:]}')
    return result.stdout


def compose(*args, timeout=60, log=None):
    return run(['docker', 'compose', '-p', PROJECT, '-f', str(CONFIG), *args], timeout, log)


def mem_available():
    if not Path('/proc/meminfo').exists():
        return 0
    for line in Path('/proc/meminfo').read_text().splitlines():
        if line.startswith('MemAvailable:'):
            return int(line.split()[1]) * 1024
    return 0


def doctor():
    checks = {
        'linux': sys.platform.startswith('linux'),
        'docker_cli': bool(shutil.which('docker')),
        'kvm_device': Path('/dev/kvm').exists(),
        'tun_device': Path('/dev/net/tun').exists(),
        'page_size_4096': os.sysconf('SC_PAGE_SIZE') == 4096,
        'available_ram_mib': round(mem_available() / 1024**2),
        'free_disk_gib': round(shutil.disk_usage(ROOT).free / 1024**3, 1),
    }
    try:
        checks['docker_engine'] = run(['docker', 'version', '--format', '{{.Server.Version}}']).strip()
        checks['docker_compose'] = run(['docker', 'compose', 'version', '--short']).strip()
    except Exception as e:
        checks['docker_error'] = str(e)
    return checks


def small_config(config, pages=256):
    d = copy.deepcopy(config)
    d['name'] = PROJECT
    # `compose config` expands names using the upstream project's name.
    # Re-home them explicitly; otherwise two installs silently share databases.
    for kind in ['volumes', 'networks']:
        for name, entry in d.get(kind, {}).items():
            if not entry.get('external'):
                entry['name'] = PROJECT + '_' + name
    s = d['services']
    for name in ['dashboard', 'dashboard-api', 'otel-collector']:
        s.pop(name, None)
    s['ready']['depends_on'].pop('dashboard', None)
    s['host-setup']['environment']['HUGEPAGES'] = str(pages)
    s['host-setup']['environment']['NBDS_MAX'] = '16'
    s['orchestrator']['environment']['NBD_POOL_SIZE'] = '4'
    s['orchestrator']['environment']['GOMEMLIMIT'] = '256MiB'
    # PostgreSQL's default huge_pages=try would consume the VM pool itself.
    s['postgres']['command'] = ['postgres', '-c', 'huge_pages=off', '-c', 'shared_buffers=64MB']
    limits = {
        'postgres': ('192m', .15), 'redis': ('64m', .05),
        'clickhouse': ('768m', .4), 'vector': ('96m', .05),
        'orchestrator': ('512m', .25), 'api': ('256m', .25),
        'client-proxy': ('128m', .15), 'ready': ('32m', .02),
        'base-template': ('256m', .5), 'preflight': ('128m', .25),
        'host-setup': ('128m', .5), 'fetch-artifacts': ('256m', .5),
        'api-secrets': ('64m', .1), 'db-migrator': ('192m', .25),
        'clickhouse-migrator': ('192m', .25), 'seed': ('128m', .25),
        'smoke': ('256m', .5), 'host-teardown': ('128m', .25),
    }
    for name, (memory, cpu) in limits.items():
        if name in s:
            s[name]['mem_limit'] = memory
            s[name]['memswap_limit'] = memory
            s[name]['cpus'] = cpu
            s[name].setdefault('logging', {})['options'] = {'max-size':'5m', 'max-file':'2'}
    return d


def initialize(pages):
    EMBED.mkdir(parents=True, exist_ok=True)
    if CONFIG.exists():
        d = small_config(json.loads(CONFIG.read_text()), pages)
        private_json(CONFIG, d)
        return d
    info = doctor()
    required = ['linux','docker_cli','kvm_device','tun_device','page_size_4096']
    if not all(info.get(k) for k in required) or 'docker_error' in info:
        raise RuntimeError('host prerequisites missing: ' + json.dumps(info))
    if info['free_disk_gib'] < 20:
        raise RuntimeError('E2B needs at least 20 GiB free disk')
    if mem_available() < pages * 2 * 1024**2 + 3 * 1024**3:
        raise RuntimeError('insufficient available RAM for the guest pool plus control plane and headroom')
    if Path('/var/lib/e2b').exists():
        raise RuntimeError('unmanaged /var/lib/e2b exists; refuse to overwrite another E2B installation')
    source = ROOT / 'deploy' / 'embed'
    raw = run(['docker','compose','--env-file',str(source/'pins.env'),
               '-f',str(source/'compose.yaml'),'--profile','test','--profile','purge',
               'config','--format','json'])
    d = small_config(json.loads(raw), pages)
    private_json(CONFIG, d)
    baseline = json.loads((EMBED/'baseline.json').read_text()) if (EMBED/'baseline.json').exists() else {'hugepages':int(Path('/proc/sys/vm/nr_hugepages').read_text())}
    baseline['pages'] = pages
    private_json(EMBED/'baseline.json', baseline)
    return d


def tools_host(script, config):
    image = config['services']['preflight']['image']
    # Use E2B's official tools image, but execute host iptables in the host's
    # network/mount namespaces. This is not an agent execution environment.
    return run(['docker','run','--rm','--privileged','--pid','host','--network','host',
                image,'nsenter','-t','1','-m','-n','-p','--','/bin/sh','-c',script])


def firewall(config, add=True):
    rule = f'INPUT ! -i lo -p tcp -m multiport --dports {PORTS} -m comment --comment {COMMENT} -j DROP'
    if add:
        tools_host(f'iptables -C {rule} 2>/dev/null || iptables -I {rule}', config)
        private_json(EMBED/'guard.json', {'installed': True})
    elif (EMBED/'guard.json').exists():
        tools_host(f'iptables -C {rule} 2>/dev/null && iptables -D {rule} || true', config)
        (EMBED/'guard.json').unlink(missing_ok=True)


def credentials():
    return json.loads((STATE/'credentials.json').read_text())


def request(path, body=None, method=None, timeout=330):
    c = credentials()
    req = urllib.request.Request(c['api_url']+path,
        data=json.dumps(body).encode() if body is not None else None,
        headers={'Authorization':'Bearer '+c['token'], 'Content-Type':'application/json'},
        method=method or ('POST' if body is not None else 'GET'))
    try:
        with urllib.request.urlopen(req, timeout=timeout) as r:
            return r.status, dict(r.headers), json.loads(r.read() or '{}')
    except urllib.error.HTTPError as e:
        return e.code, dict(e.headers), json.loads(e.read() or '{}')


def e2b_control(path, body=None, method='POST'):
    c = credentials()
    req = urllib.request.Request(c['e2b_api_url']+path,
        data=json.dumps(body).encode() if body is not None else b'', method=method,
        headers={'X-API-Key':c['e2b_key'], 'Content-Type':'application/json'})
    with urllib.request.urlopen(req, timeout=30) as r:
        return json.loads(r.read() or '{}')


def action(task_id, action_id, code, language='bash', **opts):
    body = dict(action_id=action_id, code=code, language=language, **opts)
    status, headers, result = request('/v1/tasks/'+task_id+'/actions', body)
    if status != 200:
        raise RuntimeError(f'action HTTP {status}: {result}')
    return body, headers, result


def up(args):
    config = initialize(args.pages)
    report = {'host':doctor(),'phases':[], 'live_e2e_passed':False, 'started':time.time()}
    try:
        print('Pulling pinned E2B runtime images (no cloud account, no fake backend)...', flush=True)
        compose('--parallel','3','pull','--quiet','--ignore-buildable',timeout=args.pull_timeout,log=EMBED/'pull.log')
        report['phases'].append({'phase':'pull','status':'passed'})
        firewall(config)
        print('Preparing REAL E2B; reserve VM hugepages BEFORE databases start.', flush=True)
        compose('run','--rm','--no-deps','preflight',timeout=30,log=EMBED/'preflight.log')
        compose('run','--rm','--no-deps','host-setup',timeout=60,log=EMBED/'host-setup.log')
        report['phases'].append({'phase':'host_setup','status':'passed'})
        print('Starting E2B control plane and building its base template.', flush=True)
        compose('up','-d','--wait','--wait-timeout',str(args.start_timeout),
                timeout=args.start_timeout+30,log=EMBED/'up.log')
        report['phases'].append({'phase':'embed_ready','status':'passed'})
        # Capture generated key privately. Never print ready's key-bearing logs.
        lines = compose('exec','-T','ready','cat','/run/e2b/sdk.env')
        env = {}
        for line in lines.splitlines():
            if line.startswith('export '):
                key,value = line[7:].split('=',1); env[key] = value.strip('"\'')
        if not env.get('E2B_API_KEY'):
            raise RuntimeError('ready did not expose a team key')
        c = credentials() if (STATE/'credentials.json').exists() else {'token':secrets.token_hex(24)}
        c.update(api_url='http://127.0.0.1:8090',e2b_api_url='http://127.0.0.1:3000',
                 e2b_sandbox_url='http://127.0.0.1:3002',e2b_key=env['E2B_API_KEY'])
        private_json(STATE/'credentials.json', c)
        (STATE/'ledger').mkdir(exist_ok=True)
        config['services']['tether'] = {
            'image':'tether:local','build':{'context':str(ROOT)},
            'network_mode':'host','restart':'unless-stopped','user':f'{os.getuid()}:{os.getgid()}',
            'mem_limit':'64m','memswap_limit':'64m','cpus':.25,
            'read_only':True,'cap_drop':['ALL'],'security_opt':['no-new-privileges:true'],
            'volumes':[{'type':'bind','source':str(STATE/'ledger'),'target':'/data'}],
            'environment':{'TETHER_API_TOKEN':c['token'],'E2B_API_KEY':c['e2b_key'],
                           'E2B_API_URL':c['e2b_api_url'],'E2B_SANDBOX_URL':c['e2b_sandbox_url'],
                           'E2B_EXEC_MODE':'commands'},
            'command':['-listen','127.0.0.1:8090','-data-dir','/data','-template','base','-exec-timeout','5m',
                       '-max-concurrent-actions','1','-pause-after-action'],
            'logging':{'driver':'json-file','options':{'max-size':'5m','max-file':'2'}},
        }
        private_json(CONFIG, config)
        print('Building/starting Tether; generated credentials remain private in .local/.', flush=True)
        compose('build','tether',timeout=300,log=EMBED/'build.log')
        compose('up','-d','--no-deps','tether',timeout=30,log=EMBED/'tether-up.log')
        for _ in range(30):
            try:
                if request('/healthz',timeout=2)[0] == 200: break
            except Exception: time.sleep(.5)
        else: raise RuntimeError('Tether health check failed')
        report['phases'].append({'phase':'tether_ready','status':'passed'})
        print('Ready: Tether http://127.0.0.1:8090; run make e2e for live verification.', flush=True)
    except Exception as e:
        report['error'] = str(e)
        print('Setup did NOT pass: '+str(e), file=sys.stderr)
        raise
    finally:
        report['finished']=time.time()
        private_json(STATE/'reports'/'setup.json',report)


def smoke():
    report={'backend':'REAL E2B Embed / Firecracker','started':time.time(),'checks':[], 'live_e2e_passed':False}
    ids=[]
    try:
        # Two independent guests, one resident at a time: the tiny-host daemon
        # pauses after actions and resumes on the next one. This proves logical
        # isolation/persistence, NOT two concurrent resident VMs.
        for owner in ['alpha','beta']:
            tid='e2e-'+owner+'-'+secrets.token_hex(4);ids.append(tid)
            status,_,_=request('/v1/tasks',{'id':tid,'title':'Live microVM isolation '+owner})
            if status != 201: raise RuntimeError('create task failed')
            code=f"printf '{owner}\\n' > /home/user/tether-owner.txt; printf 'GUEST_KERNEL='; uname -r; cat /home/user/tether-owner.txt"
            body,_,res=action(tid,'write-owner',code)
            if res.get('status')!='succeeded' or owner not in res.get('result',{}).get('stdout',''):
                raise RuntimeError('guest execution failed: '+str(res))
            status,headers,replay=request('/v1/tasks/'+tid+'/actions',body)
            if status!=200 or headers.get('Idempotent-Replay')!='true' or replay.get('attempts')!=1:
                raise RuntimeError('stable action ID did not deduplicate')
            report['checks'].append({'name':owner+'_execution_and_dedupe','passed':True,'result':res.get('result')})
        sandboxes=[]
        for owner,tid in zip(['alpha','beta'],ids):
            _,_,task=request('/v1/tasks/'+tid);sandboxes.append(task['sandbox_id'])
            _,_,res=action(tid,'read-owner','cat /home/user/tether-owner.txt')
            if res.get('status') != 'succeeded':
                raise RuntimeError('filesystem read was not verified: '+res.get('error', 'action did not succeed'))
            if res.get('result',{}).get('stdout','').strip()!=owner:
                raise RuntimeError('filesystem contents did not match the expected owner')
        if len(set(sandboxes))!=2: raise RuntimeError('tasks did not get separate sandboxes')
        report['sandbox_ids']=sandboxes
        report['checks'].append({'name':'same_path_distinct_microvm_filesystems','passed':True})
        config=json.loads(CONFIG.read_text())
        e2b_control('/v2/sandboxes/'+sandboxes[0]+'/connect', {'timeout':300})
        try:
            proof=tools_host('for p in $(pgrep -x firecracker); do echo "FC_PID=$p"; for f in /proc/$p/fd/*; do readlink "$f"; done; done',config)
        finally:
            e2b_control('/sandboxes/'+sandboxes[0]+'/pause')
        if not ('kvm-vm' in proof or 'kvm-vcpu' in proof or '/dev/kvm' in proof):
            raise RuntimeError('no live Firecracker/KVM file descriptors observed; refuse to call this a microVM pass')
        report['checks'].append({'name':'live_firecracker_kvm_proof','passed':True,'proof':proof})
        report['live_e2e_passed']=True
        report['resident_guest_limit']=1
        print('LIVE E2E PASS: two distinct Firecracker guests multiplexed one at a time; isolated persistent filesystems, command execution and action dedupe.')
    except Exception as e:
        report['error'] = str(e)
        raise
    finally:
        for tid in ids:
            try: request('/v1/tasks/'+tid+'/cancel',{},timeout=30)
            except Exception: pass
        report['finished']=time.time()
        private_json(STATE/'reports'/'e2e.json',report)
        if not report['live_e2e_passed']: print('LIVE E2E did not pass; see .local/reports/e2e.json',file=sys.stderr)
    return report


def down(args):
    if not CONFIG.exists(): return
    config=json.loads(CONFIG.read_text())
    compose('down',*(['-v'] if args.purge else []),timeout=120,log=EMBED/'down.log')
    if args.purge:
        compose('--profile','purge','run','--rm','--no-deps','host-teardown',timeout=120,log=EMBED/'teardown.log')
        CONFIG.unlink(missing_ok=True)
        print('Removed local Embed state and its host setup; Tether ledger/reports retained.')
    else:
        print('Stopped services; data and E2B host setup remain. Use --purge to undo demo host setup.')
    firewall(config,False)


def main():
    p=argparse.ArgumentParser(description=__doc__)
    p.add_argument('command',choices=['doctor','up','status','down','smoke'])
    p.add_argument('--pages',type=int,default=256,help='2 MiB hugepages (default: 512 MiB pool, one active guest)')
    p.add_argument('--pull-timeout',type=int,default=900)
    p.add_argument('--start-timeout',type=int,default=600)
    p.add_argument('--purge',action='store_true',help='drop only the owned Embed demo volumes and undo its host setup')
    args=p.parse_args()
    if args.pages<256 or args.pages>2048: p.error('--pages must be between 256 and 2048')
    if args.command=='doctor':print(json.dumps(doctor(),indent=2))
    elif args.command=='up':up(args)
    elif args.command=='smoke':smoke()
    elif args.command=='down':down(args)
    else:
        if CONFIG.exists():print(compose('ps'))
        else:print('Not initialized; run make local-up.')

if __name__=='__main__':
    try:main()
    except Exception as e:
        print(str(e),file=sys.stderr);sys.exit(1)

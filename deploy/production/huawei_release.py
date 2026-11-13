#!/usr/bin/python3
"""Release Longdao on its isolated Huawei runtime; never deploy the old stack."""
import argparse
import copy
import datetime
import fcntl
import hashlib
import json
import os
from pathlib import Path
import re
import shutil
import subprocess
import sys
import time
import urllib.error
import urllib.request

ROOT = Path('/www/longdao')
SOCKET = 'unix:///run/longdao-docker.sock'
DOCKER = ['/usr/bin/docker', '-H', SOCKET]
EDGE_DOCKER = ['/usr/bin/docker', '-H', 'unix:///var/run/docker.sock']
CADDY = Path('/www/sep/deploy/Caddyfile')
BEGIN, END = '# LONGDAO-AI-BEGIN', '# LONGDAO-AI-END'
PORTS = {'green': 18080, 'blue': 18081}
APP_DATA_MOUNT = '/www/longdao/app-data:/app/data'
VIDEO_REFERENCE_MOUNT = '/www/longdao/media/video-inputs:/app/media/video-inputs'
REPO = 'ghcr.io/12456756/sub2api'
VERSION = re.compile(r'[0-9]+\.[0-9]+\.[0-9]+-[0-9]+')
SEP = ['sep-green-web', 'sep-green-backend', 'sep-caddy',
       'sep-postgres', 'sep-redis', 'sep-downloads']


def read_json(path):
    return json.loads(path.read_text())


def write_json(path, value):
    temporary = path.with_name(path.name + '.new')
    with temporary.open('w') as f:
        os.chmod(temporary, 0o600)
        json.dump(value, f, indent=2)
        f.write('\n')
        f.flush()
        os.fsync(f.fileno())
    temporary.replace(path)


def ai_block(text):
    if text.count(BEGIN) != 1 or text.count(END) != 1:
        raise RuntimeError('Caddy AI block markers are missing or ambiguous')
    start, end = text.index(BEGIN), text.index(END) + len(END)
    if end <= start:
        raise RuntimeError('Caddy AI block markers are reversed')
    return text[start:end]


def replace_ai(text, block):
    old = ai_block(text)
    return text.replace(old, block, 1)


def block_color(block):
    matches = re.findall(r'^\s*reverse_proxy 172\.19\.0\.1:(18080|18081)\s*\{', block, re.M)
    expected = 2 if 'images.longdaoec.com {' in block else 1
    if len(matches) != expected or len(set(matches)) != 1 or 'ai.longdaoec.com {' not in block:
        raise RuntimeError('Unexpected AI upstream configuration')
    return {str(port): color for color, port in PORTS.items()}[matches[0]]


def validate_app_mounts(service):
    allowed = [[APP_DATA_MOUNT], [APP_DATA_MOUNT, VIDEO_REFERENCE_MOUNT]]
    if service['network_mode'] != 'host' or service['volumes'] not in allowed:
        raise RuntimeError('Unexpected application mounts/network')


def target_service(current, color, image):
    service = copy.deepcopy(current)
    service['image'] = image
    service['container_name'] = 'longdao-ai-' + color
    service['environment']['SERVER_PORT'] = str(PORTS[color])
    service['healthcheck']['test'] = ['CMD', 'wget', '-q', '-T', '5', '-O',
                                     '/dev/null', 'http://172.19.0.1:%s/health' % PORTS[color]]
    service['restart'] = 'unless-stopped'
    # Explicit service names only; these profiles prevent accidental two-color
    # startup by an unqualified `compose up` after the first deployment.
    service['profiles'] = [color]
    return service


def file_sha256(path):
    digest = hashlib.sha256()
    with path.open('rb') as f:
        for chunk in iter(lambda: f.read(1024 * 1024), b''):
            digest.update(chunk)
    return digest.hexdigest()


class Release:
    def __init__(self):
        self.deploy = ROOT / 'deploy'
        self.compose_path = self.deploy / 'compose.json'
        self.timestamp = datetime.datetime.now(datetime.timezone.utc).strftime('%Y%m%dT%H%M%S%fZ')
        self.log_path = ROOT / 'logs' / ('release-' + self.timestamp + '.log')
        self.log = self.log_path.open('a')
        os.chmod(self.log_path, 0o600)

    def say(self, message):
        print('[longdao-release] ' + message, flush=True)
        self.log.write(message + '\n')
        self.log.flush()

    def run(self, command, timeout=300, output=None):
        self.log.flush()
        with (output.open('wb') if output else open(os.devnull, 'wb')) as sink:
            r = subprocess.run(command, stdout=sink if output else self.log,
                               stderr=self.log, timeout=timeout)
        if r.returncode:
            raise RuntimeError('Command failed (%s); private log: %s' % (command[0], self.log_path))

    def capture(self, command, timeout=60):
        self.log.flush()
        return subprocess.check_output(command, stderr=self.log, timeout=timeout).decode()

    def inspect(self, container, edge=False):
        return json.loads(self.capture((EDGE_DOCKER if edge else DOCKER) + ['inspect', container]))[0]

    def compose(self, path, *args, timeout=300):
        self.run(DOCKER + ['compose', '-f', str(path), '--profile', 'blue',
                          '--profile', 'green'] + list(args), timeout=timeout)

    def healthy(self, name):
        info = self.inspect(name)
        if (not info['State']['Running'] or info['State'].get('OOMKilled')
                or info['State'].get('Health', {}).get('Status') != 'healthy'):
            raise RuntimeError('Container is not healthy: ' + name)
        return info

    def edge_state(self):
        result = {}
        for name in SEP:
            data = self.inspect(name, edge=True)
            if not data['State']['Running']:
                raise RuntimeError('SEP service is not running: ' + name)
            result[name] = {'started': data['State']['StartedAt'],
                            'restarts': data['RestartCount']}
        return result

    def http(self, url, status=200, json_body=False, headers=None):
        opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))
        try:
            request = urllib.request.Request(url, headers=headers or {})
            with opener.open(request, timeout=20) as r:
                body, code = r.read(), r.status
        except urllib.error.HTTPError as e:
            body, code = e.read(), e.code
        if code != status:
            raise RuntimeError('HTTP verification failed: %s (%s)' % (url, code))
        if json_body:
            return json.loads(body)
        return body

    def smoke(self, base, version=None):
        self.http(base + '/health', json_body=True)
        self.http(base + '/')
        settings = self.http(base + '/api/v1/settings/public', json_body=True)
        self.http(base + '/v1/models', status=401)
        if version and settings.get('data', {}).get('version') != version:
            raise RuntimeError('Public endpoint is not serving requested version ' + version)

    def key_catalog(self, base):
        key = (self.deploy / 'smoke-key.private').read_text().strip()
        if not key:
            raise RuntimeError('Private read-only smoke key is missing')
        catalog = self.http(base + '/v1/models', json_body=True,
                            headers={'Authorization': 'Bearer ' + key})
        if not isinstance(catalog.get('data'), list):
            raise RuntimeError('API key model catalog has an unexpected response shape')
        # Compare only the catalog; never print credentials or response bodies.
        encoded = json.dumps(catalog.get('data'), sort_keys=True, separators=(',', ':')).encode()
        return hashlib.sha256(encoded).hexdigest()

    def preflight(self):
        binding = read_json(self.deploy / 'release-target.json')
        if Path('/etc/machine-id').read_text().strip() != binding['machine_id']:
            raise RuntimeError('Wrong target machine')
        self.run(['/usr/bin/mountpoint', '-q', '/www'])
        actual_uuid = self.capture(['/usr/bin/findmnt', '-n', '-o', 'UUID', '-T', '/www']).strip()
        if actual_uuid != binding['data_uuid'] or binding['data_uuid'] != '7a8aa7d0-58ce-4cf3-bb50-b4fecc346735':
            raise RuntimeError('Wrong data disk mounted at /www')
        for directory in ['runtime/docker', 'runtime/containerd', 'postgres', 'redis',
                          'app-data', 'backups', 'logs', 'deploy', 'releases']:
            path = ROOT / directory
            if not path.is_dir() or not path.resolve().is_relative_to(Path('/www')):
                raise RuntimeError('Invalid data path: ' + str(path))
            if self.capture(['/usr/bin/findmnt', '-n', '-o', 'UUID', '-T', str(path)]).strip() != actual_uuid:
                raise RuntimeError('Project data is not on expected disk')
        docker_root = self.capture(DOCKER + ['info', '--format', '{{.DockerRootDir}}']).strip()
        if docker_root != str(ROOT / 'runtime/docker'):
            raise RuntimeError('Wrong Docker runtime')
        if shutil.disk_usage(ROOT).free < 5 * 1024 ** 3:
            raise RuntimeError('Less than 5 GiB free on project data disk')
        self.run(['/usr/bin/systemctl', 'is-active', '--quiet', 'longdao-docker', 'longdao-containerd'])
        self.config = read_json(self.compose_path)
        self.old_block = ai_block(CADDY.read_text())
        self.current = block_color(self.old_block)
        self.target = 'blue' if self.current == 'green' else 'green'
        self.current_service = 'sub2api-' + self.current
        self.target_name = 'longdao-ai-' + self.target
        service = self.config['services'][self.current_service]
        env = service['environment']
        expected = {'SERVER_HOST': '172.19.0.1', 'SERVER_PORT': str(PORTS[self.current]),
                    'DATABASE_HOST': '127.0.0.1', 'DATABASE_PORT': '15432',
                    'DATABASE_DBNAME': 'sub2api', 'REDIS_HOST': '127.0.0.1',
                    'REDIS_PORT': '16379', 'AUTO_SETUP': 'false',
                    'SERVER_FRONTEND_URL': 'https://ai.longdaoec.com'}
        if any(str(env.get(k)) != v for k, v in expected.items()):
            raise RuntimeError('Production endpoints differ from expected Huawei configuration')
        validate_app_mounts(service)
        if VIDEO_REFERENCE_MOUNT in service['volumes']:
            reference = ROOT / 'media/video-inputs'
            if not reference.is_dir() or reference.resolve() != reference:
                raise RuntimeError('Invalid video reference path')
            if self.capture(['/usr/bin/findmnt', '-n', '-o', 'UUID', '-T', str(reference)]).strip() != actual_uuid:
                raise RuntimeError('Video references are not on expected disk')
            for name, app in self.config['services'].items():
                if name in ['sub2api-blue', 'sub2api-green']:
                    validate_app_mounts(app)
                    if app['volumes'] != service['volumes']:
                        raise RuntimeError('Application colors must share video reference mounts')
        for name in ['longdao-ai-postgres', 'longdao-ai-redis', 'longdao-ai-' + self.current]:
            self.healthy(name)
        inactive_id = self.capture(DOCKER + ['ps', '-q', '--filter',
                                           'name=^/' + self.target_name + '$']).strip()
        if inactive_id:
            raise RuntimeError('Inactive color is still running; finish the previous release drain first')
        self.sep_before = self.edge_state()
        self.run(EDGE_DOCKER + ['exec', 'sep-caddy', 'caddy', 'validate',
                               '--config', '/etc/caddy/Caddyfile', '--adapter', 'caddyfile'])
        self.smoke('http://172.19.0.1:%s' % PORTS[self.current])
        self.smoke('https://ai.longdaoec.com')
        self.catalog_before = self.key_catalog('https://ai.longdaoec.com')
        self.say('Preflight passed: Huawei target, data disk, isolated runtime, current color ' + self.current)

    def backup(self, directory):
        directory.mkdir(mode=0o700)
        self.say('Backing up PostgreSQL, app files, Redis and private deployment configuration')
        self.run(DOCKER + ['exec', 'longdao-ai-postgres', 'pg_dump', '-h', '127.0.0.1',
                          '-p', '15432', '-U', 'sub2api', '-d', 'sub2api', '-Fc'],
                 output=directory / 'sub2api.dump', timeout=900)
        # pg_restore --list requires the dump stream, rather than the host path.
        with (directory / 'sub2api.dump').open('rb') as stream:
            r = subprocess.run(DOCKER + ['exec', '-i', 'longdao-ai-postgres',
                                        'pg_restore', '--list'], stdin=stream,
                               stdout=self.log, stderr=self.log, timeout=60)
        if r.returncode:
            raise RuntimeError('PostgreSQL backup archive validation failed')
        # Active log files are append-only operational output, not restore
        # inputs. They remain on disk; exclude them from the release snapshot
        # so a live log write cannot invalidate the application-state archive.
        self.run(['/usr/bin/tar', '--exclude=./logs', '-C', str(ROOT / 'app-data'), '-czf',
                  str(directory / 'app-data.tar.gz'), '.'])
        snapshot = '.release-' + self.timestamp + '.rdb'
        self.run(DOCKER + ['exec', 'longdao-ai-redis', 'redis-cli', '-p', '16379',
                          '--rdb', '/data/' + snapshot])
        source = ROOT / 'redis' / snapshot
        try:
            shutil.copyfile(source, directory / 'redis.rdb')
        finally:
            source.unlink(missing_ok=True)
        self.run(['/usr/bin/tar', '-C', str(ROOT), '-czf',
                  str(directory / 'deploy.private.tar.gz'), 'deploy'])
        (directory / 'Caddyfile.before.private').write_text(CADDY.read_text())
        write_json(directory / 'release-before.json', {'color': self.current,
                   'image': self.config['services'][self.current_service]['image'],
                   'sep': self.sep_before, 'backup_mode': 'online snapshots; no cross-store transaction',
                   'app_data_excluded': ['logs (live logs retained on disk)']})
        checksums = {}
        for f in directory.iterdir():
            if f.is_file():
                os.chmod(f, 0o600)
                checksums[f.name] = {'bytes': f.stat().st_size,
                                     'sha256': file_sha256(f)}
        write_json(directory / 'checksums.json', checksums)

    def switch_edge(self, expected, replacement, candidate_path):
        current = CADDY.read_text()
        if ai_block(current) != expected:
            raise RuntimeError('AI proxy changed concurrently; refusing overwrite')
        candidate = replace_ai(current, replacement)
        candidate_path.write_text(candidate)
        candidate_path.chmod(0o600)
        self.run(EDGE_DOCKER + ['cp', str(candidate_path), 'sep-caddy:/tmp/longdao-release.caddy'])
        self.run(EDGE_DOCKER + ['exec', 'sep-caddy', 'caddy', 'validate', '--config',
                               '/tmp/longdao-release.caddy', '--adapter', 'caddyfile'])
        # Recheck after validation, and preserve the bind-mounted inode.
        if CADDY.read_text() != current:
            raise RuntimeError('Caddy config changed concurrently; refusing overwrite')
        with CADDY.open('w') as f:
            f.write(candidate)
            f.flush()
            os.fsync(f.fileno())
        self.run(EDGE_DOCKER + ['exec', 'sep-caddy', 'caddy', 'reload', '--config',
                               '/etc/caddy/Caddyfile', '--adapter', 'caddyfile'])

    def deploy_version(self, version, redeploy=False):
        if not VERSION.fullmatch(version):
            raise RuntimeError('Invalid version')
        self.preflight()
        image = REPO + ':' + version
        self.say('Pulling immutable image into isolated runtime: ' + image)
        self.run(DOCKER + ['pull', image], timeout=900)
        image_info = json.loads(self.capture(DOCKER + ['image', 'inspect', image]))[0]
        if image_info['Architecture'] != 'amd64' or image_info['Os'] != 'linux':
            raise RuntimeError('Release image platform mismatch')
        # No-op reruns verify the actual image and endpoints; they still exercise
        # the full GitHub SSH path without restarting a healthy same-image app.
        active = self.inspect('longdao-ai-' + self.current)
        if active['Image'] == image_info['Id'] and not redeploy:
            self.smoke('https://ai.longdaoec.com', version)
            if self.key_catalog('https://ai.longdaoec.com') != self.catalog_before:
                raise RuntimeError('Existing API key catalog changed during verification')
            write_json(self.deploy / 'last-release.json', {'version': version, 'image_id': image_info['Id'],
                       'color': self.current, 'result': 'already-current', 'time_utc': self.timestamp,
                       'target_host': '121.37.128.126', 'runtime': SOCKET})
            self.say('SUCCESS: requested image already active on 121.37.128.126; version and new domain verified')
            return
        directory = ROOT / 'backups' / ('release-' + self.timestamp + '-v' + version)
        self.backup(directory)
        candidate = copy.deepcopy(self.config)
        candidate['services'][self.current_service]['profiles'] = [self.current]
        candidate['services']['sub2api-' + self.target] = target_service(
            self.config['services'][self.current_service], self.target, image)
        candidate_path = self.deploy / ('compose.release-' + self.timestamp + '.json')
        write_json(candidate_path, candidate)
        self.compose(candidate_path, 'config', '--quiet')
        new_block = self.old_block.replace('172.19.0.1:%s' % PORTS[self.current],
                                           '172.19.0.1:%s' % PORTS[self.target])
        switched = False
        try:
            self.say('Applying database migrations before starting target color')
            self.compose(candidate_path, 'run', '--rm', '--no-deps', '-T',
                         'sub2api-' + self.target, '/app/sub2api', '-migrate', timeout=900)
            self.say('Starting target color ' + self.target)
            self.compose(candidate_path, 'up', '-d', '--no-deps', '--wait', '--wait-timeout',
                         '180', 'sub2api-' + self.target, timeout=240)
            target = self.healthy(self.target_name)
            if target['Image'] != image_info['Id']:
                raise RuntimeError('Target container image differs from requested release')
            self.smoke('http://172.19.0.1:%s' % PORTS[self.target], version)
            if self.key_catalog('http://172.19.0.1:%s' % PORTS[self.target]) != self.catalog_before:
                raise RuntimeError('Target API key catalog differs from current production')
            # Reachability from the proxy network is distinct from host-local
            # health (UFW must permit both application color ports).
            self.run(EDGE_DOCKER + ['exec', 'sep-caddy', 'wget', '-q', '-T', '10',
                                   '-O', '/dev/null', 'http://172.19.0.1:%s/health' % PORTS[self.target]])
            self.say('Target verified; switching only AI Caddy upstream')
            # Mark before writing/reloading so reload failure restores the file.
            switched = True
            self.switch_edge(self.old_block, new_block, directory / 'Caddyfile.target.private')
            for _ in range(6):
                try:
                    self.smoke('https://ai.longdaoec.com', version)
                    if self.key_catalog('https://ai.longdaoec.com') != self.catalog_before:
                        raise RuntimeError('API key catalog verification failed after switch')
                    break
                except (RuntimeError, OSError):
                    time.sleep(2)
            else:
                raise RuntimeError('Post-switch public verification failed')
            if self.edge_state() != self.sep_before:
                raise RuntimeError('SEP continuity check failed')
            # Commit the routing-matched compose before draining the old app.
            candidate['services'][self.current_service]['restart'] = 'no'
            write_json(self.compose_path, candidate)
            write_json(self.deploy / 'app-env.json',
                       candidate['services']['sub2api-' + self.target]['environment'])
        except Exception:
            # Never restore the database automatically. Migrations must remain
            # compatible with the current app until the release is accepted.
            if switched:
                present = ai_block(CADDY.read_text())
                if present == new_block:
                    self.switch_edge(new_block, self.old_block, directory / 'Caddyfile.rollback.private')
                elif present != self.old_block:
                    raise RuntimeError('Concurrent proxy edit; manual recovery required, both apps retained')
            write_json(self.compose_path, self.config)
            write_json(self.deploy / 'app-env.json',
                       self.config['services'][self.current_service]['environment'])
            # A migration failure may occur before the target container exists.
            exists = self.capture(DOCKER + ['ps', '-aq', '--filter',
                                          'name=^/' + self.target_name + '$']).strip()
            if exists:
                self.run(DOCKER + ['update', '--restart=no', self.target_name])
                self.run(DOCKER + ['stop', '-t', '120', self.target_name], timeout=150)
            self.say('FAILED: old upstream retained/restored; database was not rolled back')
            raise
        # Once the accepted release is committed, a drain failure must never
        # route traffic back to an old container that may already be stopped.
        self.say('Draining old color ' + self.current)
        self.run(DOCKER + ['update', '--restart=no', 'longdao-ai-' + self.current])
        self.run(DOCKER + ['stop', '-t', '120', 'longdao-ai-' + self.current], timeout=150)
        old_state = self.inspect('longdao-ai-' + self.current)['State']
        if old_state['Running'] or old_state.get('OOMKilled') or old_state['ExitCode'] != 0:
            write_json(self.deploy / 'last-release.json', {'version': version,
                       'image_id': image_info['Id'], 'color': self.target,
                       'result': 'active-drain-error', 'time_utc': self.timestamp,
                       'target_host': '121.37.128.126', 'runtime': SOCKET,
                       'backup': str(directory), 'old_exit_code': old_state['ExitCode']})
            raise RuntimeError('New release remains active; old color did not exit cleanly (code %s)' % old_state['ExitCode'])
        self.healthy(self.target_name)
        if self.edge_state() != self.sep_before:
            raise RuntimeError('SEP continuity check failed after drain')
        write_json(self.deploy / 'last-release.json', {'version': version, 'image_id': image_info['Id'],
                   'color': self.target, 'result': 'deployed', 'time_utc': self.timestamp,
                   'target_host': '121.37.128.126', 'runtime': SOCKET, 'backup': str(directory),
                   'sep_before': self.sep_before, 'sep_after': self.edge_state()})
        self.say('SUCCESS: ' + version + ' active on 121.37.128.126, color ' + self.target)


def main():
    os.umask(0o077)
    p = argparse.ArgumentParser()
    choice = p.add_mutually_exclusive_group(required=True)
    choice.add_argument('--check', action='store_true')
    choice.add_argument('--version')
    p.add_argument('--redeploy', action='store_true', help='Exercise a same-image rollout (operator only)')
    args = p.parse_args()
    # This check precedes even opening a log/lock under the data directory.
    subprocess.run(['/usr/bin/mountpoint', '-q', '/www'], check=True)
    with (ROOT / 'deploy/release.lock').open('w') as lock:
        try:
            fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
        except BlockingIOError:
            print('Another production release is running', file=sys.stderr)
            return 1
        release = Release()
        try:
            if args.check:
                release.preflight()
            else:
                release.deploy_version(args.version, redeploy=args.redeploy)
            return 0
        except Exception as e:
            release.say('ERROR: ' + str(e))
            return 1
        finally:
            release.log.close()


if __name__ == '__main__':
    sys.exit(main())

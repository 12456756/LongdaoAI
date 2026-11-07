#!/usr/bin/python3
"""Restricted SSH entry for both existing and future GitHub release workflows."""
import os
import re
import shlex
import subprocess
import sys

ROOT = '/www/longdao'
VERSION = re.compile(r'[0-9]+\.[0-9]+\.[0-9]+-[0-9]+')


def parse_command(command):
    if not command or any(c in command for c in '\r\n\x00'):
        raise ValueError('Invalid SSH command')
    words = shlex.split(command)
    if words == ['longdao-preflight']:
        return ['--check']
    if len(words) == 2 and words[0] == 'longdao-deploy':
        version = words[1]
    elif (len(words) == 4 and words[0] == 'DEPLOY_PATH=' + ROOT
          and words[1].startswith('VERSION=') and words[2:] == ['bash', '-s']):
        # Compatibility with already published release.yml files. Their stdin
        # contains the retired deployment script; never execute that script.
        version = words[1][len('VERSION='):]
    else:
        raise ValueError('Only Longdao release/preflight commands are permitted')
    if not VERSION.fullmatch(version):
        raise ValueError('Invalid release version')
    return ['--version', version]


def main():
    # Consume the old workflow's heredoc, including for rejected commands.
    # This prevents a blocked SSH writer and does not interpret any shell code.
    while sys.stdin.buffer.read(65536):
        pass
    try:
        args = parse_command(os.environ.get('SSH_ORIGINAL_COMMAND', ''))
    except ValueError as e:
        print(str(e), file=sys.stderr)
        return 2
    print('Longdao Huawei release entry: server-managed deployment', flush=True)
    env = {'PATH': '/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin',
           'HOME': '/root', 'LANG': 'C.UTF-8'}
    return subprocess.call(['/usr/bin/python3', ROOT + '/deploy/huawei_release.py'] + args,
                           env=env, cwd=ROOT)


if __name__ == '__main__':
    sys.exit(main())

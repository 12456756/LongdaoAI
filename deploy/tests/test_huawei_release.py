"""Safety regressions for the production-only Huawei release boundary."""
import copy
import importlib.util
import json
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch


def load(name):
    path = Path(__file__).resolve().parents[1] / 'production' / (name + '.py')
    spec = importlib.util.spec_from_file_location(name, path)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


entry = load('huawei_ci_entry')
release = load('huawei_release')
BLOCK = '# LONGDAO-AI-BEGIN\nai.longdaoec.com {\n reverse_proxy 172.19.0.1:18080 {\n }\n}\n# LONGDAO-AI-END'
SERVICE = {'image': 'old-image', 'container_name': 'longdao-ai-green',
           'environment': {'SERVER_PORT': '18080', 'JWT_SECRET': 'unchanged',
                           'DATABASE_PASSWORD': 'unchanged', 'REDIS_PASSWORD': 'unchanged'},
           'healthcheck': {'test': []}, 'network_mode': 'host',
           'volumes': ['/www/longdao/app-data:/app/data']}


class CommandBoundaryTests(unittest.TestCase):
    def test_current_workflow_command(self):
        self.assertEqual(entry.parse_command('DEPLOY_PATH=/www/longdao VERSION=2026.10.21-2 bash -s'),
                         ['--version', '2026.10.21-2'])

    def test_new_workflow_and_check(self):
        self.assertEqual(entry.parse_command('longdao-deploy 2026.10.21-2'), ['--version', '2026.10.21-2'])
        self.assertEqual(entry.parse_command('longdao-preflight'), ['--check'])

    def test_shell_and_other_target_rejected(self):
        for command in ['', 'bash -s', 'docker ps', 'longdao-deploy latest',
                        'longdao-deploy v2026.10.21-2', 'longdao-deploy 2026.10.21-2; id',
                        'longdao-deploy $(id)', 'longdao-deploy 2026.10.21-2\nid',
                        'DEPLOY_PATH=/opt/longdao VERSION=2026.10.21-2 bash -s',
                        'DEPLOY_PATH=/www/longdao VERSION=2026.10.21-2 bash -s extra']:
            with self.subTest(command=command), self.assertRaises(ValueError):
                entry.parse_command(command)


class RoutingTests(unittest.TestCase):
    def test_only_ai_block_changed(self):
        config = 'global\nSEP BEFORE\n' + BLOCK + '\nSEP AFTER\n'
        changed = release.replace_ai(config, BLOCK.replace(':18080', ':18081'))
        self.assertEqual(changed.replace(':18081', ':18080'), config)
        self.assertEqual(release.block_color(release.ai_block(changed)), 'blue')

    def test_ambiguous_and_missing_markers_rejected(self):
        for config in ['', BLOCK + BLOCK, '# LONGDAO-AI-END\n# LONGDAO-AI-BEGIN']:
            with self.subTest(config=config), self.assertRaises(RuntimeError):
                release.ai_block(config)

    def test_unexpected_route_rejected(self):
        for block in [BLOCK.replace(':18080', ':8080'), BLOCK.replace('ai.longdaoec.com', 'sep.longdaoec.com')]:
            with self.assertRaises(RuntimeError):
                release.block_color(block)

    def test_target_preserves_credentials_mounts_and_network(self):
        original = copy.deepcopy(SERVICE)
        target = release.target_service(SERVICE, 'blue', 'release-image')
        self.assertEqual(SERVICE, original)
        self.assertEqual(target['environment']['JWT_SECRET'], original['environment']['JWT_SECRET'])
        self.assertEqual(target['environment']['DATABASE_PASSWORD'], original['environment']['DATABASE_PASSWORD'])
        self.assertEqual(target['volumes'], original['volumes'])
        self.assertEqual(target['network_mode'], 'host')
        self.assertEqual(target['environment']['SERVER_PORT'], '18081')
        self.assertEqual(target['container_name'], 'longdao-ai-blue')


class FailurePathTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.root = Path(self.temp.name)
        for d in ['deploy', 'logs', 'backups']:
            (self.root / d).mkdir()
        self.caddy = self.root / 'Caddyfile'
        self.caddy.write_text(BLOCK)
        self.root_patch = patch.object(release, 'ROOT', self.root)
        self.caddy_patch = patch.object(release, 'CADDY', self.caddy)
        self.root_patch.start()
        self.caddy_patch.start()
        self.r = release.Release()
        self.r.config = {'name': 'longdao-ai', 'services': {'sub2api-green': copy.deepcopy(SERVICE)}}
        release.write_json(self.r.compose_path, self.r.config)
        self.r.old_block = BLOCK
        self.r.current, self.r.target = 'green', 'blue'
        self.r.current_service, self.r.target_name = 'sub2api-green', 'longdao-ai-blue'
        self.r.sep_before = {'SEP': 'unchanged'}
        self.r.preflight = lambda: None
        self.r.say = lambda message: None
        self.r.backup = lambda directory: directory.mkdir()
        self.r.edge_state = lambda: {'SEP': 'unchanged'}
        self.r.healthy = lambda name: {'Image': 'new-id'}
        self.r.inspect = lambda name: {'Image': 'old-id', 'State': {'Running': False, 'ExitCode': 0}}
        self.r.smoke = lambda *args: None
        self.r.catalog_before = 'catalog-hash'
        self.r.key_catalog = lambda *args: 'catalog-hash'
        self.commands = []
        self.composes = []
        self.r.run = lambda command, **kwargs: self.commands.append(command)
        self.r.capture = lambda command, **kwargs: json.dumps([{'Id': 'new-id', 'Os': 'linux', 'Architecture': 'amd64'}]) if 'image' in command else ''
        self.r.compose = lambda path, *args, **kwargs: self.composes.append(args)

    def tearDown(self):
        self.r.log.close()
        self.caddy_patch.stop()
        self.root_patch.stop()
        self.temp.cleanup()

    def test_migration_failure_never_switches_or_stops_old(self):
        def compose(path, *args, **kwargs):
            if args[0] == 'run':
                raise RuntimeError('migration failed')
        self.r.compose = compose
        self.r.switch_edge = lambda *args: self.fail('must not switch')
        with self.assertRaisesRegex(RuntimeError, 'migration failed'):
            self.r.deploy_version('2026.10.21-3')
        self.assertEqual(self.caddy.read_text(), BLOCK)
        self.assertFalse(any('stop' in c for c in self.commands))

    def test_reload_failure_restores_route_before_stopping_target(self):
        events = []
        def switch(expected, replacement, path):
            self.assertEqual(release.ai_block(self.caddy.read_text()), expected)
            self.caddy.write_text(replacement)
            events.append(replacement)
            if len(events) == 1:
                raise RuntimeError('reload failed')
        self.r.switch_edge = switch
        self.r.capture = lambda command, **kwargs: json.dumps([{'Id': 'new-id', 'Os': 'linux', 'Architecture': 'amd64'}]) if 'image' in command else 'target-id'
        with self.assertRaisesRegex(RuntimeError, 'reload failed'):
            self.r.deploy_version('2026.10.21-3')
        self.assertEqual(self.caddy.read_text(), BLOCK)
        stops = [c for c in self.commands if 'stop' in c]
        self.assertEqual(len(stops), 1)
        self.assertEqual(stops[0][-1], 'longdao-ai-blue')

    def test_success_never_manages_sep_or_databases(self):
        self.r.switch_edge = lambda expected, replacement, path: self.caddy.write_text(replacement)
        self.r.deploy_version('2026.10.21-3')
        self.assertEqual(release.block_color(self.caddy.read_text()), 'blue')
        state = release.read_json(self.r.deploy / 'last-release.json')
        self.assertEqual(state['result'], 'deployed')
        self.assertEqual(state['target_host'], '121.37.128.126')
        stops = [c for c in self.commands if 'stop' in c]
        self.assertEqual([c[-1] for c in stops], ['longdao-ai-green'])
        for command in self.commands:
            if command[:len(release.DOCKER)] != release.DOCKER:
                self.assertEqual(command[:len(release.EDGE_DOCKER)], release.EDGE_DOCKER)
                self.assertEqual(command[len(release.EDGE_DOCKER):len(release.EDGE_DOCKER) + 3],
                                 ['exec', 'sep-caddy', 'wget'])

    def test_drain_failure_keeps_new_route_and_configuration(self):
        self.r.switch_edge = lambda expected, replacement, path: self.caddy.write_text(replacement)
        def run(command, **kwargs):
            self.commands.append(command)
            if 'stop' in command:
                raise RuntimeError('drain failed')
        self.r.run = run
        with self.assertRaisesRegex(RuntimeError, 'drain failed'):
            self.r.deploy_version('2026.10.21-3')
        self.assertEqual(release.block_color(self.caddy.read_text()), 'blue')
        self.assertIn('sub2api-blue', release.read_json(self.r.compose_path)['services'])

    def test_unclean_old_exit_is_reported_without_rolling_back(self):
        self.r.switch_edge = lambda expected, replacement, path: self.caddy.write_text(replacement)
        self.r.inspect = lambda name: {'Image': 'old-id', 'State': {'Running': False, 'ExitCode': 1}}
        with self.assertRaisesRegex(RuntimeError, 'did not exit cleanly'):
            self.r.deploy_version('2026.10.21-3')
        self.assertEqual(release.block_color(self.caddy.read_text()), 'blue')
        self.assertEqual(release.read_json(self.r.deploy / 'last-release.json')['result'], 'active-drain-error')


if __name__ == '__main__':
    unittest.main()

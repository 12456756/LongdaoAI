import importlib.util
import json
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

SCRIPT=Path(__file__).resolve().parents[1]/'production/isolated_upgrade.py'
spec=importlib.util.spec_from_file_location('isolated_upgrade',SCRIPT)
upgrade=importlib.util.module_from_spec(spec)
spec.loader.exec_module(upgrade)


class IsolatedUpgradeTests(unittest.TestCase):
    def test_restore_drops_only_candidate_database(self):
        commands=[]
        with patch.object(upgrade,'compose'),patch.object(upgrade,'restore'),patch.object(upgrade,'run',side_effect=lambda command,**_:commands.append(command)):
            upgrade.restore_fresh(Path('/private/snapshot'))
        self.assertEqual(len(commands),2)
        self.assertTrue(all(upgrade.PG in command for command in commands))
        self.assertFalse(any('longdao-ai-postgres' in command for command in commands))
        self.assertIn('dropdb',commands[0])

    def test_write_open_boundary_refuses_stale_database_rollback(self):
        import sys
        from types import SimpleNamespace
        with tempfile.TemporaryDirectory() as directory:
            root=Path(directory)
            for phase in ('opening','live'):
                (root/'state.json').write_text(json.dumps({'phase':phase}))
                with patch.object(upgrade,'ENVROOT',root),patch.object(upgrade,'compose') as compose,patch.dict(sys.modules,{'huawei_release':SimpleNamespace()}):
                    with self.assertRaisesRegex(RuntimeError,'New production writes'):
                        upgrade.rollback()
                    compose.assert_not_called()

    def test_drain_probe_is_read_only(self):
        with patch.object(upgrade,'redis_cli',return_value='2') as redis:
            self.assertEqual(upgrade.active_requests(),2)
        args=redis.call_args.args
        self.assertEqual(args[:2],('longdao-ai-redis','EVAL'))
        for mutation in ('ZREM','DEL','EXPIRE','SET','ZADD'):
            self.assertNotIn("redis.call('"+mutation+"'",args[2])

    def test_price_import_does_not_invent_missing_video_tariff(self):
        with tempfile.TemporaryDirectory() as directory:
            root=Path(directory)
            (root/'legacy-effective-prices.json').write_text(json.dumps([{'model':'seedance-2.0'}]))
            (root/'pricing-inventory.json').write_text(json.dumps({'accounts':[{'id':1,'status':'active','model_mapping':{'seedance-2.0':'seedance-2.0'}}],'groups':[{'id':2}],'account_groups':[{'account_id':1,'group_id':2}]}))
            commands=[]
            with patch.object(upgrade,'ENVROOT',root),patch.object(upgrade,'run',side_effect=lambda *_,**kwargs:commands.append(kwargs['input'])):
                upgrade.price_import(root)
            report=json.loads((root/'price-import-report.json').read_text())
            self.assertEqual(report['imported_models'],[])
            self.assertEqual(report['omitted'][0]['reason'],'No explicit legacy price')
            self.assertEqual(commands,['BEGIN;\nCOMMIT;'])


if __name__=='__main__':
    unittest.main()

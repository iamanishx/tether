import importlib.util
from pathlib import Path
import tempfile
import unittest

spec=importlib.util.spec_from_file_location('tether_local',Path(__file__).with_name('local.py'))
local=importlib.util.module_from_spec(spec);spec.loader.exec_module(local)

class LocalConfigTests(unittest.TestCase):
    def recipe(self):
        services={n:{'environment':{},'logging':{}} for n in ['ready','host-setup','orchestrator','postgres','redis','api','clickhouse','vector','dashboard','dashboard-api','otel-collector']}
        services['ready']['depends_on']={'dashboard':{},'base-template':{}}
        return {'name':'e2b','services':services,'volumes':{'postgres':{'name':'e2b_postgres'}},'networks':{'default':{'name':'e2b_default'},'external':{'name':'owned-elsewhere','external':True}}}
    def test_headless_small_pool_and_nonshared_state(self):
        original=self.recipe();d=local.small_config(original,256)
        self.assertEqual(d['services']['host-setup']['environment']['HUGEPAGES'],'256')
        self.assertNotIn('dashboard',d['services']);self.assertNotIn('dashboard-api',d['services'])
        self.assertNotIn('dashboard',d['services']['ready']['depends_on'])
        self.assertEqual(d['volumes']['postgres']['name'],'tether-local_postgres')
        self.assertEqual(d['networks']['default']['name'],'tether-local_default')
        self.assertEqual(d['networks']['external']['name'],'owned-elsewhere')
        self.assertIn('huge_pages=off',d['services']['postgres']['command'])
        self.assertEqual(original['volumes']['postgres']['name'],'e2b_postgres')
        for service in d['services'].values():
            self.assertEqual(service['mem_limit'],service['memswap_limit'])
    def test_private_atomic_configuration(self):
        with tempfile.TemporaryDirectory() as d:
            p=Path(d)/'secret.json';local.private_json(p,{'token':'test-only'})
            self.assertEqual(p.stat().st_mode & 0o777,0o600)
            local.private_json(p,{'token':'replaced'})
            self.assertIn('replaced',p.read_text())
            self.assertEqual(len(list(Path(d).iterdir())),1)

if __name__=='__main__':unittest.main()

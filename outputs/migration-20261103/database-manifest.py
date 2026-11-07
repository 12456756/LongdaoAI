#!/usr/bin/env python3
"""Read-only, deterministic database manifest. Save output privately."""
import argparse
import hashlib
import json
import subprocess

p = argparse.ArgumentParser()
p.add_argument('--container', required=True)
p.add_argument('--database', default='sub2api')
p.add_argument('--port', default='5432')
p.add_argument('--socket')
a = p.parse_args()
docker = ['docker'] + (['-H', a.socket] if a.socket else [])
psql = docker + ['exec', a.container, 'psql', '-X', '-q', '-t', '-A',
                 '-h', '127.0.0.1', '-p', a.port, '-U', 'sub2api',
                 '-d', a.database, '-v', 'ON_ERROR_STOP=1']

def query(sql):
    return subprocess.check_output(psql + ['-c', sql], text=True).splitlines()

tables = query("SELECT tablename FROM pg_tables WHERE schemaname='public' ORDER BY 1")
sequences = query("SELECT sequencename FROM pg_sequences WHERE schemaname='public' ORDER BY 1")
sql = ['BEGIN ISOLATION LEVEL REPEATABLE READ READ ONLY;', "SET TIME ZONE 'UTC';"]
for table in tables:
    identifier = '"' + table.replace('"', '""') + '"'
    literal = "'" + table.replace("'", "''") + "'"
    sql.append(f"SELECT json_build_object('table',{literal},'count',count(*),"
               "'hash',md5(COALESCE(string_agg(md5(to_jsonb(t)::text),'' "
               f"ORDER BY md5(to_jsonb(t)::text)),''))) FROM public.{identifier} t;")
for seq in sequences:
    identifier = '"' + seq.replace('"', '""') + '"'
    literal = "'" + seq.replace("'", "''") + "'"
    sql.append(f"SELECT json_build_object('sequence',{literal},'last_value',last_value,"
               f"'is_called',is_called) FROM public.{identifier};")
sql.append('COMMIT;')
rows = [json.loads(line) for line in query('\n'.join(sql)) if line.strip()]
manifest = {'tables': [r for r in rows if 'table' in r],
            'sequences': [r for r in rows if 'sequence' in r]}
manifest['content_sha256'] = hashlib.sha256(
    json.dumps(manifest, sort_keys=True, separators=(',', ':')).encode()).hexdigest()
print(json.dumps(manifest, indent=2))

#!/usr/bin/env python3
"""Prepare and inspect a private production snapshot without changing live data."""
import argparse
import copy
import datetime
import hashlib
import json
import os
from pathlib import Path
import shutil
import subprocess
import sys
import time
import urllib.request
import urllib.error
import urllib.parse

ROOT = Path('/www/longdao')
DOCKER = ['/usr/bin/docker', '-H', 'unix:///run/longdao-docker.sock']
ENVROOT = ROOT / 'environments/2026.11.27-1'
COMPOSE = ENVROOT / 'compose.json'
PG = 'longdao-ai-postgres-next'
REDIS = 'longdao-ai-redis-next'
APP = 'longdao-ai-next'
NETWORK = 'longdao-upgrade-data'


def run(args, **kwargs):
    return subprocess.run(args, check=True, **kwargs)


def query(sql, container='longdao-ai-postgres', port=15432, database='sub2api'):
    return subprocess.check_output(DOCKER + ['exec', container, 'psql', '-X', '-v', 'ON_ERROR_STOP=1',
        '-h', '127.0.0.1', '-p', str(port), '-U', 'sub2api', '-d', database, '-Atc', sql], text=True)


def write_json(path, value):
    path.write_text(json.dumps(value, ensure_ascii=False, indent=2) + '\n')
    path.chmod(0o600)


def inventory():
    queries = {
        'groups': "SELECT COALESCE(json_agg(x),'[]') FROM (SELECT id,name,platform,rate_multiplier,image_rate_independent,image_rate_multiplier,video_rate_independent,video_rate_multiplier,image_price_1k,image_price_2k,image_price_4k,video_price_480p,video_price_720p,video_price_1080p,models_list_config FROM groups WHERE deleted_at IS NULL ORDER BY id) x",
        'accounts': "SELECT COALESCE(json_agg(x),'[]') FROM (SELECT id,name,platform,type,status,credentials->'model_mapping' AS model_mapping FROM accounts WHERE deleted_at IS NULL ORDER BY id) x",
        'channels': "SELECT COALESCE(json_agg(x),'[]') FROM (SELECT id,name,status,billing_model_source,model_mapping FROM channels ORDER BY id) x",
        'channel_groups': "SELECT COALESCE(json_agg(x),'[]') FROM channel_groups x",
        'account_groups': "SELECT COALESCE(json_agg(x),'[]') FROM account_groups x",
        'prices': "SELECT COALESCE(json_agg(x),'[]') FROM channel_model_pricing x",
        'intervals': "SELECT COALESCE(json_agg(x),'[]') FROM channel_pricing_intervals x",
        'recent_models': "SELECT COALESCE(json_agg(x),'[]') FROM (SELECT COALESCE(requested_model,model) AS model,group_id,billing_mode,count(*) AS calls FROM usage_logs WHERE created_at>NOW()-INTERVAL '30 days' GROUP BY 1,2,3 ORDER BY calls DESC) x",
        'settings': "SELECT COALESCE(json_agg(x),'[]') FROM (SELECT key,value FROM settings WHERE key IN ('platform_usd_to_cny_rate','BALANCE_RECHARGE_MULTIPLIER','SUBSCRIPTION_USD_TO_CNY_RATE','balance_display_cny_rate')) x",
        'key_counts': "SELECT COALESCE(json_agg(x),'[]') FROM (SELECT group_id,count(*) FROM api_keys WHERE deleted_at IS NULL GROUP BY group_id) x",
        'recent_missing_rates': "SELECT COALESCE(json_agg(x),'[]') FROM (SELECT COALESCE(requested_model,model) AS model,count(*) AS calls,max(created_at) AS latest,sum(total_cost) AS total_cost,max(input_cost/NULLIF(input_tokens,0)*1000000) AS max_input_mtok,min(input_cost/NULLIF(input_tokens,0)*1000000) AS min_input_mtok,max(output_cost/NULLIF(output_tokens,0)*1000000) AS max_output_mtok,min(output_cost/NULLIF(output_tokens,0)*1000000) AS min_output_mtok FROM usage_logs WHERE created_at>NOW()-INTERVAL '3 days' AND (COALESCE(requested_model,model) LIKE 'deepseek-v4.1%' OR COALESCE(requested_model,model) LIKE 'qwen%') GROUP BY 1) x",
    }
    result = {}
    for name, sql in queries.items():
        result[name] = json.loads(query(sql))
    return result


def layout():
    config = json.loads((ROOT / 'deploy/compose.json').read_text())
    return {name: {'image': service.get('image'), 'env_keys': list(service.get('environment', {})),
        'network_mode': service.get('network_mode'), 'command': service.get('command'),
        'mount_destinations': [v.split(':')[1] for v in service.get('volumes', [])]}
        for name, service in config['services'].items()}


def compose(*args):
    run(DOCKER + ['compose', '-p', 'longdao-upgrade', '-f', str(COMPOSE)] + list(args))


def prepare(image, directory):
    if ENVROOT.exists():
        raise RuntimeError('Candidate environment already exists; inspect before changing it')
    ENVROOT.mkdir(parents=True, mode=0o700)
    source = json.loads((ROOT / 'deploy/compose.json').read_text())
    app = copy.deepcopy(source['services']['sub2api-green'])
    postgres = copy.deepcopy(source['services']['postgres'])
    redis = copy.deepcopy(source['services']['redis'])
    for service in (app, postgres, redis):
        for field in ('network_mode', 'profiles', 'depends_on', 'ports', 'healthcheck'):
            service.pop(field, None)
        service['networks'] = ['data']
        service['restart'] = 'no'
    postgres['container_name'] = PG
    postgres['command'] = ['postgres', '-c', 'listen_addresses=*', '-c', 'port=5432', '-c', 'max_connections=150']
    postgres['volumes'] = [str(ENVROOT / 'postgres') + ':/var/lib/postgresql']
    redis['container_name'] = REDIS
    redis_config = (ROOT / 'deploy/redis.conf').read_text()
    import re
    redis_config = re.sub(r'^bind\s+.*$', 'bind 0.0.0.0', redis_config, flags=re.M)
    redis_config = re.sub(r'^port\s+.*$', 'port 6379', redis_config, flags=re.M)
    (ENVROOT / 'redis.conf').write_text(redis_config)
    (ENVROOT / 'redis.conf').chmod(0o600)
    os.chown(ENVROOT / 'redis.conf', 999, 999)
    redis['volumes'] = [str(ENVROOT / 'redis') + ':/data', str(ENVROOT / 'redis.conf') + ':/usr/local/etc/redis/redis.conf:ro']
    app['image'], app['container_name'] = image, APP
    app['ports'] = ['172.19.0.1:18082:8080']
    app['volumes'] = [str(ENVROOT / 'app-data') + ':/app/data', str(ENVROOT / 'video-inputs') + ':/app/media/video-inputs']
    app['environment'].update({'DATABASE_HOST': PG, 'DATABASE_PORT': '5432', 'REDIS_HOST': REDIS,
        'REDIS_PORT': '6379', 'SERVER_HOST': '0.0.0.0', 'SERVER_PORT': '8080', 'BATCH_IMAGE_QUEUE_ENABLED':'false'})
    for name in ('app-data', 'video-inputs', 'redis'):
        (ENVROOT / name).mkdir(mode=0o700)
    shutil.copytree(ROOT / 'app-data', ENVROOT / 'app-data', dirs_exist_ok=True, ignore=shutil.ignore_patterns('logs'))
    shutil.copytree(ROOT / 'media/video-inputs', ENVROOT / 'video-inputs', dirs_exist_ok=True)
    file_permissions()
    # No supplier, payment, mail or webhook traffic can leave this network.
    config = {'services': {'postgres': postgres, 'redis': redis, 'app': app},
        'networks': {'data': {'name': NETWORK, 'internal': True}}}
    write_json(COMPOSE, config)
    compose('up', '-d', 'postgres', 'redis')
    for attempt in range(90):
        status = subprocess.run(DOCKER + ['exec', PG, 'psql', '-h', '127.0.0.1', '-U', 'sub2api', '-d', 'sub2api', '-Atc', 'SELECT 1'], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        if status.returncode == 0: break
        time.sleep(1)
    else: raise RuntimeError('Candidate PostgreSQL did not become ready')
    restore(directory)
    print('Candidate data restored; isolated from supplier/payment networks', flush=True)


def restore(directory):
    with (directory / 'production.dump').open('rb') as stream:
        run(DOCKER + ['exec', '-i', PG, 'pg_restore', '-U', 'sub2api', '-d', 'sub2api',
            '--clean', '--if-exists', '--exit-on-error', '--no-owner'], stdin=stream)


def file_permissions():
    os.chown(ENVROOT / 'redis.conf', 999, 999)
    for name in ('app-data', 'video-inputs'):
        for path in [ENVROOT / name] + list((ENVROOT / name).rglob('*')):
            if not path.is_symlink(): os.chown(path, 1000, 1000)


def migrate():
    started = time.monotonic()
    compose('run', '--rm', '--no-deps', '-T', 'app', '/app/sub2api', '-migrate')
    print('Migration elapsed seconds: %.1f' % (time.monotonic() - started), flush=True)


def private_query(sql, container=PG, port=5432):
    return query(sql, container, port)


def price_import(directory):
    from decimal import Decimal
    prices = json.loads((directory / 'legacy-effective-prices.json').read_text())
    inv = json.loads((directory / 'pricing-inventory.json').read_text())
    accounts = inv['accounts']
    groups = {g['id']: g for g in inv['groups']}
    membership = {}
    for row in inv['account_groups']:
        membership.setdefault(row['account_id'], []).append(groups.get(row['group_id']))
    def value(number, factor=1): return format(Decimal(str(number)) * Decimal(str(factor)), 'f')
    def sql_value(v): return "'" + str(v).replace("'", "''") + "'"
    statements = ['BEGIN;']
    omitted, inserted = [], []
    for entry in prices:
        model = entry['model']
        candidates = [a for a in accounts if model in (a.get('model_mapping') or {})]
        if not candidates:
            omitted.append({'model': model, 'reason': 'No current account explicitly advertises this historical model'})
            continue
        account = sorted(candidates, key=lambda a: (a['status'] != 'active', a['id']))[0]
        p = {'model': model, 'upstream_model': account['model_mapping'][model], 'currency': 'USD',
             'discount': '1', 'markup': '1', 'items': [], 'source_account_name': 'Migrated legacy effective price (not supplier procurement quote)'}
        def item(kind, tier, unit, amount): p['items'].append({'kind':kind, 'tier':tier, 'unit':'USD/'+unit, 'amount':amount})
        if entry.get('token'):
            p['mode'] = 'token'
            token = entry['token']
            for kind, key in [('input','InputPricePerToken'),('output','OutputPricePerToken'),('cache_read','CacheReadPricePerToken'),('cache_write','CacheCreationPricePerToken'),('image_input','ImageInputPricePerToken'),('image_output','ImageOutputPricePerToken')]:
                amount = token.get(key, 0)
                if not amount and kind == 'image_input': amount = token.get('InputPricePerToken',0)
                if not amount and kind == 'image_output': amount = token.get('OutputPricePerToken',0)
                item(kind, '', 'MTok', value(amount, 1000000))
            for kind, key in [('input','InputPricePerTokenPriority'),('output','OutputPricePerTokenPriority'),('cache_read','CacheReadPricePerTokenPriority'),('cache_write','CacheCreationPricePerTokenPriority')]:
                if token.get(key, 0) > 0: item(kind, 'priority', 'MTok', value(token[key], 1000000))
            if token.get('SupportsCacheBreakdown'):
                item('cache_write','5m','MTok',value(token['CacheCreation5mPrice'],1000000))
                item('cache_write','1h','MTok',value(token['CacheCreation1hPrice'],1000000))
            if token.get('LongContextInputThreshold', 0) > 0:
                p['policy'] = {'long_context_threshold':token['LongContextInputThreshold'], 'long_context_input_multiplier':token['LongContextInputMultiplier'], 'long_context_output_multiplier':token['LongContextOutputMultiplier']}
        elif entry.get('images'):
            p['mode'] = 'image'
            for tier, amount in entry['images'].items(): item('image',tier,'image',amount)
        elif 'seedance' in model or model == 'minimax-h3':
            p['mode'] = 'video'
            # Only copy explicit existing group prices. A missing tariff stays missing.
            for tier in ('480p','720p','1080p'):
                values = {str(g['video_price_'+tier]) for a in candidates for g in membership.get(a['id'], []) if g and g.get('video_price_'+tier) is not None}
                if len(values) > 1: raise RuntimeError('Conflicting group prices for '+model+'/'+tier)
                if values: item('video',tier,'second',value(next(iter(values))))
        else:
            omitted.append({'model': model, 'reason': entry.get('error', 'No legacy tariff that can be migrated without inventing a price')})
            continue
        if not p['items']:
            omitted.append({'model': model, 'reason': 'No explicit legacy price'})
            continue
        data = sql_value(json.dumps(p))
        m = sql_value(model)
        statements.append(f"INSERT INTO account_model_prices(account_id,model,price) VALUES({account['id']},{m},{data}::jsonb);")
        statements.append(f"INSERT INTO model_base_prices(model,source_price_id,source_revision,version,price,updated_by) SELECT {m},id,revision,1,price,0 FROM account_model_prices WHERE account_id={account['id']} AND model={m};")
        statements.append(f"INSERT INTO model_base_price_history(model,version,source_price_id,source_revision,price,updated_by) SELECT model,version,source_price_id,source_revision,price,updated_by FROM model_base_prices WHERE model={m};")
        inserted.append(model)
    statements.append('COMMIT;')
    sql = '\n'.join(statements)
    run(DOCKER + ['exec','-i',PG,'psql','-X','-v','ON_ERROR_STOP=1','-U','sub2api','-d','sub2api'], input=sql, text=True, stdout=subprocess.DEVNULL)
    report = {'imported_models': inserted, 'omitted': omitted, 'source': 'Existing effective legacy prices; discount=1, markup=1, not inferred supplier cost'}
    write_json(ENVROOT / 'price-import-report.json', report)
    print(json.dumps({'imported':len(inserted), 'omitted':omitted},ensure_ascii=False),flush=True)


def live_prices(directory):
    config = json.loads((ROOT / 'deploy/compose.json').read_text())
    env = config['services']['sub2api-green']['environment']
    opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))
    body = json.dumps({'email':env['ADMIN_EMAIL'], 'password':env['ADMIN_PASSWORD']}).encode()
    req = urllib.request.Request('http://172.19.0.1:18080/api/v1/auth/login', data=body, headers={'Content-Type':'application/json'})
    try:
        with opener.open(req,timeout=15) as response: login = json.load(response)
        token = login.get('data',{}).get('access_token')
        if not token:
            print('Stored bootstrap credential cannot supply an admin session; no credential change attempted')
            return
    except urllib.error.HTTPError as error:
        print('Stored bootstrap admin login returned HTTP %s; no retry or credential change attempted' % error.code)
        return
    entries = json.loads((directory / 'legacy-effective-prices.json').read_text())
    results = []
    for entry in entries:
        if not entry.get('error'): continue
        url = 'http://172.19.0.1:18080/api/v1/admin/channels/model-pricing?' + urllib.parse.urlencode({'model':entry['model']})
        request = urllib.request.Request(url,headers={'Authorization':'Bearer '+token})
        with opener.open(request,timeout=15) as response: result=json.load(response).get('data',{})
        results.append({'model':entry['model'], 'price':result})
    write_json(directory / 'live-memory-prices.json',results)
    print(json.dumps(results,ensure_ascii=False))


def smoke(base):
    opener=urllib.request.build_opener(urllib.request.ProxyHandler({}))
    with opener.open(base+'/health', timeout=10) as response:
        if response.status != 200: raise RuntimeError('Health check failed')
    with opener.open(base+'/api/v1/settings/public', timeout=10) as response:
        settings=json.load(response)
    # Only authenticated read-only catalog/quote requests; no model creation.
    key_rows=json.loads(query("SELECT COALESCE(json_agg(x),'[]') FROM (SELECT DISTINCT ON(group_id) id,group_id,key FROM api_keys WHERE deleted_at IS NULL AND status='active' AND group_id IN (3,43,44,47,51,52) ORDER BY group_id,id) x"))
    results=[]
    for row in key_rows:
        auth={'Authorization':'Bearer '+row['key']}
        with opener.open(urllib.request.Request(base+'/v1/models',headers=auth),timeout=15) as response:
            data=json.load(response)
        record={'group_id':row['group_id'],'key_id':row['id'],'models':sorted(x['id'] for x in data.get('data',[]))}
        if row['group_id'] in (44,47):
            video=row['group_id']==44
            path='/v1/sub2api/videos/pricing' if video else '/v1/sub2api/images/pricing'
            body={'model':'seedance-2.0','operation':'image-to-video','resolution':'720p','duration_seconds':4} if video else {'model':'gpt-image-2-1k','operation':'edit','size':'1024x1024','quality':'high','n':1}
            request=urllib.request.Request(base+path,data=json.dumps(body).encode(),headers=dict(auth,**{'Content-Type':'application/json'}))
            with opener.open(request,timeout=15) as response: quote=json.load(response)
            record['quote']={name:quote.get(name) for name in ('model','amount_usd','pricing_status','effective_rate_multiplier','billing_mode')}
        results.append(record)
    return results


def candidate_base():
    details=json.loads(subprocess.check_output(DOCKER+['inspect',APP],text=True))[0]
    address=details['NetworkSettings']['Networks'][NETWORK]['IPAddress']
    return 'http://'+address+':8080'


def baseline(directory):
    run(DOCKER+['exec',PG,'createdb','-U','sub2api','upgrade_baseline'])
    with (directory/'production.dump').open('rb') as stream:
        run(DOCKER+['exec','-i',PG,'pg_restore','-U','sub2api','-d','upgrade_baseline','--exit-on-error','--no-owner'],stdin=stream)


def compare_data():
    from decimal import Decimal
    def canonical(v):
        if isinstance(v,bool) or v is None: return v
        if isinstance(v,(Decimal,int,float)): return format(Decimal(str(v)).normalize(),'f')
        if isinstance(v,dict): return {k:canonical(x) for k,x in v.items()}
        if isinstance(v,list): return [canonical(x) for x in v]
        return v
    def signature(database,table):
        primary=query("SELECT string_agg(quote_ident(a.attname),',' ORDER BY k.ord) FROM pg_index i JOIN LATERAL unnest(i.indkey) WITH ORDINALITY k(attnum,ord) ON true JOIN pg_attribute a ON a.attrelid=i.indrelid AND a.attnum=k.attnum WHERE i.indrelid='public."+table+"'::regclass AND i.indisprimary",PG,5432,database).strip()
        if not primary: primary='to_jsonb(t)::text'
        projection="to_jsonb(t)-'upstream_company_id'"
        if table=='api_keys': projection+="-'last_used_at'-'updated_at'"
        sql="SET timezone='UTC'; SELECT "+projection+' FROM public.'+table+' t ORDER BY '+primary
        proc=subprocess.Popen(DOCKER+['exec',PG,'psql','-X','-qAt','-U','sub2api','-d',database,'-c',sql],stdout=subprocess.PIPE,text=True)
        digest=hashlib.sha256(); count=0
        for line in proc.stdout:
            value=json.loads(line,parse_float=Decimal)
            digest.update(json.dumps(canonical(value),sort_keys=True,separators=(',',':')).encode())
            digest.update(b'\n'); count+=1
        if proc.wait(): raise RuntimeError('Data comparison query failed')
        return {'rows':count,'sha256':digest.hexdigest()}
    results=[]
    for table in ('users','api_keys','groups','accounts','payment_orders','redeem_codes','user_subscriptions','user_group_rate_multipliers','usage_logs','usage_billing_dedup','billing_usage_entries'):
        old=signature('upgrade_baseline',table)
        new=signature('sub2api',table)
        results.append({'table':table,'before':old,'after':new,'equal':old==new})
        print('Data comparison: '+table+' '+('PASS' if old==new else 'FAIL'),flush=True)
    write_json(ENVROOT/'data-comparison.json',results)
    if not all(r['equal'] for r in results): raise RuntimeError('Candidate business records differ from backup baseline')


def copy_runtime(directory):
    compose('stop','-t','120','app','redis')
    snapshot_name='.upgrade-20261127.rdb'
    run(DOCKER+['exec','longdao-ai-redis','redis-cli','-p','16379','--rdb','/data/'+snapshot_name],stdout=subprocess.DEVNULL)
    shutil.copyfile(ROOT/'redis'/snapshot_name,directory/'redis.rdb')
    retained=ENVROOT/('redis-before-'+datetime.datetime.now(datetime.timezone.utc).strftime('%Y%m%dT%H%M%S%fZ'))
    (ENVROOT/'redis').rename(retained)
    (ENVROOT/'redis').mkdir(mode=0o700)
    shutil.copyfile(directory/'redis.rdb',ENVROOT/'redis/dump.rdb')
    os.chown(ENVROOT/'redis',999,1000)
    os.chown(ENVROOT/'redis/dump.rdb',999,1000)
    for source,target in [(ROOT/'app-data',ENVROOT/'app-data'),(ROOT/'media/video-inputs',ENVROOT/'video-inputs')]:
        retained=target.with_name(target.name+'-before-'+datetime.datetime.now(datetime.timezone.utc).strftime('%Y%m%dT%H%M%S%fZ'))
        target.rename(retained)
        shutil.copytree(source,target,ignore=shutil.ignore_patterns('logs'))
    file_permissions()
    import re
    config_path=ENVROOT/'redis.conf'
    config=config_path.read_text()
    config_path.write_text(re.sub(r'^appendonly\s+.*$','appendonly no',config,flags=re.M))
    compose('up','-d','redis')
    for _ in range(30):
        result=subprocess.run(DOCKER+['exec',REDIS,'redis-cli','ping'],stdout=subprocess.DEVNULL,stderr=subprocess.DEVNULL)
        # Authentication is checked by the application; startup requires a running process.
        state=json.loads(subprocess.check_output(DOCKER+['inspect',REDIS],text=True))[0]['State']
        if state['Running']: break
        time.sleep(1)
    for _ in range(30):
        try:
            if redis_cli(REDIS,'PING')=='PONG': break
        except subprocess.CalledProcessError: pass
        time.sleep(1)
    else: raise RuntimeError('Restored Redis did not become ready')
    compare_redis()
    if redis_cli(REDIS,'CONFIG','SET','appendonly','yes')!='OK': raise RuntimeError('Could not enable AOF after RDB restore')
    for _ in range(60):
        info=redis_cli(REDIS,'INFO','persistence')
        if 'aof_rewrite_in_progress:0' in info and 'aof_last_bgrewrite_status:ok' in info: break
        time.sleep(1)
    else: raise RuntimeError('Restored Redis AOF rewrite did not finish')
    config_path.write_text(config)
    print('Redis snapshot and private file copies prepared; original runtime preserved',flush=True)


def export_prices(directory):
    rows=json.loads(private_query('SELECT COALESCE(json_agg(x),\'[]\') FROM model_base_prices x'))
    write_json(directory/'managed-base-prices.json',rows)
    print('Exported '+str(len(rows))+' non-secret price snapshots')


def provider_configuration():
    row=json.loads(private_query("SELECT json_build_object('id',id,'name',name,'base_url',credentials->>'base_url','image_protocol',credentials->>'image_protocol','video_protocol',credentials->>'video_protocol') FROM accounts WHERE id=52 AND deleted_at IS NULL"))
    if row['name']!='脑生云gpt-image' or urllib.parse.urlsplit(row['base_url']).hostname not in ('ai.naoshengyun.com','api.naoshengyun.com'):
        raise RuntimeError('Expected Naoshengyun image account identity does not match: name='+row['name']+', host='+str(urllib.parse.urlsplit(row['base_url']).hostname)+'; no protocol change applied')
    private_query("UPDATE accounts SET credentials=jsonb_set(credentials,'{image_protocol}','\"naoshengyun_gpt_async\"'::jsonb),updated_at=NOW() WHERE id=52")
    write_json(ENVROOT/'provider-config-change.json',{'account_id':52,'field':'image_protocol','before':row['image_protocol'],'after':'naoshengyun_gpt_async','only_candidate_database':True})
    print('Candidate account 52 configured for Naoshengyun GPT asynchronous images',flush=True)


def redis_cli(container, *args):
    port='16379' if container=='longdao-ai-redis' else '6379'
    return subprocess.check_output(DOCKER+['exec',container,'redis-cli','--raw','-p',port]+list(args),text=True).strip()


def compare_redis():
    script="""local cursor='0';local out={};repeat local page=redis.call('SCAN',cursor,'COUNT',100);cursor=page[1];for _,k in ipairs(page[2]) do
if string.match(k,'^video:') or string.match(k,'^image_task') or string.match(k,'^media:') then
local v;local typ=redis.call('TYPE',k).ok;
if typ=='string' then v=redis.call('GET',k)
elseif typ=='hash' then local fields=redis.call('HKEYS',k);table.sort(fields);local ordered={};for _,field in ipairs(fields) do table.insert(ordered,field);table.insert(ordered,redis.call('HGET',k,field)) end;v=cjson.encode(ordered)
elseif typ=='zset' then v=cjson.encode(redis.call('ZRANGE',k,0,-1,'WITHSCORES'))
elseif typ=='set' then local members=redis.call('SMEMBERS',k);table.sort(members);v=cjson.encode(members)
elseif typ=='list' then v=cjson.encode(redis.call('LRANGE',k,0,-1))
else v=redis.call('DUMP',k) end;
if v then out[k]={hash=redis.sha1hex(v),expires=redis.call('EXPIRETIME',k)} end end end until cursor=='0';return cjson.encode(out)"""
    before=json.loads(redis_cli('longdao-ai-redis','EVAL',script,'0'))
    after=json.loads(redis_cli(REDIS,'EVAL',script,'0'))
    now=int(time.time())
    errors=[]
    for key,value in before.items():
        if value['expires']!=-1 and value['expires']<=now: continue
        if after.get(key)!=value: errors.append({'key_hash':hashlib.sha256(key.encode()).hexdigest(),'value_equal':after.get(key,{}).get('hash')==value['hash'],'expiry_before':value['expires'],'expiry_after':after.get(key,{}).get('expires')})
    write_json(ENVROOT/'redis-comparison.json',{'source_media_keys':len(before),'restored_media_keys':len(after),'different_key_hashes':errors,'passed':not errors})
    if errors: raise RuntimeError('Restored Redis media records or expiration times differ from original')
    print('Redis task records and expiration comparison: PASS (%d keys)' % len(before),flush=True)


def active_requests():
    script="local cursor='0';local total=0;local now=tonumber(redis.call('TIME')[1]);repeat local page=redis.call('SCAN',cursor,'MATCH','concurrency:user:*','COUNT',100);cursor=page[1];for _,k in ipairs(page[2]) do if string.match(k,'^concurrency:user:%d+$') then total=total+redis.call('ZCOUNT',k,now-900,'+inf') end end until cursor=='0';return total"
    return int(redis_cli('longdao-ai-redis','EVAL',script,'0'))


def state(phase, **extra):
    write_json(ENVROOT/'state.json',dict(phase=phase,at=datetime.datetime.now(datetime.timezone.utc).isoformat(),**extra))
    print('PHASE '+phase,flush=True)


def core_summary(container,port):
    summary={}
    for table in ('users','api_keys','groups','accounts','payment_orders','redeem_codes','user_subscriptions','user_group_rate_multipliers','usage_logs','usage_billing_dedup','billing_usage_entries'):
        summary[table]=int(query('SELECT count(*) FROM '+table,container,port))
    summary['funds']=json.loads(query("SELECT json_build_object('balance',sum(balance)::text,'frozen',sum(frozen_balance)::text) FROM users",container,port))
    summary['usage']=json.loads(query("SELECT json_build_object('total',sum(total_cost)::text,'actual',sum(actual_cost)::text,'input',sum(input_tokens)::text,'output',sum(output_tokens)::text) FROM usage_logs",container,port))
    return summary


def restore_fresh(directory):
    compose('stop','-t','120','app')
    run(DOCKER+['exec',PG,'dropdb','--force','-U','sub2api','sub2api'])
    run(DOCKER+['exec',PG,'createdb','-U','sub2api','sub2api'])
    restore(directory)


def wait_candidate():
    for _ in range(90):
        try:
            with urllib.request.build_opener(urllib.request.ProxyHandler({})).open(candidate_base()+'/health',timeout=3) as response:
                if response.status==200: return
        except (OSError,urllib.error.URLError): pass
        time.sleep(2)
    raise RuntimeError('Candidate health did not become ready')


def edge_check():
    edge=['docker','-H','unix:///var/run/docker.sock']
    run(edge+['exec','sep-caddy','wget','-q','-T','10','-O','/dev/null','http://172.19.0.1:18082/health'])


def validate_maintenance():
    import huawei_release as release
    before=release.CADDY.read_text()
    block=release.ai_block(before)
    gate='''ai.longdaoec.com {
    @longdao_upgrade {
        not path /health /media/video-inputs/*
    }
    handle @longdao_upgrade {
        header Content-Type application/json
        header Retry-After 180
        respond `{"error":{"source":"platform","code":"platform_maintenance","message":"Platform upgrade in progress; retry later"},"retryable":true}` 503
    }
'''
    candidate=before.replace(block,block.replace('ai.longdaoec.com {',gate,1),1)
    path=ENVROOT/'Caddyfile.validate.private'
    path.write_text(candidate)
    edge=['docker','-H','unix:///var/run/docker.sock']
    run(edge+['cp',str(path),'sep-caddy:/tmp/longdao-isolated-maintenance.caddy'])
    run(edge+['exec','sep-caddy','caddy','validate','--config','/tmp/longdao-isolated-maintenance.caddy','--adapter','caddyfile'])
    print('Maintenance gate validated without changing active routing',flush=True)


def launch():
    unit='longdao-isolated-upgrade-20261127'
    log=ENVROOT/'cutover.log'
    command=['systemd-run','--unit='+unit,'--property=Type=exec','--property=RuntimeMaxSec=1200',
        '--property=TimeoutStopSec=180','--property=StandardOutput=append:'+str(log),
        '--property=StandardError=append:'+str(log),
        '--property=ExecStopPost=/usr/bin/python3 /www/longdao/deploy/isolated_upgrade.py recover',
        '/usr/bin/python3','/www/longdao/deploy/isolated_upgrade.py','cutover',
        '--directory','/www/longdao/upgrades/20261127-final']
    run(command)
    print('Independent server job started: '+unit+'; log '+str(log),flush=True)


def recover():
    path=ENVROOT/'state.json'
    if not path.exists(): return
    phase=json.loads(path.read_text())['phase']
    if phase in ('live','opening','rolled_back'): return
    rollback()


def rehearse_recovery():
    import huawei_release as release
    if (ENVROOT/'state.json').exists(): raise RuntimeError('Rehearsal state already exists; do not overwrite a release state')
    current=release.CADDY.read_text()
    if release.block_color(release.ai_block(current))!='green': raise RuntimeError('Expected original green environment')
    (ENVROOT/'Caddyfile.before.private').write_text(current)
    shutil.copyfile(ROOT/'deploy/compose.json',ENVROOT/'compose.before.private.json')
    shutil.copyfile(ROOT/'deploy/app-env.json',ENVROOT/'app-env.before.private.json')
    state('recovery_rehearsal')
    rollback()
    if release.CADDY.read_text()!=current: raise RuntimeError('Recovery rehearsal changed the original routing')
    print('Recovery rehearsal PASS: original application and routing retained; no original database restore',flush=True)


def rollback(force=False):
    import huawei_release as release
    stored=ENVROOT/'state.json'
    phase=json.loads(stored.read_text())['phase'] if stored.exists() else ''
    if phase in ('live','opening') and not force:
        raise RuntimeError('New production writes are open; preserve and reconcile new data before rollback')
    compose('stop','-t','120','app')
    run(DOCKER+['start','longdao-ai-green'])
    for _ in range(60):
        try:
            smoke('http://172.19.0.1:18080')
            break
        except Exception: time.sleep(2)
    else: raise RuntimeError('Old environment is not healthy; maintenance remains in place')
    operator=release.Release()
    try:
        old=(ENVROOT/'Caddyfile.before.private').read_text()
        current=release.ai_block(release.CADDY.read_text())
        operator.switch_edge(current,release.ai_block(old),ENVROOT/'Caddyfile.recovery.private')
        shutil.copyfile(ENVROOT/'compose.before.private.json',ROOT/'deploy/compose.json')
        shutil.copyfile(ENVROOT/'app-env.before.private.json',ROOT/'deploy/app-env.json')
    finally: operator.log.close()
    state('rolled_back')


def cutover(directory):
    import huawei_release as release
    checks=json.loads((ENVROOT/'data-comparison.json').read_text())
    if not checks or not all(x['equal'] for x in checks): raise RuntimeError('Full rehearsal data comparison has not passed')
    proof=json.loads((ENVROOT/'price-validation.json').read_text())
    if proof.get('passed') is not True: raise RuntimeError('Offline price comparison has not passed')
    pending=int(query("SELECT count(*) FROM batch_image_jobs WHERE status NOT IN ('completed','failed','cancelled','expired')"))
    if pending: raise RuntimeError('Pending batch image work requires a separate drain before this upgrade')
    if active_requests()>0:
        print('Active requests found; maintenance will drain them before taking the final snapshot',flush=True)
    operator=release.Release()
    sep_before=operator.edge_state()
    before=release.CADDY.read_text()
    old_block=release.ai_block(before)
    if release.block_color(old_block)!='green': raise RuntimeError('Expected original green environment to be active')
    (ENVROOT/'Caddyfile.before.private').write_text(before)
    shutil.copyfile(ROOT/'deploy/compose.json',ENVROOT/'compose.before.private.json')
    shutil.copyfile(ROOT/'deploy/app-env.json',ENVROOT/'app-env.before.private.json')
    gate='''ai.longdaoec.com {
    @longdao_upgrade {
        not path /health /media/video-inputs/*
    }
    handle @longdao_upgrade {
        header Content-Type application/json
        header Retry-After 180
        respond `{"error":{"source":"platform","code":"platform_maintenance","message":"Platform upgrade in progress; retry later"},"retryable":true}` 503
    }
'''
    gated=old_block.replace('ai.longdaoec.com {',gate,1)
    if gated==old_block: raise RuntimeError('Could not construct maintenance gate')
    state('preparing_cutover')
    try:
        operator.switch_edge(old_block,gated,ENVROOT/'Caddyfile.maintenance.private')
        state('maintenance')
        deadline=time.monotonic()+300
        while True:
            active=active_requests()
            print('Draining active model requests: '+str(active),flush=True)
            if active==0: break
            if time.monotonic()>deadline: raise RuntimeError('Existing model requests did not drain; reverting maintenance without migrating production')
            time.sleep(5)
        run(DOCKER+['stop','-t','120','longdao-ai-green'])
        old=json.loads(subprocess.check_output(DOCKER+['inspect','longdao-ai-green'],text=True))[0]
        if old['State']['ExitCode']!=0: raise RuntimeError('Original backend did not exit cleanly')
        state('final_snapshot')
        snapshot(directory)
        old_summary=core_summary('longdao-ai-postgres',15432)
        write_json(directory/'original-core-summary.json',old_summary)
        shutil.copyfile(ROOT/'upgrades/20261127-rehearsal/legacy-effective-prices.json',directory/'legacy-effective-prices.json')
        if hashlib.sha256((ROOT/'app-data/model_pricing.json').read_bytes()).hexdigest()!=proof['legacy_price_sha256']:
            raise RuntimeError('Legacy pricing file changed after validation; prepare a fresh export')
        compose('stop','-t','120','app')
        restore_fresh(directory)
        migrate()
        price_import(directory)
        if core_summary(PG,5432)!=old_summary: raise RuntimeError('Final restored balances, records or usage differ from original')
        provider_configuration()
        copy_runtime(directory)
        state('verifying')
        compose('up','-d','app')
        wait_candidate()
        operator.smoke(candidate_base(),'2026.11.27-1')
        old_smoke=json.loads((ROOT/'upgrades/20261127-rehearsal/smoke-18080.json').read_text())
        candidate=smoke(candidate_base())
        if candidate!=old_smoke: raise RuntimeError('Candidate model catalog or media prices differ from the verified old environment')
        # Outbound connectivity is enabled only after the isolated checks pass.
        existing=subprocess.check_output(DOCKER+['network','ls','--format','{{.Name}}'],text=True).splitlines()
        if 'longdao-upgrade-egress' not in existing: run(DOCKER+['network','create','longdao-upgrade-egress'])
        config=json.loads(COMPOSE.read_text())
        config['networks']['egress']={'name':'longdao-upgrade-egress','external':True}
        config['services']['app']['networks']=['data','egress']
        original_config=json.loads((ENVROOT/'compose.before.private.json').read_text())
        original_queue=original_config['services']['sub2api-green']['environment'].get('BATCH_IMAGE_QUEUE_ENABLED')
        if original_queue is None: config['services']['app']['environment'].pop('BATCH_IMAGE_QUEUE_ENABLED',None)
        else: config['services']['app']['environment']['BATCH_IMAGE_QUEUE_ENABLED']=original_queue
        for service in config['services'].values(): service['restart']='unless-stopped'
        write_json(COMPOSE,config)
        compose('up','-d','--no-deps','--force-recreate','app')
        wait_candidate()
        edge_check()
        if smoke('http://172.19.0.1:18082')!=old_smoke: raise RuntimeError('Candidate published endpoint differs from verified pricing/catalog')
        if operator.edge_state()!=sep_before: raise RuntimeError('SEP process continuity changed during upgrade')
        # Persist the write-open boundary before removing maintenance. Failures
        # beyond this point require reconciliation rather than stale-DB rollback.
        write_json(ROOT/'deploy/compose.json',config)
        write_json(ROOT/'deploy/app-env.json',config['services']['app']['environment'])
        for container in (APP,PG,REDIS): run(DOCKER+['update','--restart=unless-stopped',container],stdout=subprocess.DEVNULL)
        run(DOCKER+['update','--restart=no','longdao-ai-green'],stdout=subprocess.DEVNULL)
        state('opening',original_environment_retained=True)
        target_block=old_block.replace('172.19.0.1:18080','172.19.0.1:18082')
        operator.switch_edge(gated,target_block,ENVROOT/'Caddyfile.active.private')
        state('live',version='2026.11.27-1',image=config['services']['app']['image'],original_environment_retained=True)
        write_json(ROOT/'deploy/isolated-active.json',{'environment':str(ENVROOT),'version':'2026.11.27-1','port':18082,'app':APP,'postgres':PG,'redis':REDIS})
        write_json(ROOT/'deploy/last-release.json',{'version':'2026.11.27-1','image':config['services']['app']['image'],'environment':str(ENVROOT),'result':'deployed-isolated','time_utc':datetime.datetime.now(datetime.timezone.utc).isoformat(),'backup':str(directory),'sep_before':sep_before,'sep_after':operator.edge_state()})
        print('SUCCESS: new backend, PostgreSQL, Redis and private files are active; original environment retained',flush=True)
    except BaseException:
        phase=json.loads((ENVROOT/'state.json').read_text())['phase']
        if phase not in ('live','opening'):
            rollback()
        else:
            print('Production write-open boundary reached; retained new data and refused stale database rollback',flush=True)
        raise
    finally: operator.log.close()


def snapshot(directory):
    directory.mkdir(parents=True, exist_ok=False, mode=0o700)
    with (directory / 'production.dump').open('wb') as stream:
        run(DOCKER + ['exec', 'longdao-ai-postgres', 'pg_dump', '-h', '127.0.0.1', '-p', '15432',
            '-U', 'sub2api', '-d', 'sub2api', '-Fc'], stdout=stream)
    with (directory / 'production.dump').open('rb') as stream:
        run(DOCKER + ['exec', '-i', 'longdao-ai-postgres', 'pg_restore', '--list'], stdin=stream, stdout=subprocess.DEVNULL)
    write_json(directory / 'pricing-inventory.json', inventory())
    print('Snapshot completed: ' + str(directory), flush=True)


def main():
    os.umask(0o077)
    parser = argparse.ArgumentParser()
    parser.add_argument('action', choices=['inventory', 'snapshot', 'layout', 'prepare', 'restore', 'migrate', 'prices','live-prices','smoke','start','permissions','baseline','compare','runtime','export-prices','cutover','rollback','providers','validate-maintenance','launch','recover','rehearse-recovery'])
    parser.add_argument('--directory')
    parser.add_argument('--image')
    parser.add_argument('--base', default='candidate')
    args = parser.parse_args()
    if args.action=='rehearse-recovery':
        rehearse_recovery()
    elif args.action=='launch':
        launch()
    elif args.action=='recover':
        recover()
    elif args.action=='validate-maintenance':
        validate_maintenance()
    elif args.action in ('cutover','rollback'):
        import fcntl
        with (ROOT/'deploy/release.lock').open('w') as lock:
            fcntl.flock(lock,fcntl.LOCK_EX|fcntl.LOCK_NB)
            if args.action=='cutover': cutover(Path(args.directory))
            else: rollback()
    elif args.action == 'providers':
        provider_configuration()
    elif args.action == 'runtime':
        copy_runtime(Path(args.directory))
    elif args.action == 'export-prices':
        export_prices(Path(args.directory))
    elif args.action == 'baseline':
        baseline(Path(args.directory))
    elif args.action == 'compare':
        compare_data()
    elif args.action == 'permissions':
        file_permissions()
    elif args.action == 'start':
        compose('up','-d','redis')
        compose('up','-d','app')
    elif args.action == 'smoke':
        results=smoke(candidate_base() if args.base=='candidate' else args.base)
        if args.directory: write_json(Path(args.directory)/('smoke-'+args.base.rsplit(':',1)[-1]+'.json'),results)
        print(json.dumps(results,ensure_ascii=False))
    elif args.action == 'live-prices':
        live_prices(Path(args.directory))
    elif args.action == 'prepare':
        prepare(args.image, Path(args.directory))
    elif args.action == 'restore':
        restore(Path(args.directory))
    elif args.action == 'migrate':
        migrate()
    elif args.action == 'prices':
        price_import(Path(args.directory))
    elif args.action == 'layout':
        print(json.dumps(layout()))
    elif args.action == 'inventory':
        print(json.dumps(inventory(), ensure_ascii=False))
    else:
        if not args.directory:
            parser.error('--directory is required')
        directory = Path(args.directory).resolve()
        if directory.parent != ROOT / 'upgrades':
            raise ValueError('Snapshot must be an immediate child of /www/longdao/upgrades')
        snapshot(directory)


if __name__ == '__main__':
    main()

const assert = require('node:assert/strict')
const crypto = require('node:crypto')
const http = require('node:http')
const { execFileSync } = require('node:child_process')
const { chromium } = require('C:/Users/锐/.cache/codex-runtimes/codex-primary-runtime/dependencies/node/node_modules/playwright')

const base = 'http://127.0.0.1:28080'
const agent = 'LongdaoSEPValidation/1.0'
const hash = value => crypto.createHash('sha256').update(value).digest('hex')
const sql = query => execFileSync('docker', ['exec', 'longdao-model-pricing-test-20261120', 'psql', '-U', 'pricing_test', '-d', 'pricing_test', '-v', 'ON_ERROR_STOP=1', '-Atc', query], { encoding: 'utf8' }).trim()
const wait = ms => new Promise(resolve => setTimeout(resolve, ms))

async function main() {
  assert.ok(process.env.COMPUTE_TEST_PASSWORD_HASH, 'A fixture bcrypt password hash is required')
  let token = ''
  async function admin(method, resource, body, expected = 200) {
    const response = await fetch(base + '/api/v1' + resource, { method, headers: { 'Content-Type': 'application/json', 'User-Agent': agent, ...(token ? { Authorization: `Bearer ${token}` } : {}) }, body: body === undefined ? undefined : JSON.stringify(body) })
    const json = await response.json()
    assert.equal(response.status, expected, `${method} ${resource}: ${JSON.stringify(json)}`)
    return Object.hasOwn(json, 'data') ? json.data : json
  }
  const auth = await admin('POST', '/auth/login', { email: process.env.COMPUTE_TEST_ADMIN_EMAIL, password: process.env.COMPUTE_TEST_ADMIN_PASSWORD })
  token = auth.access_token
  const stamp = String(Date.now())
  const signing = crypto.generateKeyPairSync('ed25519')
  const encryption = crypto.generateKeyPairSync('rsa', { modulusLength: 3072 })
  const previous = await admin('GET', '/admin/sep/config')
  assert.equal(previous.enabled, false, 'Validation must not replace an enabled integration')
  let upstreamCalls = 0
  let mutateDuringCall
  const upstream = http.createServer(async (req, res) => {
    try {
      const chunks = []; for await (const chunk of req) chunks.push(chunk)
      const body = Buffer.concat(chunks).toString()
      if (req.url === '/v1/chat/completions') {
        upstreamCalls++
        if (mutateDuringCall) { const fn = mutateDuringCall; mutateDuringCall = null; await fn() }
        const request = JSON.parse(body)
        if (request.messages?.[0]?.content === 'ambiguous') { res.destroy(); return }
        if (request.messages?.[0]?.content === 'missing-usage') {
          res.writeHead(200, { 'Content-Type': 'application/json' })
          res.end(JSON.stringify({ id: `no-usage-${stamp}`, object: 'chat.completion', model: 'upstream-chat', choices: [{ index: 0, message: { role: 'assistant', content: 'No usage evidence' }, finish_reason: 'stop' }] }))
          return
        }
        if (request.stream) {
          res.writeHead(200, { 'Content-Type': 'text/event-stream' })
          res.write(`data: ${JSON.stringify({ id: 'chat-stream-test', object: 'chat.completion.chunk', model: 'upstream-chat', choices: [{ index: 0, delta: { role: 'assistant', content: 'test' }, finish_reason: null }] })}\n\n`)
          res.write(`data: ${JSON.stringify({ id: 'chat-stream-test', object: 'chat.completion.chunk', model: 'upstream-chat', choices: [], usage: { prompt_tokens: 1000, completion_tokens: 100, prompt_tokens_details: { cached_tokens: 400 } } })}\n\n`)
          res.end('data: [DONE]\n\n')
        } else {
          res.writeHead(200, { 'Content-Type': 'application/json' })
          res.end(JSON.stringify({ id: `chat-${stamp}-${upstreamCalls}`, object: 'chat.completion', created: 1, model: 'upstream-chat', choices: [{ index: 0, message: { role: 'assistant', content: 'SEP test' }, finish_reason: 'stop' }], usage: { prompt_tokens: 1000, completion_tokens: 100, total_tokens: 1100, prompt_tokens_details: { cached_tokens: 400 } } }))
        }
      } else if (req.url === '/v1/images/generations') {
        upstreamCalls++
        res.writeHead(200, { 'Content-Type': 'application/json' })
        res.end(JSON.stringify({ created: 1, data: [{ b64_json: 'iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAwMCAO+aH3cAAAAASUVORK5CYII=' }] }))
      } else if (req.url === '/v1/videos' && req.method === 'POST') {
        upstreamCalls++
        res.writeHead(200, { 'Content-Type': 'application/json' })
        res.end(JSON.stringify({ id: `video_${stamp}`, status: 'queued', seconds: '6', size: '1280x720' }))
      } else if (req.url === `/v1/videos/video_${stamp}`) {
        res.writeHead(200, { 'Content-Type': 'application/json' })
        res.end(JSON.stringify({ id: `video_${stamp}`, status: 'completed', seconds: '6', size: '1280x720' }))
      } else if (req.url === `/v1/videos/video_${stamp}/content`) {
        res.writeHead(200, { 'Content-Type': 'video/mp4' }); res.end(Buffer.from('local-video-test'))
      } else { res.writeHead(404); res.end(JSON.stringify({ error: { message: `Unexpected upstream path ${req.url}` } })) }
    } catch (error) { res.writeHead(500); res.end(String(error)) }
  })
  await new Promise(resolve => upstream.listen(0, '127.0.0.1', resolve))

  const config = { enabled: true, integration_id: previous.integration_id || 'sep-local-validation', signing_key_id: `sign-${stamp}`, signing_public_key: signing.publicKey.export({ type: 'spki', format: 'pem' }), encryption_key_id: `enc-${stamp}`, encryption_public_key: encryption.publicKey.export({ type: 'spki', format: 'pem' }) }
  function signed(method, resource, body, options = {}) {
    const bytes = body === undefined ? '' : typeof body === 'string' ? body : JSON.stringify(body)
    const headers = { 'Content-Type': 'application/json', 'User-Agent': agent, 'X-SEP-Version': '1', 'X-SEP-Key-ID': config.signing_key_id, 'X-SEP-Timestamp': String(Math.floor(Date.now() / 1000)), 'X-SEP-Nonce': crypto.randomUUID(), ...options }
    const canonical = ['sep-v1', headers['X-SEP-Key-ID'], headers['X-SEP-Timestamp'], headers['X-SEP-Nonce'], method, resource, hash(bytes), hash(headers.Authorization || ''), headers['X-SEP-Enterprise-ID'] || '', headers['X-SEP-Execution-ID'] || '', headers['Idempotency-Key'] || '', headers['Content-Type'] || '', headers['X-SEP-Quote-ID'] || '', headers['X-Video-Operation'] || ''].join('\n')
    headers['X-SEP-Signature'] = crypto.sign(null, Buffer.from(canonical), signing.privateKey).toString('base64url')
    return { method, headers, body: body === undefined ? undefined : bytes }
  }
  async function sep(method, resource, body, options, expected = 200) {
    const response = await fetch(base + resource, signed(method, resource, body, options))
    const text = await response.text()
    let json; try { json = JSON.parse(text) } catch { json = { text } }
    assert.equal(response.status, expected, `${method} ${resource}: ${text.slice(0, 1500)}`)
    return { json, response }
  }
  function decrypt(jwe) {
    const [protectedHeader, encryptedKey, iv, ciphertext, tag] = jwe.split('.')
    assert.equal(JSON.parse(Buffer.from(protectedHeader, 'base64url')).alg, 'RSA-OAEP-256')
    const key = crypto.privateDecrypt({ key: encryption.privateKey, oaepHash: 'sha256', padding: crypto.constants.RSA_PKCS1_OAEP_PADDING }, Buffer.from(encryptedKey, 'base64url'))
    const decipher = crypto.createDecipheriv('aes-256-gcm', key, Buffer.from(iv, 'base64url'))
    decipher.setAAD(Buffer.from(protectedHeader)); decipher.setAuthTag(Buffer.from(tag, 'base64url'))
    return JSON.parse(Buffer.concat([decipher.update(Buffer.from(ciphertext, 'base64url')), decipher.final()]).toString())
  }
  let enterpriseID
  let fixtureAccountID
  const groups = []
  try {
    for (const kind of ['chat', 'image', 'video']) groups.push(await admin('POST', '/admin/groups', { name: `sep-${kind}-${stamp}`, platform: 'openai', subscription_type: 'standard', rate_multiplier: 1.5, allow_image_generation: true }))
    Object.assign(config, { chat_group_id: groups[0].id, image_group_id: groups[1].id, video_group_id: groups[2].id })
    await admin('PUT', '/admin/sep/config', config)
    await admin('PUT', '/admin/settings', { platform_usd_to_cny_rate: '7.15' })
    const models = { chat: `sep-chat-${stamp}`, image: `gpt-image-2-${stamp}`, video: `minimax-h3-${stamp}` }
    const price = (model, mode, items) => ({ model, mode, discount: '1', markup: '1', items, policy: {} })
    const prices = [price(models.chat, 'token', [{ kind: 'input', tier: '', unit: 'USD/MTok', amount: '2' }, { kind: 'output', tier: '', unit: 'USD/MTok', amount: '8' }, { kind: 'cache_read', tier: '', unit: 'USD/MTok', amount: '0.2' }]), price(models.image, 'image', [{ kind: 'image', tier: '1K', unit: 'USD/image', amount: '0.04' }]), price(models.video, 'video', [{ kind: 'video', tier: '720p', unit: 'USD/second', amount: '0.02' }])]
    const company = await admin('POST', '/admin/upstream-companies', { name: `SEP upstream ${stamp}` })
    const account = await admin('POST', '/admin/accounts', { name: `SEP test upstream ${stamp}`, platform: 'openai', type: 'apikey', concurrency: 20, priority: 1, rate_multiplier: 1, group_ids: groups.map(g => g.id), credentials: { api_key: 'local-only-unused-key', base_url: `http://127.0.0.1:${upstream.address().port}`, model_mapping: { [models.chat]: 'upstream-chat', [models.image]: 'gpt-image-2', [models.video]: 'minimax-h3' }, openai_capabilities: ['chat_completions', 'videos'] }, extra: { openai_responses_mode: 'force_chat_completions' }, upstream_company_id: company.id, model_prices: prices })
    fixtureAccountID = account.id
    const listed = await admin('GET', `/admin/accounts/${account.id}/model-prices`)
    for (const p of listed.prices) await admin('PUT', '/admin/model-pricing/base-price', { model: p.model, source_price_id: p.id, source_revision: p.revision, expected_version: 0 })

    enterpriseID = `ent-${stamp}`
    const registration = { enterprise_id: enterpriseID, registration_ref: `reg-${stamp}`, company_name: `SEP 联调企业 ${stamp}`, admin_email: `sep-${stamp}@example.test`, encryption_key_id: config.encryption_key_id, password_hash: process.env.COMPUTE_TEST_PASSWORD_HASH }
    const registrationOptions = { 'Idempotency-Key': registration.registration_ref }
    const registrations = await Promise.all(Array.from({ length: 5 }, () => sep('POST', '/api/v1/sep/enterprises', registration, registrationOptions)))
    const enterprise = registrations[0].json
    for (const result of registrations) assert.equal(result.json.compute_account_id, enterprise.compute_account_id)
    assert.equal(enterprise.credentials.length, 3)
    assert.equal(sql(`SELECT COUNT(*) FROM sep_enterprises WHERE enterprise_id='${enterpriseID}'`), '1')
    const credentials = Object.fromEntries(enterprise.credentials.map(c => [c.type, decrypt(c.encrypted_key)]))
	const listedKeys=await admin('GET',`/admin/users/${enterprise.compute_account_id}/api-keys`)
	assert.ok(listedKeys.items.every(k=>!Object.values(credentials).some(c=>c.key===k.key)))
	const guardedBalance=await fetch(base+`/api/v1/admin/users/${enterprise.compute_account_id}/balance`,{method:'POST',headers:{Authorization:`Bearer ${token}`,'Content-Type':'application/json','User-Agent':agent},body:JSON.stringify({balance:100,operation:'set',notes:'expected rejection'})})
	assert.notEqual(guardedBalance.status,200)
	assert.equal(sql(`SELECT balance::text FROM users WHERE id=${enterprise.compute_account_id}`),'0.00000000')
    const metadata = (await sep('GET', `/api/v1/sep/enterprises/${enterpriseID}`)).json
    assert.ok(metadata.credentials.every(c => !c.encrypted_key && !c.key))
    await sep('POST', '/api/v1/sep/enterprises', { ...registration, company_name: 'different' }, registrationOptions, 409)
    const replay = signed('GET', `/api/v1/sep/enterprises/${enterpriseID}/balance`)
    assert.equal((await fetch(base + `/api/v1/sep/enterprises/${enterpriseID}/balance`, replay)).status, 200)
    assert.equal((await fetch(base + `/api/v1/sep/enterprises/${enterpriseID}/balance`, replay)).status, 401)
    const tampered = signed('GET', `/api/v1/sep/enterprises/${enterpriseID}/balance`); tampered.headers['X-SEP-Enterprise-ID'] = 'other'
    assert.equal((await fetch(base + `/api/v1/sep/enterprises/${enterpriseID}/balance`, tampered)).status, 401)
    console.log('Concurrent registration, encrypted credential interoperability, no plaintext query, replay rejection and signature tampering verified')

    const balance = async () => (await sep('GET', `/api/v1/sep/enterprises/${enterpriseID}/balance`)).json
    async function billed(id) { for (let attempt = 0; attempt < 60; attempt++) { const bill = (await sep('GET', `/api/v1/sep/executions/${id}/billing`)).json; if (bill.fee_status === 'final') return bill; await wait(100) } throw Error(`Fee not finalized for ${id}`) }
    const keyHeaders = (kind, id, more = {}) => ({ Authorization: `Bearer ${credentials[kind].key}`, 'X-SEP-Enterprise-ID': enterpriseID, ...(id ? { 'X-SEP-Execution-ID': id } : {}), ...more })
    for (const kind of ['chat', 'image', 'video']) {
      const directory = (await sep('GET', '/v1/models', undefined, keyHeaders(kind))).json
      assert.deepEqual(directory.data.map(model => model.id), [models[kind]], `Mixed group ${kind} directory`)
      assert.ok(!directory.partial)
    }
    const directoryGroup = await admin('POST', '/admin/groups', { name: `sep-empty-${stamp}`, platform: 'openai', subscription_type: 'standard', rate_multiplier: 1 })
    groups.push(directoryGroup)
    await admin('PUT', `/admin/sep/enterprises/${enterpriseID}`, { status: 'ready', groups: { chat: directoryGroup.id } })
    assert.deepEqual((await sep('GET', '/v1/models', undefined, keyHeaders('chat'))).json.data, [])
    await admin('PUT', `/admin/sep/enterprises/${enterpriseID}`, { status: 'ready', groups: { chat: groups[0].id } })
    const originalMapping = { [models.chat]: 'upstream-chat', [models.image]: 'gpt-image-2', [models.video]: 'minimax-h3' }
    await admin('PUT', `/admin/accounts/${account.id}`, { credentials: { ...account.credentials, model_mapping: { [models.image]: 'gpt-image-2', [models.video]: 'minimax-h3' } } })
    assert.deepEqual((await sep('GET', '/v1/models', undefined, keyHeaders('chat'))).json.data, [])
    await admin('PUT', `/admin/accounts/${account.id}`, { credentials: { ...account.credentials, model_mapping: originalMapping } })
    const errorResponse = await fetch(base + '/api/v1/sep/enterprises', signed('POST', '/api/v1/sep/enterprises', { ...registration, company_name: 'changed-again' }, registrationOptions))
    assert.equal(errorResponse.status, 409)
    const errorBody = await errorResponse.json()
    assert.equal(errorBody.error.code, 'IDEMPOTENCY_CONFLICT')
    assert.ok(errorBody.request_id)
    assert.equal(errorBody.request_id, errorResponse.headers.get('x-request-id'))
    console.log('Mixed-purpose model discovery, empty groups, immediate whitelist withdrawal and error trace IDs verified')
    const chatBody = { model: models.chat, messages: [{ role: 'user', content: 'test' }] }
    await sep('POST', '/v1/chat/completions', chatBody, keyHeaders('image', `wrong-${stamp}`), 403)
    assert.equal((await fetch(base + '/v1/chat/completions', { method: 'POST', headers: { Authorization: `Bearer ${credentials.chat.key}`, 'Content-Type': 'application/json' }, body: JSON.stringify(chatBody) })).status, 401)
    const fullFreeID = `free-${stamp}`
    await sep('POST', '/v1/chat/completions', chatBody, keyHeaders('chat', fullFreeID))
    const freeBill = await billed(fullFreeID)
    assert.equal(freeBill.gross_cny, '0.022308')
    assert.equal((await balance()).total_paid_cny, '0.000000')
    const decision = (bill, free, paid, id) => ({ contract_version: 1, enterprise_id: enterpriseID, settlement_id: id, bill_id: bill.bill_id, bill_version: bill.bill_version, currency: 'CNY', gross_cny: bill.gross_cny, free_cny: free, paid_cny: paid })
    const freeDecision = decision(freeBill, freeBill.gross_cny, '0.000000', `settle-free-${stamp}`)
    const freeReceipts = await Promise.all(Array.from({ length: 5 }, () => sep('POST', `/api/v1/sep/executions/${fullFreeID}/settlement`, freeDecision, { 'Idempotency-Key': freeDecision.settlement_id })))
    for (const r of freeReceipts) assert.equal(r.json.ledger_entry_id, freeReceipts[0].json.ledger_entry_id)
    assert.equal((await balance()).sequence, '1')
    assert.equal((await balance()).total_paid_cny, '0.000000')
    const count = upstreamCalls
    await sep('POST', '/v1/chat/completions', chatBody, keyHeaders('chat', fullFreeID), 409)
    await sep('POST', '/v1/chat/completions', { ...chatBody, temperature: 0.1 }, keyHeaders('chat', fullFreeID), 409)
    assert.equal(upstreamCalls, count)
    const uncertainID = `uncertain-${stamp}`
    const beforeUncertain = upstreamCalls
    const uncertainResponse = await fetch(base + '/v1/chat/completions', signed('POST', '/v1/chat/completions', { ...chatBody, messages: [{ role: 'user', content: 'ambiguous' }] }, keyHeaders('chat', uncertainID)))
    assert.ok(uncertainResponse.status >= 400); await uncertainResponse.text()
    assert.equal(upstreamCalls, beforeUncertain + 1)
    const uncertainBill = (await sep('GET', `/api/v1/sep/executions/${uncertainID}/billing`)).json
    assert.equal(uncertainBill.gross_cny, null); assert.notEqual(uncertainBill.fee_status, 'final')
    await sep('POST', '/v1/chat/completions', { ...chatBody, messages: [{ role: 'user', content: 'ambiguous' }] }, keyHeaders('chat', uncertainID), 409)
    assert.equal(upstreamCalls, beforeUncertain + 1)
    const missingUsageID = `missing-usage-${stamp}`
    await sep('POST', '/v1/chat/completions', { ...chatBody, messages: [{ role: 'user', content: 'missing-usage' }] }, keyHeaders('chat', missingUsageID))
    let missingUsageBill
    for (let attempt = 0; attempt < 30; attempt++) {
      missingUsageBill = (await sep('GET', `/api/v1/sep/executions/${missingUsageID}/billing`)).json
      if (missingUsageBill.fee_status === 'reconciling' || missingUsageBill.fee_status === 'final') break
      await wait(100)
    }
    assert.equal(missingUsageBill.gross_cny, null)
    assert.equal(missingUsageBill.fee_status, 'reconciling')
    const alternateKeyHeaders = keyHeaders('chat', `alternate-key-${stamp}`)
    alternateKeyHeaders['x-api-key'] = alternateKeyHeaders.Authorization.slice(7)
    delete alternateKeyHeaders.Authorization
    await sep('POST', '/v1/chat/completions', chatBody, alternateKeyHeaders, 401)
    assert.equal(upstreamCalls, beforeUncertain + 2)
    const settledBill = (await sep('GET', `/api/v1/sep/executions/${fullFreeID}/billing`)).json
    assert.equal(settledBill.settlement_receipt.ledger_entry_id, freeReceipts[0].json.ledger_entry_id)
    console.log('Zero-balance execution, cache-aware fee, all-free settlement, concurrent settlement deduplication and execution deduplication verified')

    const transfer = { contract_version: 1, enterprise_id: enterpriseID, transfer_id: `credit-${stamp}`, direction: 'credit_compute', currency: 'CNY', amount_cny: '1.000000', funding_ref: `funding-${stamp}` }
    const credits = await Promise.all(Array.from({ length: 8 }, () => sep('POST', '/api/v1/sep/transfers', transfer, { 'Idempotency-Key': transfer.transfer_id })))
    assert.equal(new Set(credits.map(r => r.json.ledger_entry_id)).size, 1)
    assert.equal((await balance()).total_paid_cny, '1.000000')
    assert.equal((await sep('GET', `/api/v1/sep/transfers/${transfer.transfer_id}`)).json.ledger_entry_id, credits[0].json.ledger_entry_id)
    await sep('POST', '/api/v1/sep/transfers', { ...transfer, amount_cny: '2.000000' }, { 'Idempotency-Key': transfer.transfer_id }, 409)
    const mixedID = `mixed-${stamp}`
    mutateDuringCall = async () => { await admin('PUT', '/admin/settings', { platform_usd_to_cny_rate: '8' }); await admin('PUT', `/admin/groups/${groups[0].id}`, { rate_multiplier: 3 }) }
    await sep('POST', '/v1/chat/completions', chatBody, keyHeaders('chat', mixedID))
    const mixed = await billed(mixedID)
    assert.equal(mixed.gross_cny, '0.022308')
    const mixedDecision = decision(mixed, '0.010000', '0.012308', `settle-mixed-${stamp}`)
    await sep('POST', `/api/v1/sep/executions/${mixedID}/settlement`, mixedDecision, { 'Idempotency-Key': mixedDecision.settlement_id })
    assert.equal((await balance()).available_paid_cny, '0.987692')
    assert.equal(sql(`SELECT balance::text FROM users WHERE id=${enterprise.compute_account_id}`), '0.00000000')
    const duplicateDecision = { ...mixedDecision, settlement_id: `second-${stamp}` }
    await sep('POST', `/api/v1/sep/executions/${mixedID}/settlement`, duplicateDecision, { 'Idempotency-Key': duplicateDecision.settlement_id }, 409)
    const debits = await Promise.all(['a', 'b'].map(suffix => { const r = { ...transfer, transfer_id: `debit-${suffix}-${stamp}`, direction: 'debit_compute', amount_cny: '0.800000' }; return sep('POST', '/api/v1/sep/transfers', r, { 'Idempotency-Key': r.transfer_id }) }))
    assert.equal(debits.filter(r => r.json.status === 'committed').length, 1)
    assert.equal(debits.filter(r => r.json.status === 'rejected').length, 1)
    assert.equal((await balance()).available_paid_cny, '0.187692')
    const oldWatermark = (await sep('GET', `/api/v1/sep/enterprises/${enterpriseID}/ledger?limit=1`)).json
    assert.equal(oldWatermark.items.length, 1)
    const extraCredit = { ...transfer, transfer_id: `extra-${stamp}`, amount_cny: '1.000000' }
    await sep('POST', '/api/v1/sep/transfers', extraCredit, { 'Idempotency-Key': extraCredit.transfer_id })
    const nextPage = (await sep('GET', `/api/v1/sep/enterprises/${enterpriseID}/ledger?after_sequence=${oldWatermark.after_sequence}&snapshot_sequence=${oldWatermark.snapshot_sequence}&limit=200`)).json
    assert.equal(nextPage.total_paid_cny, '0.187692')
    assert.equal((await balance()).available_paid_cny, '1.187692')
    const rejectedTransfer = debits.find(r => r.json.status === 'rejected').json
    const rejectedReplay = (await sep('POST', '/api/v1/sep/transfers', { ...transfer, transfer_id: rejectedTransfer.transfer_id, direction: 'debit_compute', amount_cny: '0.800000' }, { 'Idempotency-Key': rejectedTransfer.transfer_id })).json
    assert.equal(rejectedReplay.status, 'rejected')
    assert.equal((await balance()).available_paid_cny, '1.187692')
    console.log('Mixed settlement, frozen in-flight rate/multiplier, no USD deduction, concurrent refund guard and fixed-watermark ledger verified')

    await admin('PUT', '/admin/settings', { platform_usd_to_cny_rate: '7.15' })
    const streamID = `stream-${stamp}`
    const stream = await sep('POST', '/v1/chat/completions', { ...chatBody, stream: true }, keyHeaders('chat', streamID))
    assert.ok(stream.json.text.includes('[DONE]'))
    assert.equal((await billed(streamID)).gross_cny, '0.044616')
    const imageQuote = (await sep('POST', '/v1/sub2api/images/pricing', { model: models.image, operation: 'generate', size: '1024x1024', quality: 'medium', n: 1 }, keyHeaders('image'))).json
	const beforeValidation=upstreamCalls
	const platformQuote=(await sep('POST','/api/v1/sep/validation/images/pricing',{model:models.image,operation:'generate',size:'1024x1024',quality:'medium',n:1})).json
	assert.equal(platformQuote.quote_scope,'platform_validation');assert.equal(platformQuote.currency,'CNY');assert.equal(platformQuote.pricing_version,'');assert.equal(upstreamCalls,beforeValidation)
    assert.equal(imageQuote.currency, 'CNY'); assert.equal(imageQuote.amount_cny, '0.429000'); assert.ok(!Object.hasOwn(imageQuote, 'amount_usd'))
    const imageID = `image-${stamp}`
	await sep('POST','/v1/images/generations',{model:models.image,prompt:'test',size:'1024x1024',quality:'medium',n:2},keyHeaders('image',`wrong-spec-${stamp}`,{'X-SEP-Quote-ID':imageQuote.pricing_version}),409)
    await sep('POST', '/v1/images/generations', { model: models.image, prompt: 'test', size: '1024x1024', quality: 'medium', n: 1, response_format: 'b64_json' }, keyHeaders('image', imageID, { 'X-SEP-Quote-ID': imageQuote.pricing_version }))
    const imageBill = await billed(imageID)
    assert.equal(imageBill.gross_cny, '0.429000')
    const videoQuote = (await sep('POST', '/v1/sub2api/videos/pricing', { model: models.video, operation: 'text-to-video', resolution: '720p', duration_seconds: 6 }, keyHeaders('video'))).json
    assert.equal(videoQuote.amount_cny, '1.287000')
    const videoID = `video-${stamp}`
    await sep('POST', '/v1/videos', { model: models.video, prompt: 'test', seconds: '6', size: '1280x720' }, keyHeaders('video', videoID, { 'X-SEP-Quote-ID': videoQuote.pricing_version, 'X-Video-Operation': 'text-to-video' }))
    assert.notEqual(sql(`SELECT fee_status FROM sep_executions WHERE execution_id='${videoID}'`), 'final')
    const videoBill = await billed(videoID)
    assert.equal(videoBill.gross_cny, '1.287000')
	await admin('PUT',`/admin/sep/enterprises/${enterpriseID}`,{status:'disabled',groups:{video:groups[1].id}})
	const content=await fetch(base+`/v1/videos/video_${stamp}/content`,signed('GET',`/v1/videos/video_${stamp}/content`,undefined,keyHeaders('video')))
	assert.equal(content.status,200);assert.equal(await content.text(),'local-video-test')
	await sep('POST','/v1/chat/completions',chatBody,keyHeaders('chat',`disabled-${stamp}`),403)
	await admin('PUT',`/admin/sep/enterprises/${enterpriseID}`,{status:'ready',groups:{video:groups[2].id}})
    const insufficientDecision = decision(videoBill, '0.000000', videoBill.gross_cny, `video-insufficient-${stamp}`)
    await sep('POST', `/api/v1/sep/executions/${videoID}/settlement`, insufficientDecision, { 'Idempotency-Key': insufficientDecision.settlement_id }, 409)
    assert.equal((await balance()).available_paid_cny, '1.187692')
    const videoDecision = decision(videoBill, videoBill.gross_cny, '0.000000', `video-free-${stamp}`)
    await sep('POST', `/api/v1/sep/executions/${videoID}/settlement`, videoDecision, { 'Idempotency-Key': videoDecision.settlement_id })
    console.log('Enterprise-specific CNY image/video quotes and async video finalization verified')

    const browser = await chromium.launch({ executablePath: 'C:/Program Files (x86)/Microsoft/Edge/Application/msedge.exe', headless: true })
    try {
      const context = await browser.newContext({ viewport: { width: 1440, height: 1100 }, userAgent: agent })
      await context.addInitScript(({ auth }) => { if (window !== top) return; localStorage.setItem('auth_token', auth.access_token); localStorage.setItem('auth_user', JSON.stringify(auth.user)); localStorage.setItem('token_expires_at', String(Date.now() + 3600000)); localStorage.setItem('locale', 'zh-CN'); localStorage.setItem(`onboarding_tour_${auth.user.id}_${auth.user.role}_v4_interactive`, 'true'); localStorage.setItem(`admin_guide_${auth.user.id}_${auth.user.role}_v4_interactive`, 'true') }, { auth })
      const page = await context.newPage(); const errors = []; page.on('pageerror', e => errors.push(e.message))
      await page.goto('http://127.0.0.1:3301/admin/sep')
      await page.getByRole('main').getByRole('heading', { name: 'SEP 企业对接', exact: true }).waitFor()
      await page.getByLabel('对话 Key', { exact: true }).waitFor()
      await page.screenshot({ path: 'outputs/sep-config-desktop.png', fullPage: true, animations: 'disabled' })
      await page.getByRole('tab', { name: '企业账户' }).click()
      await page.getByRole('searchbox', { name: '搜索企业' }).fill(enterpriseID)
      await page.getByRole('button', { name: '查看', exact: true }).click()
      await page.getByRole('dialog').getByText('可用人民币', { exact: true }).waitFor()
      await page.waitForFunction(() => Array.from(document.querySelectorAll('[role="dialog"]')).every(el => getComputedStyle(el).opacity === '1'))
      await page.screenshot({ path: 'outputs/sep-enterprise-desktop.png', fullPage: true, animations: 'disabled' })
      await page.getByRole('dialog').getByRole('button', { name: 'Close modal' }).click()
      await page.getByRole('tab', { name: '对接配置' }).click()
      await page.setViewportSize({ width: 390, height: 844 })
      await page.waitForFunction(() => document.querySelector('main').getBoundingClientRect().left < 30 && document.querySelector('aside').getBoundingClientRect().right < 1)
      await page.screenshot({ path: 'outputs/sep-config-mobile.png', fullPage: true, animations: 'disabled' })
      assert.ok(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth + 1), 'Mobile overflow')
      await page.getByRole('tab', { name: '企业账户' }).click()
      await page.getByRole('button', { name: '查看', exact: true }).click()
      await page.getByRole('dialog').getByText('可用人民币', { exact: true }).waitFor()
      await page.screenshot({ path: 'outputs/sep-enterprise-mobile.png', fullPage: true, animations: 'disabled' })
      assert.ok(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth + 1), 'Enterprise mobile overflow')
      assert.deepEqual(errors, [])
      console.log('Admin configuration, enterprise detail, ledger and desktop/mobile layout verified')
    } finally { await browser.close() }
    console.log(JSON.stringify({ verified: true, enterprise_id: enterpriseID, upstream_calls: upstreamCalls }))
  } finally {
    if (enterpriseID) { try { await admin('PUT', `/admin/sep/enterprises/${enterpriseID}`, { status: 'disabled', groups: {} }) } catch {} }
    if (fixtureAccountID) { try { await admin('PUT', `/admin/accounts/${fixtureAccountID}`, { schedulable: false }) } catch {} }
    for (const group of groups) { try { await admin('PUT', `/admin/groups/${group.id}`, { status: 'disabled' }) } catch {} }
    await admin('PUT', '/admin/settings', { platform_usd_to_cny_rate: '7.15' })
    await admin('PUT', '/admin/sep/config', { ...previous, enabled: false })
    await new Promise(resolve => upstream.close(resolve))
  }
}
main().catch(error => { console.error(error); process.exitCode = 1 })
